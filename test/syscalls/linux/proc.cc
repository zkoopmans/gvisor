// Copyright 2018 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

#include <elf.h>
#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <linux/magic.h>
#include <sched.h>
#include <signal.h>
#include <stddef.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/prctl.h>
#include <sys/ptrace.h>
#include <sys/stat.h>
#include <sys/statfs.h>
#include <sys/utsname.h>
#include <syscall.h>
#include <unistd.h>

#include <algorithm>
#include <atomic>
#include <functional>
#include <iostream>
#include <iterator>
#include <map>
#include <memory>
#include <ostream>
#include <regex>
#include <string>
#include <unordered_set>
#include <utility>
#include <vector>

#include "gmock/gmock.h"
#include "gtest/gtest.h"
#include "absl/algorithm/container.h"
#include "absl/container/btree_map.h"
#include "absl/container/flat_hash_set.h"
#include "absl/container/node_hash_set.h"
#include "absl/flags/flag.h"
#include "absl/strings/ascii.h"
#include "absl/strings/match.h"
#include "absl/strings/numbers.h"
#include "absl/strings/str_cat.h"
#include "absl/strings/str_format.h"
#include "absl/strings/str_join.h"
#include "absl/strings/str_split.h"
#include "absl/strings/string_view.h"
#include "absl/synchronization/mutex.h"
#include "absl/synchronization/notification.h"
#include "absl/time/clock.h"
#include "absl/time/time.h"
#include "test/util/capability_util.h"
#include "test/util/cleanup.h"
#include "test/util/eventfd_util.h"
#include "test/util/file_descriptor.h"
#include "test/util/fs_util.h"
#include "test/util/memory_util.h"
#include "test/util/mount_util.h"
#include "test/util/multiprocess_util.h"
#include "test/util/posix_error.h"
#include "test/util/proc_util.h"
#include "test/util/temp_path.h"
#include "test/util/test_util.h"
#include "test/util/thread_util.h"
#include "test/util/time_util.h"
#include "test/util/timer_util.h"

// NOTE(magi): No, this isn't really a syscall but this is a really simple
// way to get it tested on both gVisor, PTrace and Linux.

using ::testing::AllOf;
using ::testing::AnyOf;
using ::testing::ContainerEq;
using ::testing::Contains;
using ::testing::ContainsRegex;
using ::testing::Eq;
using ::testing::Gt;
using ::testing::HasSubstr;
using ::testing::IsSupersetOf;
using ::testing::Pair;
using ::testing::StartsWith;
using ::testing::UnorderedElementsAre;
using ::testing::UnorderedElementsAreArray;

// Exported by glibc.
extern char** environ;

ABSL_FLAG(bool, proc_pid_reuse_child, false,
          "If true, run the Proc_PidReuse child workload.");

namespace gvisor {
namespace testing {
namespace {

#ifndef SUID_DUMP_DISABLE
#define SUID_DUMP_DISABLE 0
#endif /* SUID_DUMP_DISABLE */
#ifndef SUID_DUMP_USER
#define SUID_DUMP_USER 1
#endif /* SUID_DUMP_USER */
#ifndef SUID_DUMP_ROOT
#define SUID_DUMP_ROOT 2
#endif /* SUID_DUMP_ROOT */

#if defined(__x86_64__) || defined(__i386__)
// This list of "required" fields consists of the set of fields that are printed
// unconditionally by the kernel (see arch/x86/kernel/cpu/proc.c) and a set of
// fields that are printed conditionally, but are present on common
// configurations (e.g. when CONFIG_SMP is set).
static const char* required_fields[] = {
    "processor",     "vendor_id",        "cpu family",
    "model\t\t:",    "model name",       "stepping",
    "cpu MHz",       "cache size",       "physical id",
    "siblings",      "core id",          "cpu cores",
    "apicid\t\t:",   "initial apicid",   "fpu\t\t:",
    "fpu_exception", "cpuid level",      "wp",
    "bogomips",      "clflush size",     "cache_alignment",
    "address sizes", "power management",
};
#elif defined(__aarch64__)
// This list of "required" fields is taken from reading the file
// arch/arm64/kernel/cpuinfo.c and seeing which fields will be unconditionally
// printed by the kernel.
static const char* required_fields[] = {
    "processor",        "BogoMIPS",    "Features", "CPU implementer",
    "CPU architecture", "CPU variant", "CPU part", "CPU revision",
};
#elif defined(__riscv)
// This list of "required" fields is taken from reading the file
// arch/riscv/kernel/cpu.c and seeing which fields will be unconditionally
// printed by the kernel.
static const char* required_fields[] = {
    "processor",
    "hart",
};
#else
#error "Unknown architecture"
#endif

// Takes the subprocess command line and pid.
// If it returns !OK, WithSubprocess returns immediately.
using SubprocessCallback = std::function<PosixError(int)>;

std::vector<std::string> saved_argv;  // NOLINT

// Helper function to dump /proc/{pid}/status and check the
// state data. State should = "Z" for zombied or "RSD" for
// running, interruptible sleeping (S), or uninterruptible sleep
// (D).
void CompareProcessState(absl::string_view state, int pid) {
  auto status_file = ASSERT_NO_ERRNO_AND_VALUE(
      GetContents(absl::StrCat("/proc/", pid, "/status")));
  // N.B. POSIX extended regexes don't support shorthand character classes (\w)
  // inside of brackets.
  EXPECT_THAT(status_file,
              ContainsRegex(absl::StrCat("State:.[", state,
                                         R"EOL(]\s+\([a-zA-Z ]+\))EOL")));
}

// Run callbacks while a subprocess is running, zombied, and/or exited.
PosixError WithSubprocess(SubprocessCallback const& running,
                          SubprocessCallback const& zombied,
                          SubprocessCallback const& exited) {
  int pipe_fds[2] = {};
  if (pipe(pipe_fds) < 0) {
    return PosixError(errno, "pipe");
  }

  int child_pid = fork();
  if (child_pid < 0) {
    return PosixError(errno, "fork");
  }

  if (child_pid == 0) {
    close(pipe_fds[0]);    // Close the read end.
    const DisableSave ds;  // Timing issues.

    // Write to the pipe to tell it we're ready.
    char buf = 'a';
    int res = 0;
    res = WriteFd(pipe_fds[1], &buf, sizeof(buf));
    TEST_CHECK_MSG(res == sizeof(buf), "Write failure in subprocess");

    while (true) {
      SleepSafe(absl::Milliseconds(100));
    }
  }

  close(pipe_fds[1]);  // Close the write end.

  int status = 0;
  auto wait_cleanup = Cleanup([child_pid, &status] {
    EXPECT_THAT(waitpid(child_pid, &status, 0), SyscallSucceeds());
  });
  auto kill_cleanup = Cleanup([child_pid] {
    EXPECT_THAT(kill(child_pid, SIGKILL), SyscallSucceeds());
  });

  // Wait for the child.
  char buf = 0;
  int res = ReadFd(pipe_fds[0], &buf, sizeof(buf));
  if (res < 0) {
    return PosixError(errno, "Read from pipe");
  } else if (res == 0) {
    return PosixError(EPIPE, "Unable to read from pipe: EOF");
  }

  if (running) {
    // The first arg, RSD, refers to a "running process", or a process with a
    // state of Running (R), Interruptible Sleep (S) or Uninterruptible
    // Sleep (D).
    CompareProcessState("RSD", child_pid);
    RETURN_IF_ERRNO(running(child_pid));
  }

  // Kill the process.
  kill_cleanup.Release()();
  siginfo_t info;
  // Wait until the child process has exited (WEXITED flag) but don't
  // reap the child (WNOWAIT flag).
  EXPECT_THAT(waitid(P_PID, child_pid, &info, WNOWAIT | WEXITED),
              SyscallSucceeds());

  if (zombied) {
    // Arg of "Z" refers to a Zombied Process.
    CompareProcessState("Z", child_pid);
    RETURN_IF_ERRNO(zombied(child_pid));
  }

  // Wait on the process.
  wait_cleanup.Release()();
  // If the process is reaped, then then this should return
  // with ECHILD.
  EXPECT_THAT(waitpid(child_pid, &status, WNOHANG),
              SyscallFailsWithErrno(ECHILD));

  if (exited) {
    RETURN_IF_ERRNO(exited(child_pid));
  }

  return NoError();
}

// Access the file returned by name when a subprocess is running.
PosixError AccessWhileRunning(std::function<std::string(int pid)> name,
                              int flags, std::function<void(int fd)> access) {
  FileDescriptor fd;
  return WithSubprocess(
      [&](int pid) -> PosixError {
        // Running.
        ASSIGN_OR_RETURN_ERRNO(fd, Open(name(pid), flags));

        access(fd.get());
        return NoError();
      },
      nullptr, nullptr);
}

// Access the file returned by name when the a subprocess is zombied.
PosixError AccessWhileZombied(std::function<std::string(int pid)> name,
                              int flags, std::function<void(int fd)> access) {
  FileDescriptor fd;
  return WithSubprocess(
      [&](int pid) -> PosixError {
        // Running.
        ASSIGN_OR_RETURN_ERRNO(fd, Open(name(pid), flags));
        return NoError();
      },
      [&](int pid) -> PosixError {
        // Zombied.
        access(fd.get());
        return NoError();
      },
      nullptr);
}

// Access the file returned by name when the a subprocess is exited.
PosixError AccessWhileExited(std::function<std::string(int pid)> name,
                             int flags, std::function<void(int fd)> access) {
  FileDescriptor fd;
  return WithSubprocess(
      [&](int pid) -> PosixError {
        // Running.
        ASSIGN_OR_RETURN_ERRNO(fd, Open(name(pid), flags));
        return NoError();
      },
      nullptr,
      [&](int pid) -> PosixError {
        // Exited.
        access(fd.get());
        return NoError();
      });
}

// ReadFd(fd=/proc/PID/basename) while PID is running.
int ReadWhileRunning(std::string const& basename, void* buf, size_t count) {
  int ret = 0;
  int err = 0;
  EXPECT_NO_ERRNO(AccessWhileRunning(
      [&](int pid) -> std::string {
        return absl::StrCat("/proc/", pid, "/", basename);
      },
      O_RDONLY,
      [&](int fd) {
        ret = ReadFd(fd, buf, count);
        err = errno;
      }));
  errno = err;
  return ret;
}

// ReadFd(fd=/proc/PID/basename) while PID is zombied.
int ReadWhileZombied(std::string const& basename, void* buf, size_t count) {
  int ret = 0;
  int err = 0;
  EXPECT_NO_ERRNO(AccessWhileZombied(
      [&](int pid) -> std::string {
        return absl::StrCat("/proc/", pid, "/", basename);
      },
      O_RDONLY,
      [&](int fd) {
        ret = ReadFd(fd, buf, count);
        err = errno;
      }));
  errno = err;
  return ret;
}

// ReadFd(fd=/proc/PID/basename) while PID is exited.
int ReadWhileExited(std::string const& basename, void* buf, size_t count) {
  int ret = 0;
  int err = 0;
  EXPECT_NO_ERRNO(AccessWhileExited(
      [&](int pid) -> std::string {
        return absl::StrCat("/proc/", pid, "/", basename);
      },
      O_RDONLY,
      [&](int fd) {
        ret = ReadFd(fd, buf, count);
        err = errno;
      }));
  errno = err;
  return ret;
}

// readlinkat(fd=/proc/PID/, basename) while PID is running.
int ReadlinkWhileRunning(std::string const& basename, char* buf, size_t count) {
  int ret = 0;
  int err = 0;
  EXPECT_NO_ERRNO(AccessWhileRunning(
      [&](int pid) -> std::string { return absl::StrCat("/proc/", pid, "/"); },
      O_DIRECTORY,
      [&](int fd) {
        ret = readlinkat(fd, basename.c_str(), buf, count);
        err = errno;
      }));
  errno = err;
  return ret;
}

// readlinkat(fd=/proc/PID/, basename) while PID is zombied.
int ReadlinkWhileZombied(std::string const& basename, char* buf, size_t count) {
  int ret = 0;
  int err = 0;
  EXPECT_NO_ERRNO(AccessWhileZombied(
      [&](int pid) -> std::string { return absl::StrCat("/proc/", pid, "/"); },
      O_DIRECTORY,
      [&](int fd) {
        ret = readlinkat(fd, basename.c_str(), buf, count);
        err = errno;
      }));
  errno = err;
  return ret;
}

// readlinkat(fd=/proc/PID/, basename) while PID is exited.
int ReadlinkWhileExited(std::string const& basename, char* buf, size_t count) {
  int ret = 0;
  int err = 0;
  EXPECT_NO_ERRNO(AccessWhileExited(
      [&](int pid) -> std::string { return absl::StrCat("/proc/", pid, "/"); },
      O_DIRECTORY,
      [&](int fd) {
        ret = readlinkat(fd, basename.c_str(), buf, count);
        err = errno;
      }));
  errno = err;
  return ret;
}

void RemoveUnstableCPUInfoFields(std::vector<std::string>& cpu_info_fields) {
  const std::vector<std::string> unstable_fields{"cpu MHz", "bogomips"};
  auto it = cpu_info_fields.begin();
  while (it != cpu_info_fields.end()) {
    bool found = false;
    for (const std::string& unstable_field : unstable_fields) {
      if (it->find(unstable_field) != std::string::npos) {
        found = true;
        break;
      }
    }
    if (found) {
      it = cpu_info_fields.erase(it);
    } else {
      ++it;
    }
  }
}

TEST(ProcTest, RootInodeNumber) {
  struct stat s;
  ASSERT_THAT(stat("/proc", &s), SyscallSucceeds());
  EXPECT_EQ(s.st_ino, 1);
}

TEST(ProcTest, NotFoundInRoot) {
  struct stat s;
  EXPECT_THAT(stat("/proc/foobar", &s), SyscallFailsWithErrno(ENOENT));
}

TEST(ProcSelfTest, IsThreadGroupLeader) {
  ScopedThread([] {
    const pid_t tgid = getpid();
    const pid_t tid = syscall(SYS_gettid);
    EXPECT_NE(tgid, tid);
    auto link = ASSERT_NO_ERRNO_AND_VALUE(ReadLink("/proc/self"));
    EXPECT_EQ(link, absl::StrCat(tgid));
  });
}

TEST(ProcThreadSelfTest, Basic) {
  const pid_t tgid = getpid();
  const pid_t tid = syscall(SYS_gettid);
  EXPECT_EQ(tgid, tid);
  auto link_threadself =
      ASSERT_NO_ERRNO_AND_VALUE(ReadLink("/proc/thread-self"));
  EXPECT_EQ(link_threadself, absl::StrCat(tgid, "/task/", tid));
  // Just read one file inside thread-self to ensure that the link is valid.
  auto link_threadself_exe =
      ASSERT_NO_ERRNO_AND_VALUE(ReadLink("/proc/thread-self/exe"));
  auto link_procself_exe =
      ASSERT_NO_ERRNO_AND_VALUE(ReadLink("/proc/self/exe"));
  EXPECT_EQ(link_threadself_exe, link_procself_exe);
}

TEST(ProcThreadSelfTest, Thread) {
  ScopedThread([] {
    const pid_t tgid = getpid();
    const pid_t tid = syscall(SYS_gettid);
    EXPECT_NE(tgid, tid);
    auto link_threadself =
        ASSERT_NO_ERRNO_AND_VALUE(ReadLink("/proc/thread-self"));

    EXPECT_EQ(link_threadself, absl::StrCat(tgid, "/task/", tid));
    // Just read one file inside thread-self to ensure that the link is valid.
    auto link_threadself_exe =
        ASSERT_NO_ERRNO_AND_VALUE(ReadLink("/proc/thread-self/exe"));
    auto link_procself_exe =
        ASSERT_NO_ERRNO_AND_VALUE(ReadLink("/proc/self/exe"));
    EXPECT_EQ(link_threadself_exe, link_procself_exe);
    // A thread should not have "/proc/<tid>/task".
    struct stat s;
    EXPECT_THAT(stat("/proc/thread-self/task", &s),
                SyscallFailsWithErrno(ENOENT));
  });
}

// Returns the /proc/PID/maps entry for the MAP_PRIVATE | MAP_ANONYMOUS mapping
// m with start address addr and length len.
std::string AnonymousMapsEntry(uintptr_t addr, size_t len, int prot) {
  return absl::StrCat(absl::Hex(addr, absl::PadSpec::kZeroPad8), "-",
                      absl::Hex(addr + len, absl::PadSpec::kZeroPad8), " ",
                      prot & PROT_READ ? "r" : "-",
                      prot & PROT_WRITE ? "w" : "-",
                      prot & PROT_EXEC ? "x" : "-", "p 00000000 00:00 0 ");
}

std::string AnonymousMapsEntryForMapping(const Mapping& m, int prot) {
  return AnonymousMapsEntry(m.addr(), m.len(), prot);
}

PosixErrorOr<std::map<uint64_t, uint64_t>> ReadProcSelfAuxv() {
  std::string auxv_file;
  RETURN_IF_ERRNO(GetContents("/proc/self/auxv", &auxv_file));
  const Elf64_auxv_t* auxv_data =
      reinterpret_cast<const Elf64_auxv_t*>(auxv_file.data());
  std::map<uint64_t, uint64_t> auxv_entries;
  for (int i = 0; auxv_data[i].a_type != AT_NULL; i++) {
    auto a_type = auxv_data[i].a_type;
    EXPECT_EQ(0, auxv_entries.count(a_type)) << "a_type: " << a_type;
    auxv_entries.emplace(a_type, auxv_data[i].a_un.a_val);
  }
  return auxv_entries;
}

TEST(ProcSelfAuxv, EntryPresence) {
  auto auxv_entries = ASSERT_NO_ERRNO_AND_VALUE(ReadProcSelfAuxv());

  EXPECT_EQ(auxv_entries.count(AT_ENTRY), 1);
  EXPECT_EQ(auxv_entries.count(AT_PHDR), 1);
  EXPECT_EQ(auxv_entries.count(AT_PHENT), 1);
  EXPECT_EQ(auxv_entries.count(AT_PHNUM), 1);
  EXPECT_EQ(auxv_entries.count(AT_BASE), 1);
  EXPECT_EQ(auxv_entries.count(AT_UID), 1);
  EXPECT_EQ(auxv_entries.count(AT_EUID), 1);
  EXPECT_EQ(auxv_entries.count(AT_GID), 1);
  EXPECT_EQ(auxv_entries.count(AT_EGID), 1);
  EXPECT_EQ(auxv_entries.count(AT_SECURE), 1);
  EXPECT_EQ(auxv_entries.count(AT_CLKTCK), 1);
  EXPECT_EQ(auxv_entries.count(AT_RANDOM), 1);
  EXPECT_EQ(auxv_entries.count(AT_EXECFN), 1);
  EXPECT_EQ(auxv_entries.count(AT_PAGESZ), 1);
  EXPECT_EQ(auxv_entries.count(AT_SYSINFO_EHDR), 1);
}

TEST(ProcSelfAuxv, EntryValues) {
  auto proc_auxv = ASSERT_NO_ERRNO_AND_VALUE(ReadProcSelfAuxv());

  // We need to find the ELF auxiliary vector. The section of memory pointed to
  // by envp contains some pointers to non-null pointers, followed by a single
  // pointer to a null pointer, followed by the auxiliary vector.
  char** envpi = environ;
  while (*envpi) {
    ++envpi;
  }

  const Elf64_auxv_t* envp_auxv =
      reinterpret_cast<const Elf64_auxv_t*>(envpi + 1);
  int i;
  for (i = 0; envp_auxv[i].a_type != AT_NULL; i++) {
    auto a_type = envp_auxv[i].a_type;
    EXPECT_EQ(proc_auxv.count(a_type), 1);
    EXPECT_EQ(proc_auxv[a_type], envp_auxv[i].a_un.a_val)
        << "a_type: " << a_type;
  }
  EXPECT_EQ(i, proc_auxv.size());
}

// Just open and read a part of /proc/self/mem, check that we can read an item.
TEST(ProcPidMem, Read) {
  auto memfd = ASSERT_NO_ERRNO_AND_VALUE(Open("/proc/self/mem", O_RDONLY));
  char input[] = "hello-world";
  char output[sizeof(input)];
  ASSERT_THAT(pread(memfd.get(), output, sizeof(output),
                    reinterpret_cast<off_t>(input)),
              SyscallSucceedsWithValue(sizeof(input)));
  ASSERT_STREQ(input, output);
}

TEST(ProcPidMem, PWrite) {
  auto memfd = ASSERT_NO_ERRNO_AND_VALUE(Open("/proc/self/mem", O_RDWR));
  char input[] = "hello-world";
  char output[sizeof(input)];
  ASSERT_THAT(pwrite(memfd.get(), input, sizeof(input),
                     reinterpret_cast<off_t>(output)),
              SyscallSucceedsWithValue(sizeof(input)));
  ASSERT_STREQ(input, output);
}

TEST(ProcPidMem, Write) {
  auto memfd = ASSERT_NO_ERRNO_AND_VALUE(Open("/proc/self/mem", O_RDWR));
  char input[] = "hello-world";
  char output[sizeof(input)];
  ASSERT_THAT(lseek(memfd.get(), reinterpret_cast<off_t>(output), SEEK_SET),
              SyscallSucceedsWithValue(reinterpret_cast<off_t>(output)));
  ASSERT_THAT(write(memfd.get(), input, sizeof(input)),
              SyscallSucceedsWithValue(sizeof(input)));
  ASSERT_STREQ(input, output);
}

// Perform read on an unmapped region.
TEST(ProcPidMem, Unmapped) {
  // Strategy: map then unmap, so we have a guaranteed unmapped region
  auto memfd = ASSERT_NO_ERRNO_AND_VALUE(Open("/proc/self/mem", O_RDONLY));
  Mapping mapping = ASSERT_NO_ERRNO_AND_VALUE(
      MmapAnon(kPageSize, PROT_READ | PROT_WRITE, MAP_PRIVATE));
  // Fill it with things
  memset(mapping.ptr(), 'x', mapping.len());
  char expected = 'x', output;
  ASSERT_THAT(pread(memfd.get(), &output, sizeof(output),
                    reinterpret_cast<off_t>(mapping.ptr())),
              SyscallSucceedsWithValue(sizeof(output)));
  ASSERT_EQ(expected, output);

  const auto rest = [&] {
    // This is a new process, so we need to re-open /proc/self/mem.
    int memfd = open("/proc/self/mem", O_RDONLY);
    TEST_PCHECK_MSG(memfd >= 0, "open failed");
    // Unmap region again
    TEST_PCHECK_MSG(MunmapSafe(mapping.ptr(), mapping.len()) == 0,
                    "munmap failed");
    // Now we want EIO error
    TEST_CHECK(pread(memfd, &output, sizeof(output),
                     reinterpret_cast<off_t>(mapping.ptr())) == -1);
    TEST_PCHECK_MSG(errno == EIO, "pread failed with unexpected errno");
  };

  EXPECT_THAT(InForkedProcess(rest), IsPosixErrorOkAndHolds(0));
}

// Perform read repeatedly to verify offset change.
TEST(ProcPidMem, RepeatedRead) {
  auto const num_reads = 3;
  char expected[] = "01234567890abcdefghijkl";
  char output[sizeof(expected) / num_reads];

  auto memfd = ASSERT_NO_ERRNO_AND_VALUE(Open("/proc/self/mem", O_RDONLY));
  ASSERT_THAT(lseek(memfd.get(), reinterpret_cast<off_t>(&expected), SEEK_SET),
              SyscallSucceedsWithValue(reinterpret_cast<off_t>(&expected)));
  for (auto i = 0; i < num_reads; i++) {
    ASSERT_THAT(read(memfd.get(), &output, sizeof(output)),
                SyscallSucceedsWithValue(sizeof(output)));
    ASSERT_EQ(strncmp(&expected[i * sizeof(output)], output, sizeof(output)),
              0);
  }
}

// Perform seek operations repeatedly.
TEST(ProcPidMem, RepeatedSeek) {
  auto const num_reads = 3;
  char expected[] = "01234567890abcdefghijkl";
  char output[sizeof(expected) / num_reads];

  auto memfd = ASSERT_NO_ERRNO_AND_VALUE(Open("/proc/self/mem", O_RDONLY));
  ASSERT_THAT(lseek(memfd.get(), reinterpret_cast<off_t>(&expected), SEEK_SET),
              SyscallSucceedsWithValue(reinterpret_cast<off_t>(&expected)));
  // Read from start
  ASSERT_THAT(read(memfd.get(), &output, sizeof(output)),
              SyscallSucceedsWithValue(sizeof(output)));
  ASSERT_EQ(strncmp(&expected[0 * sizeof(output)], output, sizeof(output)), 0);
  // Skip ahead one read
  ASSERT_THAT(lseek(memfd.get(), sizeof(output), SEEK_CUR),
              SyscallSucceedsWithValue(reinterpret_cast<off_t>(&expected) +
                                       sizeof(output) * 2));
  // Do read again
  ASSERT_THAT(read(memfd.get(), &output, sizeof(output)),
              SyscallSucceedsWithValue(sizeof(output)));
  ASSERT_EQ(strncmp(&expected[2 * sizeof(output)], output, sizeof(output)), 0);
  // Skip back three reads
  ASSERT_THAT(lseek(memfd.get(), -3 * sizeof(output), SEEK_CUR),
              SyscallSucceedsWithValue(reinterpret_cast<off_t>(&expected)));
  // Do read again
  ASSERT_THAT(read(memfd.get(), &output, sizeof(output)),
              SyscallSucceedsWithValue(sizeof(output)));
  ASSERT_EQ(strncmp(&expected[0 * sizeof(output)], output, sizeof(output)), 0);
  // Check that SEEK_END does not work
  ASSERT_THAT(lseek(memfd.get(), 0, SEEK_END), SyscallFailsWithErrno(EINVAL));
}

// Perform read past an allocated memory region.
TEST(ProcPidMem, PartialRead) {
  // Reserve 2 pages.
  Mapping mapping = ASSERT_NO_ERRNO_AND_VALUE(
      MmapAnon(2 * kPageSize, PROT_READ | PROT_WRITE, MAP_PRIVATE));

  // Fill the first page with data.
  memset(mapping.ptr(), 'x', kPageSize);

  char expected[] = {'x'};
  std::unique_ptr<char[]> output(new char[kPageSize]);
  off_t read_offset = reinterpret_cast<off_t>(mapping.ptr()) + kPageSize - 1;
  const auto rest = [&] {
    int memfd = open("/proc/self/mem", O_RDONLY);
    TEST_PCHECK_MSG(memfd >= 0, "open failed");
    // Unmap the second page.
    TEST_PCHECK_MSG(
        MunmapSafe(reinterpret_cast<void*>(mapping.addr() + kPageSize),
                   kPageSize) == 0,
        "munmap failed");
    // Expect to read up to the end of the first page without getting EIO.
    TEST_PCHECK_MSG(
        pread(memfd, output.get(), kPageSize, read_offset) == sizeof(expected),
        "pread failed");
    TEST_CHECK(expected[0] == output.get()[0]);
  };

  EXPECT_THAT(InForkedProcess(rest), IsPosixErrorOkAndHolds(0));
}

// Perform read on /proc/[pid]/mem after exit.
TEST(ProcPidMem, AfterExit) {
  int pfd1[2] = {};
  int pfd2[2] = {};

  char expected[] = "hello-world";

  ASSERT_THAT(pipe(pfd1), SyscallSucceeds());
  ASSERT_THAT(pipe(pfd2), SyscallSucceeds());

  // Create child process
  pid_t const child_pid = fork();
  if (child_pid == 0) {
    // Close reading end of first pipe
    close(pfd1[0]);

    // Tell parent about location of input
    char ok = 1;
    TEST_CHECK(WriteFd(pfd1[1], &ok, sizeof(ok)) == sizeof(ok));
    TEST_PCHECK(close(pfd1[1]) == 0);

    // Close writing end of second pipe
    TEST_PCHECK(close(pfd2[1]) == 0);

    // Await parent OK to die
    ok = 0;
    TEST_CHECK(ReadFd(pfd2[0], &ok, sizeof(ok)) == sizeof(ok));

    // Close rest pipes
    TEST_PCHECK(close(pfd2[0]) == 0);
    _exit(0);
  }

  // In parent process.
  ASSERT_THAT(child_pid, SyscallSucceeds());

  // Close writing end of first pipe
  EXPECT_THAT(close(pfd1[1]), SyscallSucceeds());

  // Wait for child to be alive and well
  char ok = 0;
  EXPECT_THAT(ReadFd(pfd1[0], &ok, sizeof(ok)),
              SyscallSucceedsWithValue(sizeof(ok)));
  // Close reading end of first pipe
  EXPECT_THAT(close(pfd1[0]), SyscallSucceeds());

  // Open /proc/pid/mem fd
  std::string mempath = absl::StrCat("/proc/", child_pid, "/mem");
  auto memfd = ASSERT_NO_ERRNO_AND_VALUE(Open(mempath, O_RDONLY));

  // Expect that we can read
  char output[sizeof(expected)];
  EXPECT_THAT(pread(memfd.get(), &output, sizeof(output),
                    reinterpret_cast<off_t>(&expected)),
              SyscallSucceedsWithValue(sizeof(output)));
  EXPECT_STREQ(expected, output);

  // Tell proc its ok to go
  EXPECT_THAT(close(pfd2[0]), SyscallSucceeds());
  ok = 1;
  EXPECT_THAT(WriteFd(pfd2[1], &ok, sizeof(ok)),
              SyscallSucceedsWithValue(sizeof(ok)));
  EXPECT_THAT(close(pfd2[1]), SyscallSucceeds());

  // Expect termination
  int status;
  ASSERT_THAT(waitpid(child_pid, &status, 0), SyscallSucceeds());

  // Expect that we can't read anymore
  EXPECT_THAT(pread(memfd.get(), &output, sizeof(output),
                    reinterpret_cast<off_t>(&expected)),
              SyscallSucceedsWithValue(0));
}

// Read from /proc/[pid]/mem with different UID/GID and attached state.
TEST(ProcPidMem, DifferentUserAttached) {
  SKIP_IF(!ASSERT_NO_ERRNO_AND_VALUE(HaveCapability(CAP_SETUID)));
  SKIP_IF(!ASSERT_NO_ERRNO_AND_VALUE(HaveCapability(CAP_DAC_OVERRIDE)));
  SKIP_IF(!ASSERT_NO_ERRNO_AND_VALUE(HaveCapability(CAP_SYS_PTRACE)));

  int pfd1[2] = {};
  int pfd2[2] = {};

  ASSERT_THAT(pipe(pfd1), SyscallSucceeds());
  ASSERT_THAT(pipe(pfd2), SyscallSucceeds());

  // Create child process
  pid_t const child_pid = fork();
  if (child_pid == 0) {
    // Close reading end of first pipe
    close(pfd1[0]);

    // Tell parent about location of input
    char input[] = "hello-world";
    off_t input_location = reinterpret_cast<off_t>(input);
    TEST_CHECK(WriteFd(pfd1[1], &input_location, sizeof(input_location)) ==
               sizeof(input_location));
    TEST_PCHECK(close(pfd1[1]) == 0);

    // Close writing end of second pipe
    TEST_PCHECK(close(pfd2[1]) == 0);

    // Await parent OK to die
    char ok = 0;
    TEST_CHECK(ReadFd(pfd2[0], &ok, sizeof(ok)) == sizeof(ok));

    // Close rest pipes
    TEST_PCHECK(close(pfd2[0]) == 0);
    _exit(0);
  }

  // In parent process.
  ASSERT_THAT(child_pid, SyscallSucceeds());

  // Close writing end of first pipe
  EXPECT_THAT(close(pfd1[1]), SyscallSucceeds());

  // Read target location from child
  off_t target_location;
  EXPECT_THAT(ReadFd(pfd1[0], &target_location, sizeof(target_location)),
              SyscallSucceedsWithValue(sizeof(target_location)));
  // Close reading end of first pipe
  EXPECT_THAT(close(pfd1[0]), SyscallSucceeds());

  ScopedThread([&] {
    // Attach to child subprocess without stopping it
    EXPECT_THAT(ptrace(PTRACE_SEIZE, child_pid, NULL, NULL), SyscallSucceeds());

    // Keep capabilities after setuid
    EXPECT_THAT(prctl(PR_SET_KEEPCAPS, 1, 0, 0, 0), SyscallSucceeds());
    constexpr int kNobody = 65534;
    EXPECT_THAT(syscall(SYS_setuid, kNobody), SyscallSucceeds());

    // Only restore CAP_SYS_PTRACE and CAP_DAC_OVERRIDE
    EXPECT_NO_ERRNO(SetCapability(CAP_SYS_PTRACE, true));
    EXPECT_NO_ERRNO(SetCapability(CAP_DAC_OVERRIDE, true));

    // Open /proc/pid/mem fd
    std::string mempath = absl::StrCat("/proc/", child_pid, "/mem");
    auto memfd = ASSERT_NO_ERRNO_AND_VALUE(Open(mempath, O_RDONLY));
    char expected[] = "hello-world";
    char output[sizeof(expected)];
    EXPECT_THAT(pread(memfd.get(), output, sizeof(output),
                      reinterpret_cast<off_t>(target_location)),
                SyscallSucceedsWithValue(sizeof(output)));
    EXPECT_STREQ(expected, output);

    // Tell proc its ok to go
    EXPECT_THAT(close(pfd2[0]), SyscallSucceeds());
    char ok = 1;
    EXPECT_THAT(WriteFd(pfd2[1], &ok, sizeof(ok)),
                SyscallSucceedsWithValue(sizeof(ok)));
    EXPECT_THAT(close(pfd2[1]), SyscallSucceeds());

    // Expect termination
    int status;
    ASSERT_THAT(waitpid(child_pid, &status, 0), SyscallSucceeds());
    EXPECT_TRUE(WIFEXITED(status) && WEXITSTATUS(status) == 0)
        << " status " << status;
  });
}

// Attempt to read from /proc/[pid]/mem with different UID/GID.
TEST(ProcPidMem, DifferentUser) {
  SKIP_IF(!ASSERT_NO_ERRNO_AND_VALUE(HaveCapability(CAP_SETUID)));

  int pfd1[2] = {};
  int pfd2[2] = {};

  ASSERT_THAT(pipe(pfd1), SyscallSucceeds());
  ASSERT_THAT(pipe(pfd2), SyscallSucceeds());

  // Create child process
  pid_t const child_pid = fork();
  if (child_pid == 0) {
    // Close reading end of first pipe
    close(pfd1[0]);

    // Tell parent about location of input
    char input[] = "hello-world";
    off_t input_location = reinterpret_cast<off_t>(input);
    TEST_CHECK(WriteFd(pfd1[1], &input_location, sizeof(input_location)) ==
               sizeof(input_location));
    TEST_PCHECK(close(pfd1[1]) == 0);

    // Close writing end of second pipe
    TEST_PCHECK(close(pfd2[1]) == 0);

    // Await parent OK to die
    char ok = 0;
    TEST_CHECK(ReadFd(pfd2[0], &ok, sizeof(ok)) == sizeof(ok));

    // Close rest pipes
    TEST_PCHECK(close(pfd2[0]) == 0);
    _exit(0);
  }

  // In parent process.
  ASSERT_THAT(child_pid, SyscallSucceeds());

  // Close writing end of first pipe
  EXPECT_THAT(close(pfd1[1]), SyscallSucceeds());

  // Read target location from child
  off_t target_location;
  EXPECT_THAT(ReadFd(pfd1[0], &target_location, sizeof(target_location)),
              SyscallSucceedsWithValue(sizeof(target_location)));
  // Close reading end of first pipe
  EXPECT_THAT(close(pfd1[0]), SyscallSucceeds());

  ScopedThread([&] {
    constexpr int kNobody = 65534;
    EXPECT_THAT(syscall(SYS_setuid, kNobody), SyscallSucceeds());

    // Attempt to open /proc/[child_pid]/mem
    std::string mempath = absl::StrCat("/proc/", child_pid, "/mem");
    EXPECT_THAT(open(mempath.c_str(), O_RDONLY), SyscallFailsWithErrno(EACCES));

    // Tell proc its ok to go
    EXPECT_THAT(close(pfd2[0]), SyscallSucceeds());
    char ok = 1;
    EXPECT_THAT(WriteFd(pfd2[1], &ok, sizeof(ok)),
                SyscallSucceedsWithValue(sizeof(ok)));
    EXPECT_THAT(close(pfd2[1]), SyscallSucceeds());

    // Expect termination
    int status;
    ASSERT_THAT(waitpid(child_pid, &status, 0), SyscallSucceeds());
  });
}

// Perform read on /proc/[pid]/mem with same UID/GID.
TEST(ProcPidMem, SameUser) {
  int pfd1[2] = {};
  int pfd2[2] = {};

  ASSERT_THAT(pipe(pfd1), SyscallSucceeds());
  ASSERT_THAT(pipe(pfd2), SyscallSucceeds());

  // Create child process
  pid_t const child_pid = fork();
  if (child_pid == 0) {
    // Close reading end of first pipe
    close(pfd1[0]);

    // Tell parent about location of input
    char input[] = "hello-world";
    off_t input_location = reinterpret_cast<off_t>(input);
    TEST_CHECK(WriteFd(pfd1[1], &input_location, sizeof(input_location)) ==
               sizeof(input_location));
    TEST_PCHECK(close(pfd1[1]) == 0);

    // Close writing end of second pipe
    TEST_PCHECK(close(pfd2[1]) == 0);

    // Await parent OK to die
    char ok = 0;
    TEST_CHECK(ReadFd(pfd2[0], &ok, sizeof(ok)) == sizeof(ok));

    // Close rest pipes
    TEST_PCHECK(close(pfd2[0]) == 0);
    _exit(0);
  }
  // In parent process.
  ASSERT_THAT(child_pid, SyscallSucceeds());

  // Close writing end of first pipe
  EXPECT_THAT(close(pfd1[1]), SyscallSucceeds());

  // Read target location from child
  off_t target_location;
  EXPECT_THAT(ReadFd(pfd1[0], &target_location, sizeof(target_location)),
              SyscallSucceedsWithValue(sizeof(target_location)));
  // Close reading end of first pipe
  EXPECT_THAT(close(pfd1[0]), SyscallSucceeds());

  // Open /proc/pid/mem fd
  std::string mempath = absl::StrCat("/proc/", child_pid, "/mem");
  auto memfd = ASSERT_NO_ERRNO_AND_VALUE(Open(mempath, O_RDONLY));
  char expected[] = "hello-world";
  char output[sizeof(expected)];
  EXPECT_THAT(pread(memfd.get(), output, sizeof(output),
                    reinterpret_cast<off_t>(target_location)),
              SyscallSucceedsWithValue(sizeof(output)));
  EXPECT_STREQ(expected, output);

  // Tell proc its ok to go
  EXPECT_THAT(close(pfd2[0]), SyscallSucceeds());
  char ok = 1;
  EXPECT_THAT(WriteFd(pfd2[1], &ok, sizeof(ok)),
              SyscallSucceedsWithValue(sizeof(ok)));
  EXPECT_THAT(close(pfd2[1]), SyscallSucceeds());

  // Expect termination
  int status;
  ASSERT_THAT(waitpid(child_pid, &status, 0), SyscallSucceeds());
}

// Just open and read /proc/self/maps, check that we can find [stack]
TEST(ProcSelfMaps, Basic) {
  auto proc_self_maps =
      ASSERT_NO_ERRNO_AND_VALUE(GetContents("/proc/self/maps"));

  std::vector<std::string> strings = absl::StrSplit(proc_self_maps, '\n');
  std::vector<std::string> stacks;
  // Make sure there's a stack in there.
  for (const auto& str : strings) {
    if (str.find("[stack]") != std::string::npos) {
      stacks.push_back(str);
    }
  }
  ASSERT_EQ(1, stacks.size()) << "[stack] not found in: " << proc_self_maps;
  // Linux pads to 73 characters then we add 7.
  EXPECT_EQ(80, stacks[0].length());
}

TEST(ProcSelfMaps, Map1) {
  Mapping mapping =
      ASSERT_NO_ERRNO_AND_VALUE(MmapAnon(kPageSize, PROT_READ, MAP_PRIVATE));
  auto proc_self_maps =
      ASSERT_NO_ERRNO_AND_VALUE(GetContents("/proc/self/maps"));
  std::vector<std::string> strings = absl::StrSplit(proc_self_maps, '\n');
  std::vector<std::string> addrs;
  // Make sure if is listed.
  for (const auto& str : strings) {
    if (str == AnonymousMapsEntryForMapping(mapping, PROT_READ)) {
      addrs.push_back(str);
    }
  }
  ASSERT_EQ(1, addrs.size());
}

TEST(ProcSelfMaps, Map2) {
  // NOTE(magi): The permissions must be different or the pages will get merged.
  Mapping map1 = ASSERT_NO_ERRNO_AND_VALUE(
      MmapAnon(kPageSize, PROT_READ | PROT_EXEC, MAP_PRIVATE));
  Mapping map2 =
      ASSERT_NO_ERRNO_AND_VALUE(MmapAnon(kPageSize, PROT_WRITE, MAP_PRIVATE));

  auto proc_self_maps =
      ASSERT_NO_ERRNO_AND_VALUE(GetContents("/proc/self/maps"));
  std::vector<std::string> strings = absl::StrSplit(proc_self_maps, '\n');
  std::vector<std::string> addrs;
  // Make sure if is listed.
  for (const auto& str : strings) {
    if (str == AnonymousMapsEntryForMapping(map1, PROT_READ | PROT_EXEC)) {
      addrs.push_back(str);
    }
  }
  ASSERT_EQ(1, addrs.size());
  addrs.clear();
  for (const auto& str : strings) {
    if (str == AnonymousMapsEntryForMapping(map2, PROT_WRITE)) {
      addrs.push_back(str);
    }
  }
  ASSERT_EQ(1, addrs.size());
}

TEST(ProcSelfMaps, MapUnmap) {
  Mapping map1 = ASSERT_NO_ERRNO_AND_VALUE(
      MmapAnon(kPageSize, PROT_READ | PROT_EXEC, MAP_PRIVATE));
  Mapping map2 =
      ASSERT_NO_ERRNO_AND_VALUE(MmapAnon(kPageSize, PROT_WRITE, MAP_PRIVATE));

  auto proc_self_maps =
      ASSERT_NO_ERRNO_AND_VALUE(GetContents("/proc/self/maps"));
  std::vector<std::string> strings = absl::StrSplit(proc_self_maps, '\n');
  std::vector<std::string> addrs;
  // Make sure if is listed.
  for (const auto& str : strings) {
    if (str == AnonymousMapsEntryForMapping(map1, PROT_READ | PROT_EXEC)) {
      addrs.push_back(str);
    }
  }
  ASSERT_EQ(1, addrs.size()) << proc_self_maps;
  addrs.clear();
  for (const auto& str : strings) {
    if (str == AnonymousMapsEntryForMapping(map2, PROT_WRITE)) {
      addrs.push_back(str);
    }
  }
  ASSERT_EQ(1, addrs.size());

  map2.reset();

  // Read it again.
  proc_self_maps = ASSERT_NO_ERRNO_AND_VALUE(GetContents("/proc/self/maps"));
  strings = absl::StrSplit(proc_self_maps, '\n');
  // First entry should be there.
  addrs.clear();
  for (const auto& str : strings) {
    if (str == AnonymousMapsEntryForMapping(map1, PROT_READ | PROT_EXEC)) {
      addrs.push_back(str);
    }
  }
  ASSERT_EQ(1, addrs.size());
  addrs.clear();
  // But not the second.
  for (const auto& str : strings) {
    if (str == AnonymousMapsEntryForMapping(map2, PROT_WRITE)) {
      addrs.push_back(str);
    }
  }
  ASSERT_EQ(0, addrs.size());
}

TEST(ProcSelfMaps, Mprotect) {
  // FIXME(jamieliu): Linux's mprotect() sometimes fails to merge VMAs in this
  // case.
  SKIP_IF(!IsRunningOnGvisor());

  // Reserve 5 pages of address space.
  Mapping m = ASSERT_NO_ERRNO_AND_VALUE(
      MmapAnon(5 * kPageSize, PROT_NONE, MAP_PRIVATE));

  // Change the permissions on the middle 3 pages. (The first and last pages may
  // be merged with other vmas on either side, so they aren't tested directly;
  // they just ensure that the middle 3 pages are bracketed by VMAs with
  // incompatible permissions.)
  ASSERT_THAT(mprotect(reinterpret_cast<void*>(m.addr() + kPageSize),
                       3 * kPageSize, PROT_READ),
              SyscallSucceeds());

  // Check that the middle 3 pages make up a single VMA.
  auto proc_self_maps =
      ASSERT_NO_ERRNO_AND_VALUE(GetContents("/proc/self/maps"));
  std::vector<std::string> strings = absl::StrSplit(proc_self_maps, '\n');
  EXPECT_THAT(strings, Contains(AnonymousMapsEntry(m.addr() + kPageSize,
                                                   3 * kPageSize, PROT_READ)));

  // Change the permissions on the middle page only.
  ASSERT_THAT(mprotect(reinterpret_cast<void*>(m.addr() + 2 * kPageSize),
                       kPageSize, PROT_READ | PROT_WRITE),
              SyscallSucceeds());

  // Check that the single VMA has been split into 3 VMAs.
  proc_self_maps = ASSERT_NO_ERRNO_AND_VALUE(GetContents("/proc/self/maps"));
  strings = absl::StrSplit(proc_self_maps, '\n');
  EXPECT_THAT(
      strings,
      IsSupersetOf(
          {AnonymousMapsEntry(m.addr() + kPageSize, kPageSize, PROT_READ),
           AnonymousMapsEntry(m.addr() + 2 * kPageSize, kPageSize,
                              PROT_READ | PROT_WRITE),
           AnonymousMapsEntry(m.addr() + 3 * kPageSize, kPageSize,
                              PROT_READ)}));

  // Change the permissions on the middle page back.
  ASSERT_THAT(mprotect(reinterpret_cast<void*>(m.addr() + 2 * kPageSize),
                       kPageSize, PROT_READ),
              SyscallSucceeds());

  // Check that the 3 VMAs have been merged back into a single VMA.
  proc_self_maps = ASSERT_NO_ERRNO_AND_VALUE(GetContents("/proc/self/maps"));
  strings = absl::StrSplit(proc_self_maps, '\n');
  EXPECT_THAT(strings, Contains(AnonymousMapsEntry(m.addr() + kPageSize,
                                                   3 * kPageSize, PROT_READ)));
}

// Expected pathname for MAP_SHARED | MAP_ANONYMOUS mappings. See proc(5),
// "/proc/[pid]/map_files/".
constexpr char kSharedAnonPath[] = "/dev/zero (deleted)";

TEST(ProcSelfMaps, SharedAnon) {
  const Mapping m = ASSERT_NO_ERRNO_AND_VALUE(
      MmapAnon(kPageSize, PROT_READ, MAP_SHARED | MAP_ANONYMOUS));

  const auto proc_self_maps =
      ASSERT_NO_ERRNO_AND_VALUE(GetContents("/proc/self/maps"));
  const auto entries = ASSERT_NO_ERRNO_AND_VALUE(ParseProcMaps(proc_self_maps));
  const auto entry =
      ASSERT_NO_ERRNO_AND_VALUE(FindUniqueMapsEntry(entries, m.addr()));
  EXPECT_EQ(entry.filename, kSharedAnonPath);
}

#ifndef PR_SET_VMA
#define PR_SET_VMA 0x53564d41
#endif
#ifndef PR_SET_VMA_ANON_NAME
#define PR_SET_VMA_ANON_NAME 0
#endif

TEST(ProcSelfMaps, AnonNamePrivateAnon) {
  const Mapping m = ASSERT_NO_ERRNO_AND_VALUE(
      MmapAnon(kPageSize, PROT_READ, MAP_PRIVATE | MAP_ANONYMOUS));

  int rv = prctl(PR_SET_VMA, PR_SET_VMA_ANON_NAME, m.addr(), m.len(), "test");
  SKIP_IF(rv < 0 && errno == EINVAL);
  ASSERT_THAT(rv, SyscallSucceeds());
  auto proc_self_maps =
      ASSERT_NO_ERRNO_AND_VALUE(GetContents("/proc/self/maps"));
  auto entries = ASSERT_NO_ERRNO_AND_VALUE(ParseProcMaps(proc_self_maps));
  auto entry =
      ASSERT_NO_ERRNO_AND_VALUE(FindUniqueMapsEntry(entries, m.addr()));
  EXPECT_EQ(entry.filename, "[anon:test]");

  ASSERT_THAT(prctl(PR_SET_VMA, PR_SET_VMA_ANON_NAME, m.addr(), m.len(), ""),
              SyscallSucceeds());
  proc_self_maps = ASSERT_NO_ERRNO_AND_VALUE(GetContents("/proc/self/maps"));
  entries = ASSERT_NO_ERRNO_AND_VALUE(ParseProcMaps(proc_self_maps));
  entry = ASSERT_NO_ERRNO_AND_VALUE(FindUniqueMapsEntry(entries, m.addr()));
  EXPECT_EQ(entry.filename, "[anon:]");

  ASSERT_THAT(
      prctl(PR_SET_VMA, PR_SET_VMA_ANON_NAME, m.addr(), m.len(), nullptr),
      SyscallSucceeds());
  proc_self_maps = ASSERT_NO_ERRNO_AND_VALUE(GetContents("/proc/self/maps"));
  entries = ASSERT_NO_ERRNO_AND_VALUE(ParseProcMaps(proc_self_maps));
  entry = ASSERT_NO_ERRNO_AND_VALUE(FindUniqueMapsEntry(entries, m.addr()));
  EXPECT_EQ(entry.filename, "");
}

TEST(ProcSelfMaps, AnonNameSharedAnon) {
  const Mapping m = ASSERT_NO_ERRNO_AND_VALUE(
      MmapAnon(kPageSize, PROT_READ, MAP_SHARED | MAP_ANONYMOUS));

  int rv = prctl(PR_SET_VMA, PR_SET_VMA_ANON_NAME, m.addr(), m.len(), "test");
  SKIP_IF(rv < 0 && errno == EINVAL);
  // Using PR_SET_VMA_ANON_NAME on shared anonymous mappings isn't permitted
  // until d09e8ca6cb93 ("mm: anonymous shared memory naming"), Linux 6.2+.
  SKIP_IF(rv < 0 && errno == EBADF);
  ASSERT_THAT(rv, SyscallSucceeds());
  auto proc_self_maps =
      ASSERT_NO_ERRNO_AND_VALUE(GetContents("/proc/self/maps"));
  auto entries = ASSERT_NO_ERRNO_AND_VALUE(ParseProcMaps(proc_self_maps));
  auto entry =
      ASSERT_NO_ERRNO_AND_VALUE(FindUniqueMapsEntry(entries, m.addr()));
  EXPECT_EQ(entry.filename, "[anon_shmem:test]");

  ASSERT_THAT(prctl(PR_SET_VMA, PR_SET_VMA_ANON_NAME, m.addr(), m.len(), ""),
              SyscallSucceeds());
  proc_self_maps = ASSERT_NO_ERRNO_AND_VALUE(GetContents("/proc/self/maps"));
  entries = ASSERT_NO_ERRNO_AND_VALUE(ParseProcMaps(proc_self_maps));
  entry = ASSERT_NO_ERRNO_AND_VALUE(FindUniqueMapsEntry(entries, m.addr()));
  EXPECT_EQ(entry.filename, "[anon_shmem:]");

  ASSERT_THAT(
      prctl(PR_SET_VMA, PR_SET_VMA_ANON_NAME, m.addr(), m.len(), nullptr),
      SyscallSucceeds());
  proc_self_maps = ASSERT_NO_ERRNO_AND_VALUE(GetContents("/proc/self/maps"));
  entries = ASSERT_NO_ERRNO_AND_VALUE(ParseProcMaps(proc_self_maps));
  entry = ASSERT_NO_ERRNO_AND_VALUE(FindUniqueMapsEntry(entries, m.addr()));
  EXPECT_EQ(entry.filename, kSharedAnonPath);
}

// Test parameterized by mmap flags.
class ProcSelfMapsMmapFileTest : public ::testing::TestWithParam<int> {};

TEST_P(ProcSelfMapsMmapFileTest, AnonNameFile) {
  const auto f = ASSERT_NO_ERRNO_AND_VALUE(TempPath::CreateFile());
  const auto fd = ASSERT_NO_ERRNO_AND_VALUE(Open(f.path(), O_RDONLY));
  const Mapping m = ASSERT_NO_ERRNO_AND_VALUE(
      Mmap(nullptr, kPageSize, PROT_READ, GetParam(), fd.get(), 0));

  int rv = prctl(PR_SET_VMA, PR_SET_VMA_ANON_NAME, m.addr(), m.len(), "test");
  SKIP_IF(rv < 0 && errno == EINVAL);
  ASSERT_THAT(rv, SyscallFailsWithErrno(EBADF));
  auto proc_self_maps =
      ASSERT_NO_ERRNO_AND_VALUE(GetContents("/proc/self/maps"));
  auto entries = ASSERT_NO_ERRNO_AND_VALUE(ParseProcMaps(proc_self_maps));
  auto entry =
      ASSERT_NO_ERRNO_AND_VALUE(FindUniqueMapsEntry(entries, m.addr()));
  EXPECT_EQ(entry.filename, f.path());

  ASSERT_THAT(
      prctl(PR_SET_VMA, PR_SET_VMA_ANON_NAME, m.addr(), m.len(), nullptr),
      SyscallFailsWithErrno(EBADF));
  proc_self_maps = ASSERT_NO_ERRNO_AND_VALUE(GetContents("/proc/self/maps"));
  entries = ASSERT_NO_ERRNO_AND_VALUE(ParseProcMaps(proc_self_maps));
  entry = ASSERT_NO_ERRNO_AND_VALUE(FindUniqueMapsEntry(entries, m.addr()));
  EXPECT_EQ(entry.filename, f.path());
}

INSTANTIATE_TEST_SUITE_P(SelfAndNumericPid, ProcSelfMapsMmapFileTest,
                         ::testing::Values(MAP_SHARED, MAP_PRIVATE));

TEST(ProcSelfFd, OpenFd) {
  int pipe_fds[2];
  ASSERT_THAT(pipe2(pipe_fds, O_CLOEXEC), SyscallSucceeds());

  // Reopen the write end.
  const std::string path = absl::StrCat("/proc/self/fd/", pipe_fds[1]);
  const FileDescriptor fd = ASSERT_NO_ERRNO_AND_VALUE(Open(path, O_WRONLY));

  // Ensure that a read/write works.
  const std::string data = "hello";
  std::unique_ptr<char[]> buffer(new char[data.size()]);
  EXPECT_THAT(write(fd.get(), data.c_str(), data.size()),
              SyscallSucceedsWithValue(5));
  EXPECT_THAT(read(pipe_fds[0], buffer.get(), data.size()),
              SyscallSucceedsWithValue(5));
  EXPECT_EQ(strncmp(buffer.get(), data.c_str(), data.size()), 0);

  // Cleanup.
  ASSERT_THAT(close(pipe_fds[0]), SyscallSucceeds());
  ASSERT_THAT(close(pipe_fds[1]), SyscallSucceeds());
}

static void CheckFdDirGetdentsDuplicates(const std::string& path) {
  const FileDescriptor fd =
      ASSERT_NO_ERRNO_AND_VALUE(Open(path.c_str(), O_RDONLY | O_DIRECTORY));
  // Open a FD whose value is supposed to be much larger than
  // the number of FDs opened by current process.
  auto newfd = fcntl(fd.get(), F_DUPFD, 1024);
  EXPECT_GE(newfd, 1024);
  auto fd_closer = Cleanup([newfd]() { close(newfd); });
  auto fd_files = ASSERT_NO_ERRNO_AND_VALUE(ListDir(path.c_str(), false));
  absl::flat_hash_set<std::string> fd_files_dedup(fd_files.begin(),
                                                  fd_files.end());
  EXPECT_EQ(fd_files.size(), fd_files_dedup.size());
}

// This is a regression test for gvisor.dev/issues/3894
TEST(ProcSelfFd, GetdentsDuplicates) {
  CheckFdDirGetdentsDuplicates("/proc/self/fd");
}

// This is a regression test for gvisor.dev/issues/3894
TEST(ProcSelfFdInfo, GetdentsDuplicates) {
  CheckFdDirGetdentsDuplicates("/proc/self/fdinfo");
}

TEST(ProcSelfFdInfo, CorrectFds) {
  // Make sure there is at least one open file.
  auto f = ASSERT_NO_ERRNO_AND_VALUE(TempPath::CreateFile());
  const FileDescriptor fd = ASSERT_NO_ERRNO_AND_VALUE(Open(f.path(), O_RDONLY));

  // Get files in /proc/self/fd.
  auto fd_files = ASSERT_NO_ERRNO_AND_VALUE(ListDir("/proc/self/fd", false));

  // Get files in /proc/self/fdinfo.
  auto fdinfo_files =
      ASSERT_NO_ERRNO_AND_VALUE(ListDir("/proc/self/fdinfo", false));

  // They should contain the same fds.
  EXPECT_THAT(fd_files, UnorderedElementsAreArray(fdinfo_files));

  // Both should contain fd.
  auto fd_s = absl::StrCat(fd.get());
  EXPECT_THAT(fd_files, Contains(fd_s));
}

TEST(ProcSelfFdInfo, Flags) {
  std::string path = NewTempAbsPath();

  // Create file here with O_CREAT to test that O_CREAT does not appear in
  // fdinfo flags.
  int flags = O_CREAT | O_RDWR | O_APPEND | O_CLOEXEC;
  const FileDescriptor fd = ASSERT_NO_ERRNO_AND_VALUE(Open(path, flags, 0644));

  // Automatically delete path.
  TempPath temp_path(path);

  // O_CREAT does not appear in fdinfo flags.
  flags &= ~O_CREAT;

  // O_LARGEFILE always appears (on x86_64).
  flags |= kOLargeFile;

  auto fd_info = ASSERT_NO_ERRNO_AND_VALUE(
      GetContents(absl::StrCat("/proc/self/fdinfo/", fd.get())));
  EXPECT_THAT(fd_info, HasSubstr(absl::StrFormat("flags:\t%#o", flags)));
}

TEST(ProcSelfExe, Absolute) {
  auto exe = ASSERT_NO_ERRNO_AND_VALUE(ReadLink("/proc/self/exe"));
  EXPECT_EQ(exe[0], '/');
}

TEST(ProcSelfCwd, Absolute) {
  auto exe = ASSERT_NO_ERRNO_AND_VALUE(ReadLink("/proc/self/cwd"));
  EXPECT_EQ(exe[0], '/');
}

TEST(ProcSelfRoot, IsRoot) {
  auto exe = ASSERT_NO_ERRNO_AND_VALUE(ReadLink("/proc/self/root"));
  EXPECT_EQ(exe, "/");
}

// Sanity check that /proc/cmdline is present.
TEST(ProcCmdline, IsPresent) {
  std::string proc_cmdline =
      ASSERT_NO_ERRNO_AND_VALUE(GetContents("/proc/cmdline"));
  ASSERT_FALSE(proc_cmdline.empty());
}

// Sanity check for /proc/cpuinfo fields that must be present.
TEST(ProcCpuinfo, RequiredFieldsArePresent) {
  std::string proc_cpuinfo =
      ASSERT_NO_ERRNO_AND_VALUE(GetContents("/proc/cpuinfo"));
  ASSERT_FALSE(proc_cpuinfo.empty());

  // Check that the usual fields are there. We don't really care about the
  // contents.
  for (const char* field : required_fields) {
    EXPECT_THAT(proc_cpuinfo, HasSubstr(field));
  }
}

TEST(ProcCpuinfo, DeniesWriteNonRoot) {
  SKIP_IF(!ASSERT_NO_ERRNO_AND_VALUE(HaveCapability(CAP_FOWNER)));

  // Do setuid in a separate thread so that after finishing this test, the
  // process can still open files the test harness created before starting this
  // test. Otherwise, the files are created by root (UID before the test), but
  // cannot be opened by the `uid` set below after the test. After calling
  // setuid(non-zero-UID), there is no way to get root privileges back.
  ScopedThread([&] {
    // Use syscall instead of glibc setuid wrapper because we want this setuid
    // call to only apply to this task. POSIX threads, however, require that all
    // threads have the same UIDs, so using the setuid wrapper sets all threads'
    // real UID.
    // Also drops capabilities.
    constexpr int kNobody = 65534;
    EXPECT_THAT(syscall(SYS_setuid, kNobody), SyscallSucceeds());
    EXPECT_THAT(open("/proc/cpuinfo", O_WRONLY), SyscallFailsWithErrno(EACCES));
    EXPECT_THAT(truncate("/proc/cpuinfo", 123), SyscallFailsWithErrno(EACCES));
  });
}

// With root privileges, it is possible to open /proc/cpuinfo with write mode,
// but all write operations should fail.
TEST(ProcCpuinfo, DeniesWriteRoot) {
  SKIP_IF(!ASSERT_NO_ERRNO_AND_VALUE(HaveCapability(CAP_FOWNER)));

  int fd;
  EXPECT_THAT(fd = open("/proc/cpuinfo", O_WRONLY), SyscallSucceeds());
  if (fd > 0) {
    // Truncate is not tested--it may succeed on some kernels without doing
    // anything.
    EXPECT_THAT(write(fd, "x", 1), SyscallFails());
    EXPECT_THAT(pwrite(fd, "x", 1, 123), SyscallFails());
  }
}

// Cpuinfo should not change across save/restore.
TEST(ProcCpuinfo, Stable) {
  std::string output_before;
  ASSERT_NO_ERRNO(GetContents("/proc/cpuinfo", &output_before));
  MaybeSave();
  std::string output_after;
  ASSERT_NO_ERRNO(GetContents("/proc/cpuinfo", &output_after));

  std::vector<std::string> before_fields = absl::StrSplit(output_before, '\n');
  std::vector<std::string> after_fields = absl::StrSplit(output_before, '\n');
  RemoveUnstableCPUInfoFields(before_fields);
  RemoveUnstableCPUInfoFields(after_fields);

  EXPECT_THAT(absl::StrJoin(before_fields, "\n"),
              Eq(absl::StrJoin(after_fields, "\n")));
}

// Sanity checks that uptime is present.
TEST(ProcUptime, IsPresent) {
  std::string proc_uptime =
      ASSERT_NO_ERRNO_AND_VALUE(GetContents("/proc/uptime"));
  ASSERT_FALSE(proc_uptime.empty());
  std::vector<std::string> uptime_parts = absl::StrSplit(proc_uptime, ' ');

  // Parse once.
  double uptime0, uptime1, idletime0, idletime1;
  ASSERT_TRUE(absl::SimpleAtod(uptime_parts[0], &uptime0));
  ASSERT_TRUE(absl::SimpleAtod(uptime_parts[1], &idletime0));

  // Sleep for one second.
  absl::SleepFor(absl::Seconds(1));

  // Parse again.
  proc_uptime = ASSERT_NO_ERRNO_AND_VALUE(GetContents("/proc/uptime"));
  ASSERT_FALSE(proc_uptime.empty());
  uptime_parts = absl::StrSplit(proc_uptime, ' ');
  ASSERT_TRUE(absl::SimpleAtod(uptime_parts[0], &uptime1));
  ASSERT_TRUE(absl::SimpleAtod(uptime_parts[1], &idletime1));

  // Sanity check.
  //
  // We assert that between 0.99 and 59.99 seconds have passed. If more than a
  // minute has passed, then we must be executing really, really slowly.
  EXPECT_GE(uptime0, 0.0);
  EXPECT_GE(idletime0, 0.0);
  EXPECT_GT(uptime1, uptime0);
  EXPECT_GE(uptime1, uptime0 + 0.99);
  EXPECT_LE(uptime1, uptime0 + 59.99);
  EXPECT_GE(idletime1, idletime0);
}

TEST(ProcMeminfo, ContainsBasicFields) {
  std::string proc_meminfo =
      ASSERT_NO_ERRNO_AND_VALUE(GetContents("/proc/meminfo"));
  EXPECT_THAT(proc_meminfo, AllOf(ContainsRegex(R"(MemTotal:\s+[0-9]+ kB)"),
                                  ContainsRegex(R"(MemFree:\s+[0-9]+ kB)")));
}

TEST(ProcSentryMeminfo, ContainsFieldsAndEndsWithNewline) {
  SKIP_IF(!IsRunningOnGvisor());

  std::string proc_sentry_meminfo =
      ASSERT_NO_ERRNO_AND_VALUE(GetContents("/proc/sentry-meminfo"));

  // Assert that all expected fields are present.
  EXPECT_THAT(proc_sentry_meminfo,
              AllOf(ContainsRegex(R"(Alloc:\s+[0-9]+ kB)"),
                    ContainsRegex(R"(TotalAlloc:\s+[0-9]+ kB)"),
                    ContainsRegex(R"(Sys:\s+[0-9]+ kB)"),
                    ContainsRegex(R"(Mallocs:\s+[0-9]+)"),
                    ContainsRegex(R"(Frees:\s+[0-9]+)"),
                    ContainsRegex(R"(Live Objects:\s+[0-9]+)"),
                    ContainsRegex(R"(HeapAlloc:\s+[0-9]+ kB)"),
                    ContainsRegex(R"(HeapSys:\s+[0-9]+ kB)"),
                    ContainsRegex(R"(HeapObjects:\s+[0-9]+)")));

  // Assert that /proc/sentry-meminfo ends with a new line.
  EXPECT_EQ(proc_sentry_meminfo.back(), '\n');
}

TEST(ProcStat, ContainsBasicFields) {
  std::string proc_stat = ASSERT_NO_ERRNO_AND_VALUE(GetContents("/proc/stat"));

  std::vector<std::string> names;
  for (auto const& line : absl::StrSplit(proc_stat, '\n')) {
    std::vector<std::string> fields =
        absl::StrSplit(line, ' ', absl::SkipWhitespace());
    if (fields.empty()) {
      continue;
    }
    names.push_back(fields[0]);
  }

  EXPECT_THAT(names,
              IsSupersetOf({"cpu", "intr", "ctxt", "btime", "processes",
                            "procs_running", "procs_blocked", "softirq"}));
}

TEST(ProcStat, EndsWithNewline) {
  std::string proc_stat = ASSERT_NO_ERRNO_AND_VALUE(GetContents("/proc/stat"));
  EXPECT_EQ(proc_stat.back(), '\n');
}

TEST(ProcStat, Fields) {
  std::string proc_stat = ASSERT_NO_ERRNO_AND_VALUE(GetContents("/proc/stat"));

  std::vector<std::string> names;
  for (auto const& line : absl::StrSplit(proc_stat, '\n')) {
    std::vector<std::string> fields =
        absl::StrSplit(line, ' ', absl::SkipWhitespace());
    if (fields.empty()) {
      continue;
    }

    if (absl::StartsWith(fields[0], "cpu")) {
      // As of Linux 3.11, each CPU entry has 10 fields, plus the name.
      EXPECT_GE(fields.size(), 11) << proc_stat;
    } else if (fields[0] == "ctxt") {
      // Single field.
      EXPECT_EQ(fields.size(), 2) << proc_stat;
    } else if (fields[0] == "btime") {
      // Single field.
      EXPECT_EQ(fields.size(), 2) << proc_stat;
    } else if (fields[0] == "itime") {
      // Single field.
      ASSERT_EQ(fields.size(), 2) << proc_stat;
      // This is the only floating point field.
      double val;
      EXPECT_TRUE(absl::SimpleAtod(fields[1], &val)) << proc_stat;
      continue;
    } else if (fields[0] == "processes") {
      // Single field.
      EXPECT_EQ(fields.size(), 2) << proc_stat;
    } else if (fields[0] == "procs_running") {
      // Single field.
      EXPECT_EQ(fields.size(), 2) << proc_stat;
    } else if (fields[0] == "procs_blocked") {
      // Single field.
      EXPECT_EQ(fields.size(), 2) << proc_stat;
    } else if (fields[0] == "softirq") {
      // As of Linux 3.11, there are 10 softirqs. 12 fields for name + total.
      EXPECT_GE(fields.size(), 12) << proc_stat;
    }

    // All fields besides itime are valid base 10 numbers.
    for (size_t i = 1; i < fields.size(); i++) {
      uint64_t val;
      EXPECT_TRUE(absl::SimpleAtoi(fields[i], &val)) << proc_stat;
    }
  }
}

TEST(ProcLoadavg, EndsWithNewline) {
  std::string proc_loadvg =
      ASSERT_NO_ERRNO_AND_VALUE(GetContents("/proc/loadavg"));
  EXPECT_EQ(proc_loadvg.back(), '\n');
}

TEST(ProcLoadavg, Fields) {
  std::string proc_loadvg =
      ASSERT_NO_ERRNO_AND_VALUE(GetContents("/proc/loadavg"));
  std::vector<std::string> lines = absl::StrSplit(proc_loadvg, '\n');

  // Single line.
  EXPECT_EQ(lines.size(), 2) << proc_loadvg;

  std::vector<std::string> fields =
      absl::StrSplit(lines[0], absl::ByAnyChar(" /"), absl::SkipWhitespace());

  // Six fields.
  EXPECT_EQ(fields.size(), 6) << proc_loadvg;

  double val;
  uint64_t val2;
  // First three fields are floating point numbers.
  EXPECT_TRUE(absl::SimpleAtod(fields[0], &val)) << proc_loadvg;
  EXPECT_TRUE(absl::SimpleAtod(fields[1], &val)) << proc_loadvg;
  EXPECT_TRUE(absl::SimpleAtod(fields[2], &val)) << proc_loadvg;
  // Rest of the fields are valid base 10 numbers.
  EXPECT_TRUE(absl::SimpleAtoi(fields[3], &val2)) << proc_loadvg;
  EXPECT_TRUE(absl::SimpleAtoi(fields[4], &val2)) << proc_loadvg;
  EXPECT_TRUE(absl::SimpleAtoi(fields[5], &val2)) << proc_loadvg;
}

// NOTE: Tests in priority.cc also check certain priority related fields in
// /proc/self/stat.

class ProcPidStatTest : public ::testing::TestWithParam<std::string> {};

// Parses /proc/<pid>/stat output to a vector of string. We need a more
// complicated approach than absl::StrSplit because COMM can contain spaces.
PosixErrorOr<std::vector<std::string>> ParseProcPidStat(
    absl::string_view proc_pid_stat) {
  auto comm_start = proc_pid_stat.find('(');
  auto comm_end = proc_pid_stat.rfind(')');
  if (comm_start == proc_pid_stat.npos || comm_end == proc_pid_stat.npos) {
    return PosixError(EINVAL, absl::StrCat("Invalid /proc/<pid>/stat"));
  }
  std::vector<std::string> fields =
      absl::StrSplit(proc_pid_stat.substr(0, comm_start - 1), ' ');
  fields.push_back(std::string{proc_pid_stat.substr(comm_start, comm_end + 1)});
  absl::c_transform(absl::StrSplit(proc_pid_stat.substr(comm_end + 2), ' '),
                    std::back_inserter(fields),
                    [](auto sv) { return std::string{sv}; });
  return fields;
}

TEST_P(ProcPidStatTest, HasBasicFields) {
  std::string proc_pid_stat = ASSERT_NO_ERRNO_AND_VALUE(
      GetContents(absl::StrCat("/proc/", GetParam(), "/stat")));

  ASSERT_FALSE(proc_pid_stat.empty());
  std::vector<std::string> fields =
      ASSERT_NO_ERRNO_AND_VALUE(ParseProcPidStat(proc_pid_stat));

  ASSERT_GE(fields.size(), 24);
  EXPECT_EQ(absl::StrCat(getpid()), fields[0]);
  // fields[1] is the thread name.
  EXPECT_EQ("R", fields[2]);  // task state
  EXPECT_EQ(absl::StrCat(getppid()), fields[3]);

  // If the test starts up quickly, then the process start time and the kernel
  // boot time will be very close, and the proc starttime field (which is the
  // delta of the two times) will be 0.  For that unfortunate reason, we can
  // only check that starttime >= 0, and not that it is strictly > 0.
  uint64_t starttime;
  ASSERT_TRUE(absl::SimpleAtoi(fields[21], &starttime));
  EXPECT_GE(starttime, 0);

  uint64_t vss;
  ASSERT_TRUE(absl::SimpleAtoi(fields[22], &vss));
  EXPECT_GT(vss, 0);

  uint64_t rss;
  ASSERT_TRUE(absl::SimpleAtoi(fields[23], &rss));
  EXPECT_GT(rss, 0);

  uint64_t rsslim;
  ASSERT_TRUE(absl::SimpleAtoi(fields[24], &rsslim));
  EXPECT_GT(rsslim, 0);
}

INSTANTIATE_TEST_SUITE_P(SelfAndNumericPid, ProcPidStatTest,
                         ::testing::Values("self", absl::StrCat(getpid())));

using ProcPidStatmTest = ::testing::TestWithParam<std::string>;

TEST_P(ProcPidStatmTest, HasBasicFields) {
  std::string proc_pid_statm = ASSERT_NO_ERRNO_AND_VALUE(
      GetContents(absl::StrCat("/proc/", GetParam(), "/statm")));
  ASSERT_FALSE(proc_pid_statm.empty());
  std::vector<std::string> fields = absl::StrSplit(proc_pid_statm, ' ');
  ASSERT_GE(fields.size(), 7);

  uint64_t vss;
  ASSERT_TRUE(absl::SimpleAtoi(fields[0], &vss));
  EXPECT_GT(vss, 0);

  uint64_t rss;
  ASSERT_TRUE(absl::SimpleAtoi(fields[1], &rss));
  EXPECT_GT(rss, 0);
}

INSTANTIATE_TEST_SUITE_P(SelfAndNumericPid, ProcPidStatmTest,
                         ::testing::Values("self", absl::StrCat(getpid())));

PosixErrorOr<uint64_t> CurrentRSS() {
  ASSIGN_OR_RETURN_ERRNO(auto proc_self_stat, GetContents("/proc/self/stat"));
  if (proc_self_stat.empty()) {
    return PosixError(EINVAL, "empty /proc/self/stat");
  }

  ASSIGN_OR_RETURN_ERRNO(std::vector<std::string> fields,
                         ParseProcPidStat(proc_self_stat));
  if (fields.size() < 24) {
    return PosixError(
        EINVAL,
        absl::StrCat("/proc/self/stat has too few fields: ", proc_self_stat));
  }

  uint64_t rss;
  if (!absl::SimpleAtoi(fields[23], &rss)) {
    return PosixError(
        EINVAL, absl::StrCat("/proc/self/stat RSS field is not a number: ",
                             fields[23]));
  }

  // RSS is given in number of pages.
  return rss * kPageSize;
}

// The size of mapping created by MapPopulateRSS.
constexpr uint64_t kMappingSize = 100 << 20;

// Tolerance on RSS comparisons to account for background thread mappings,
// reclaimed pages, newly faulted pages, etc.
constexpr uint64_t kRSSTolerance = 10 << 20;

// Capture RSS before and after an anonymous mapping with passed prot.
void MapPopulateRSS(int prot, uint64_t* before, uint64_t* after) {
  *before = ASSERT_NO_ERRNO_AND_VALUE(CurrentRSS());

  // N.B. The kernel asynchronously accumulates per-task RSS counters into the
  // mm RSS, which is exposed by /proc/PID/stat. Task exit is a synchronization
  // point (kernel/exit.c:do_exit -> sync_mm_rss), so perform the mapping on
  // another thread to ensure it is reflected in RSS after the thread exits.
  Mapping mapping;
  ScopedThread t([&mapping, prot] {
    mapping = ASSERT_NO_ERRNO_AND_VALUE(
        MmapAnon(kMappingSize, prot, MAP_PRIVATE | MAP_POPULATE));
  });
  t.Join();

  *after = ASSERT_NO_ERRNO_AND_VALUE(CurrentRSS());
}

// TODO(b/73896574): Test for PROT_READ + MAP_POPULATE anonymous mappings. Their
// semantics are more subtle:
//
// Small pages -> Zero page mapped, not counted in RSS
// (mm/memory.c:do_anonymous_page).
//
// Huge pages (THP enabled, use_zero_page=0) -> Pages committed
// (mm/memory.c:__handle_mm_fault -> create_huge_pmd).
//
// Huge pages (THP enabled, use_zero_page=1) -> Zero page mapped, not counted in
// RSS (mm/huge_memory.c:do_huge_pmd_anonymous_page).

// PROT_WRITE + MAP_POPULATE anonymous mappings are always committed.
TEST(ProcSelfStat, PopulateWriteRSS) {
  uint64_t before, after;
  MapPopulateRSS(PROT_READ | PROT_WRITE, &before, &after);

  // Mapping is committed.
  EXPECT_NEAR(before + kMappingSize, after, kRSSTolerance);
}

// PROT_NONE + MAP_POPULATE anonymous mappings are never committed.
TEST(ProcSelfStat, PopulateNoneRSS) {
  uint64_t before, after;
  MapPopulateRSS(PROT_NONE, &before, &after);

  // Mapping not committed.
  EXPECT_NEAR(before, after, kRSSTolerance);
}

// Returns the calling thread's name.
PosixErrorOr<std::string> ThreadName() {
  // "The buffer should allow space for up to 16 bytes; the returned std::string
  // will be null-terminated if it is shorter than that." - prctl(2). But we
  // always want the thread name to be null-terminated.
  char thread_name[17];
  int rc = prctl(PR_GET_NAME, thread_name, 0, 0, 0);
  MaybeSave();
  if (rc < 0) {
    return PosixError(errno, "prctl(PR_GET_NAME)");
  }
  thread_name[16] = '\0';
  return std::string(thread_name);
}

// Parses the contents of a /proc/[pid]/status file into a collection of
// key-value pairs.
PosixErrorOr<absl::btree_map<std::string, std::string>> ParseProcStatus(
    absl::string_view status_str) {
  absl::btree_map<std::string, std::string> fields;
  for (absl::string_view const line :
       absl::StrSplit(status_str, '\n', absl::SkipWhitespace())) {
    const std::pair<absl::string_view, absl::string_view> kv =
        absl::StrSplit(line, absl::MaxSplits(":\t", 1));
    if (kv.first.empty()) {
      return PosixError(
          EINVAL, absl::StrCat("failed to parse key in line \"", line, "\""));
    }
    std::string key(kv.first);
    if (fields.count(key)) {
      return PosixError(EINVAL,
                        absl::StrCat("duplicate key \"", kv.first, "\""));
    }
    std::string value(kv.second);
    absl::StripLeadingAsciiWhitespace(&value);
    fields.emplace(std::move(key), std::move(value));
  }
  return fields;
}

TEST(ParseProcStatusTest, ParsesSimpleStatusFileWithMixedWhitespaceCorrectly) {
  EXPECT_THAT(
      ParseProcStatus(
          "Name:\tinit\nState:\tS (sleeping)\nCapEff:\t 0000001fffffffff\n"),
      IsPosixErrorOkAndHolds(UnorderedElementsAre(
          Pair("Name", "init"), Pair("State", "S (sleeping)"),
          Pair("CapEff", "0000001fffffffff"))));
}

TEST(ParseProcStatusTest, DetectsDuplicateKeys) {
  auto proc_status_or = ParseProcStatus("Name:\tfoo\nName:\tfoo\n");
  EXPECT_THAT(proc_status_or,
              PosixErrorIs(EINVAL, ::testing::StrEq("duplicate key \"Name\"")));
}

TEST(ParseProcStatusTest, DetectsMissingTabs) {
  EXPECT_THAT(ParseProcStatus("Name:foo\nPid: 1\n"),
              IsPosixErrorOkAndHolds(UnorderedElementsAre(Pair("Name:foo", ""),
                                                          Pair("Pid: 1", ""))));
}

TEST(ProcPidStatusTest, HasBasicFields) {
  // Do this on a separate thread since we want tgid != tid.
  ScopedThread([] {
    const pid_t tgid = getpid();
    const pid_t tid = syscall(SYS_gettid);
    EXPECT_NE(tgid, tid);
    const auto thread_name = ASSERT_NO_ERRNO_AND_VALUE(ThreadName());

    std::string status_str = ASSERT_NO_ERRNO_AND_VALUE(
        GetContents(absl::StrCat("/proc/", tid, "/status")));

    ASSERT_FALSE(status_str.empty());
    const auto status = ASSERT_NO_ERRNO_AND_VALUE(ParseProcStatus(status_str));
    EXPECT_THAT(status, IsSupersetOf({
                            Pair("Name", thread_name),
                            Pair("Tgid", absl::StrCat(tgid)),
                            Pair("Pid", absl::StrCat(tid)),
                            Pair("PPid", absl::StrCat(getppid())),
                        }));

    uid_t ruid, euid, suid;
    ASSERT_THAT(getresuid(&ruid, &euid, &suid), SyscallSucceeds());
    gid_t rgid, egid, sgid;
    ASSERT_THAT(getresgid(&rgid, &egid, &sgid), SyscallSucceeds());
    std::vector<gid_t> supplementary_gids;
    int ngids = getgroups(0, nullptr);
    supplementary_gids.resize(ngids);
    ASSERT_THAT(getgroups(ngids, supplementary_gids.data()), SyscallSucceeds());

    EXPECT_THAT(
        status,
        IsSupersetOf(std::vector<
                     ::testing::Matcher<std::pair<std::string, std::string>>>{
            // gVisor doesn't support fsuid/gid, and even if it did there is
            // no getfsuid/getfsgid().
            Pair("Uid",
                 StartsWith(absl::StrFormat("%d\t%d\t%d\t", ruid, euid, suid))),
            Pair("Gid",
                 StartsWith(absl::StrFormat("%d\t%d\t%d\t", rgid, egid, sgid))),
            // ParseProcStatus strips leading whitespace for each value,
            // so if the Groups line is empty then the trailing space is
            // stripped.
            Pair("Groups", StartsWith(absl::StrJoin(supplementary_gids, " "))),
        }));
  });
}

TEST(ProcPidStatusTest, StateRunning) {
  // Task must be running when reading the file.
  const pid_t tid = syscall(SYS_gettid);
  std::string status_str = ASSERT_NO_ERRNO_AND_VALUE(
      GetContents(absl::StrCat("/proc/", tid, "/status")));

  EXPECT_THAT(ParseProcStatus(status_str),
              IsPosixErrorOkAndHolds(Contains(Pair("State", "R (running)"))));
}

TEST(ProcPidStatusTest, StateSleeping) {
  // Starts a child process that blocks and checks that State is sleeping.
  auto res = WithSubprocess(
      [&](int pid) -> PosixError {
        // Because this test is timing based we will disable cooperative saving
        // and the test itself also has random saving disabled.
        const DisableSave ds;
        // Try multiple times in case the child isn't sleeping when status file
        // is read.
        MonotonicTimer timer;
        timer.Start();
        for (;;) {
          ASSIGN_OR_RETURN_ERRNO(
              std::string status_str,
              GetContents(absl::StrCat("/proc/", pid, "/status")));
          ASSIGN_OR_RETURN_ERRNO(auto map, ParseProcStatus(status_str));
          if (map["State"] == std::string("S (sleeping)")) {
            // Test passed!
            return NoError();
          }
          if (timer.Duration() > absl::Seconds(10)) {
            return PosixError(ETIMEDOUT, "Timeout waiting for child to sleep");
          }
          absl::SleepFor(absl::Milliseconds(10));
        }
      },
      nullptr, nullptr);
  ASSERT_NO_ERRNO(res);
}

TEST(ProcPidStatusTest, ValuesAreTabDelimited) {
  std::string status_str =
      ASSERT_NO_ERRNO_AND_VALUE(GetContents("/proc/self/status"));
  ASSERT_FALSE(status_str.empty());
  for (absl::string_view const line :
       absl::StrSplit(status_str, '\n', absl::SkipWhitespace())) {
    EXPECT_NE(std::string::npos, line.find(":\t"));
  }
}

// Threads properly counts running threads.
//
// TODO(mpratt): Test zombied threads while the thread group leader is still
// running with generalized fork and clone children from the wait test.
TEST(ProcPidStatusTest, Threads) {
  char buf[4096] = {};
  EXPECT_THAT(ReadWhileRunning("status", buf, sizeof(buf) - 1),
              SyscallSucceedsWithValue(Gt(0)));

  auto status = ASSERT_NO_ERRNO_AND_VALUE(ParseProcStatus(buf));
  auto it = status.find("Threads");
  ASSERT_NE(it, status.end());
  int threads = -1;
  EXPECT_TRUE(absl::SimpleAtoi(it->second, &threads))
      << "Threads value " << it->second << " is not a number";
  // Don't make assumptions about the exact number of threads, as it may not be
  // constant.
  EXPECT_GE(threads, 1);

  memset(buf, 0, sizeof(buf));
  EXPECT_THAT(ReadWhileZombied("status", buf, sizeof(buf) - 1),
              SyscallSucceedsWithValue(Gt(0)));

  status = ASSERT_NO_ERRNO_AND_VALUE(ParseProcStatus(buf));
  it = status.find("Threads");
  ASSERT_NE(it, status.end());
  threads = -1;
  EXPECT_TRUE(absl::SimpleAtoi(it->second, &threads))
      << "Threads value " << it->second << " is not a number";
  // There must be only the thread group leader remaining, zombied.
  EXPECT_EQ(threads, 1);
}

// Returns true if all characters in s are digits.
bool IsDigits(absl::string_view s) {
  return std::all_of(s.begin(), s.end(), absl::ascii_isdigit);
}

TEST(ProcPidStatTest, VmStats) {
  std::string status_str =
      ASSERT_NO_ERRNO_AND_VALUE(GetContents("/proc/self/status"));
  ASSERT_FALSE(status_str.empty());
  auto status = ASSERT_NO_ERRNO_AND_VALUE(ParseProcStatus(status_str));

  const auto vss_it = status.find("VmSize");
  ASSERT_NE(vss_it, status.end());

  absl::string_view vss_str(vss_it->second);

  // Room for the " kB" suffix plus at least one digit.
  ASSERT_GT(vss_str.length(), 3);
  EXPECT_TRUE(absl::EndsWith(vss_str, " kB"));
  // Everything else is part of a number.
  EXPECT_TRUE(IsDigits(vss_str.substr(0, vss_str.length() - 3))) << vss_str;
  // ... which is not 0.
  EXPECT_NE('0', vss_str[0]);

  const auto rss_it = status.find("VmRSS");
  ASSERT_NE(rss_it, status.end());

  absl::string_view rss_str(rss_it->second);

  // Room for the " kB" suffix plus at least one digit.
  ASSERT_GT(rss_str.length(), 3);
  EXPECT_TRUE(absl::EndsWith(rss_str, " kB"));
  // Everything else is part of a number.
  EXPECT_TRUE(IsDigits(rss_str.substr(0, rss_str.length() - 3))) << rss_str;
  // ... which is not 0.
  EXPECT_NE('0', rss_str[0]);

  const auto data_it = status.find("VmData");
  ASSERT_NE(data_it, status.end());

  absl::string_view data_str(data_it->second);

  // Room for the " kB" suffix plus at least one digit.
  ASSERT_GT(data_str.length(), 3);
  EXPECT_TRUE(absl::EndsWith(data_str, " kB"));
  // Everything else is part of a number.
  EXPECT_TRUE(IsDigits(data_str.substr(0, data_str.length() - 3))) << data_str;
  // ... which is not 0.
  EXPECT_NE('0', data_str[0]);
}

// Parse an array of NUL-terminated char* arrays, returning a vector of
// strings.
std::vector<std::string> ParseNulTerminatedStrings(std::string contents) {
  EXPECT_EQ('\0', contents.back());
  // The split will leave an empty string if the NUL-byte remains, so pop
  // it.
  contents.pop_back();

  return absl::StrSplit(contents, '\0');
}

TEST(ProcPidCmdline, MatchesArgv) {
  std::vector<std::string> proc_cmdline = ParseNulTerminatedStrings(
      ASSERT_NO_ERRNO_AND_VALUE(GetContents("/proc/self/cmdline")));
  EXPECT_THAT(saved_argv, ContainerEq(proc_cmdline));
}

TEST(ProcPidEnviron, MatchesEnviron) {
  std::vector<std::string> proc_environ = ParseNulTerminatedStrings(
      ASSERT_NO_ERRNO_AND_VALUE(GetContents("/proc/self/environ")));
  // Get the environment from the environ variable, which we will compare with
  // /proc/self/environ.
  std::vector<std::string> env;
  for (char** v = environ; *v; v++) {
    env.push_back(*v);
  }
  EXPECT_THAT(env, ContainerEq(proc_environ));
}

TEST(ProcPidCmdline, SubprocessForkSameCmdline) {
  std::vector<std::string> proc_cmdline_parent;
  std::vector<std::string> proc_cmdline;
  proc_cmdline_parent = ParseNulTerminatedStrings(
      ASSERT_NO_ERRNO_AND_VALUE(GetContents("/proc/self/cmdline")));
  auto res = WithSubprocess(
      [&](int pid) -> PosixError {
        ASSIGN_OR_RETURN_ERRNO(
            auto raw_cmdline,
            GetContents(absl::StrCat("/proc/", pid, "/cmdline")));
        proc_cmdline = ParseNulTerminatedStrings(raw_cmdline);
        return NoError();
      },
      nullptr, nullptr);
  ASSERT_NO_ERRNO(res);

  for (size_t i = 0; i < proc_cmdline_parent.size(); i++) {
    EXPECT_EQ(proc_cmdline_parent[i], proc_cmdline[i]);
  }
}

TEST(ProcPidCmdline, SubprocessSeekCmdline) {
  FileDescriptor fd;
  ASSERT_NO_ERRNO(WithSubprocess(
      [&](int pid) -> PosixError {
        // Running. Open /proc/pid/cmdline.
        ASSIGN_OR_RETURN_ERRNO(
            fd, Open(absl::StrCat("/proc/", pid, "/cmdline"), O_RDONLY));
        return NoError();
      },
      [&](int pid) -> PosixError {
        // Zombie, but seek should still succeed.
        int ret = lseek(fd.get(), 0x801, 0);
        if (ret < 0) {
          return PosixError(errno);
        }
        return NoError();
      },
      [&](int pid) -> PosixError {
        // Exited.
        int ret = lseek(fd.get(), 0x801, 0);
        if (ret < 0) {
          return PosixError(errno);
        }
        return NoError();
      }));
}

// Test whether /proc/PID/ symlinks can be read for a running process.
TEST(ProcPidSymlink, SubprocessRunning) {
  char buf[1];

  EXPECT_THAT(ReadlinkWhileRunning("exe", buf, sizeof(buf)),
              SyscallSucceedsWithValue(sizeof(buf)));

  EXPECT_THAT(ReadlinkWhileRunning("ns/net", buf, sizeof(buf)),
              SyscallSucceedsWithValue(sizeof(buf)));

  EXPECT_THAT(ReadlinkWhileRunning("ns/pid", buf, sizeof(buf)),
              SyscallSucceedsWithValue(sizeof(buf)));

  EXPECT_THAT(ReadlinkWhileRunning("ns/user", buf, sizeof(buf)),
              SyscallSucceedsWithValue(sizeof(buf)));
}

TEST(ProcPidSymlink, SubprocessZombied) {
  AutoCapability cap1(CAP_DAC_OVERRIDE, false);
  AutoCapability cap2(CAP_DAC_READ_SEARCH, false);

  char buf[1];

  int want = EACCES;
  if (!IsRunningOnGvisor()) {
    auto version = ASSERT_NO_ERRNO_AND_VALUE(GetKernelVersion());
    if (version.major > 4 || (version.major == 4 && version.minor > 3)) {
      want = ENOENT;
    }
  }

  EXPECT_THAT(ReadlinkWhileZombied("exe", buf, sizeof(buf)),
              SyscallFailsWithErrno(want));

  if (!IsRunningOnGvisor()) {
    EXPECT_THAT(ReadlinkWhileZombied("ns/net", buf, sizeof(buf)),
                SyscallFailsWithErrno(want));
  }

  // FIXME(gvisor.dev/issue/164): Inconsistent behavior between linux on proc
  // files.
  //
  // ~4.3: Syscall fails with EACCES.
  // 4.17: Syscall succeeds and returns 1.
  //
  if (!IsRunningOnGvisor()) {
    return;
  }

  EXPECT_THAT(ReadlinkWhileZombied("ns/pid", buf, sizeof(buf)),
              SyscallFailsWithErrno(want));

  EXPECT_THAT(ReadlinkWhileZombied("ns/user", buf, sizeof(buf)),
              SyscallFailsWithErrno(want));
}

// Test whether /proc/PID/ symlinks can be read for an exited process.
TEST(ProcPidSymlink, SubprocessExited) {
  char buf[1];

  EXPECT_THAT(ReadlinkWhileExited("exe", buf, sizeof(buf)),
              SyscallFailsWithErrno(ESRCH));

  EXPECT_THAT(ReadlinkWhileExited("ns/net", buf, sizeof(buf)),
              SyscallFailsWithErrno(ESRCH));

  EXPECT_THAT(ReadlinkWhileExited("ns/pid", buf, sizeof(buf)),
              SyscallFailsWithErrno(ESRCH));

  EXPECT_THAT(ReadlinkWhileExited("ns/user", buf, sizeof(buf)),
              SyscallFailsWithErrno(ESRCH));
}

// /proc/PID/exe points to the correct binary.
TEST(ProcPidExe, Subprocess) {
  auto link = ASSERT_NO_ERRNO_AND_VALUE(ReadLink("/proc/self/exe"));
  auto expected_absolute_path =
      ASSERT_NO_ERRNO_AND_VALUE(MakeAbsolute(link, ""));

  char actual[PATH_MAX + 1] = {};
  ASSERT_THAT(ReadlinkWhileRunning("exe", actual, sizeof(actual)),
              SyscallSucceedsWithValue(Gt(0)));
  EXPECT_EQ(actual, expected_absolute_path);
}

// /proc/PID/cwd points to the correct directory.
TEST(ProcPidCwd, Subprocess) {
  auto want = ASSERT_NO_ERRNO_AND_VALUE(GetCWD());

  char got[PATH_MAX + 1] = {};
  ASSERT_THAT(ReadlinkWhileRunning("cwd", got, sizeof(got)),
              SyscallSucceedsWithValue(Gt(0)));
  EXPECT_EQ(got, want);
}

// /proc/PID/root points to the correct directory.
TEST(ProcPidRoot, Subprocess) {
  char got[PATH_MAX + 1] = {};
  ASSERT_THAT(ReadlinkWhileRunning("root", got, sizeof(got)),
              SyscallSucceedsWithValue(Gt(0)));
  EXPECT_STREQ(got, "/");
}

// Test whether /proc/PID/ files can be read for a running process.
TEST(ProcPidFile, SubprocessRunning) {
  char buf[1];

  EXPECT_THAT(ReadWhileRunning("auxv", buf, sizeof(buf)),
              SyscallSucceedsWithValue(sizeof(buf)));

  EXPECT_THAT(ReadWhileRunning("cmdline", buf, sizeof(buf)),
              SyscallSucceedsWithValue(sizeof(buf)));

  EXPECT_THAT(ReadWhileRunning("comm", buf, sizeof(buf)),
              SyscallSucceedsWithValue(sizeof(buf)));

  EXPECT_THAT(ReadWhileRunning("gid_map", buf, sizeof(buf)),
              SyscallSucceedsWithValue(sizeof(buf)));

  EXPECT_THAT(ReadWhileRunning("io", buf, sizeof(buf)),
              SyscallSucceedsWithValue(sizeof(buf)));

  EXPECT_THAT(ReadWhileRunning("maps", buf, sizeof(buf)),
              SyscallSucceedsWithValue(sizeof(buf)));

  EXPECT_THAT(ReadWhileRunning("stat", buf, sizeof(buf)),
              SyscallSucceedsWithValue(sizeof(buf)));

  EXPECT_THAT(ReadWhileRunning("status", buf, sizeof(buf)),
              SyscallSucceedsWithValue(sizeof(buf)));

  EXPECT_THAT(ReadWhileRunning("uid_map", buf, sizeof(buf)),
              SyscallSucceedsWithValue(sizeof(buf)));

  EXPECT_THAT(ReadWhileRunning("oom_score", buf, sizeof(buf)),
              SyscallSucceedsWithValue(sizeof(buf)));

  EXPECT_THAT(ReadWhileRunning("oom_score_adj", buf, sizeof(buf)),
              SyscallSucceedsWithValue(sizeof(buf)));
}

// Test whether /proc/PID/ files can be read for a zombie process.
TEST(ProcPidFile, SubprocessZombie) {
  char buf[1];

  // FIXME(gvisor.dev/issue/164): Loosen requirement due to inconsistent
  // behavior on different kernels.
  //
  // ~4.3: Succeeds and returns 0.
  // 4.17: Succeeds and returns 1.
  // gVisor: Succeeds and returns 0.
  EXPECT_THAT(ReadWhileZombied("auxv", buf, sizeof(buf)), SyscallSucceeds());

  EXPECT_THAT(ReadWhileZombied("cmdline", buf, sizeof(buf)),
              SyscallSucceedsWithValue(0));

  EXPECT_THAT(ReadWhileZombied("comm", buf, sizeof(buf)),
              SyscallSucceedsWithValue(sizeof(buf)));

  EXPECT_THAT(ReadWhileZombied("gid_map", buf, sizeof(buf)),
              SyscallSucceedsWithValue(sizeof(buf)));

  EXPECT_THAT(ReadWhileZombied("maps", buf, sizeof(buf)),
              SyscallSucceedsWithValue(0));

  EXPECT_THAT(ReadWhileZombied("stat", buf, sizeof(buf)),
              SyscallSucceedsWithValue(sizeof(buf)));

  EXPECT_THAT(ReadWhileZombied("status", buf, sizeof(buf)),
              SyscallSucceedsWithValue(sizeof(buf)));

  EXPECT_THAT(ReadWhileZombied("uid_map", buf, sizeof(buf)),
              SyscallSucceedsWithValue(sizeof(buf)));

  EXPECT_THAT(ReadWhileZombied("oom_score", buf, sizeof(buf)),
              SyscallSucceedsWithValue(sizeof(buf)));

  EXPECT_THAT(ReadWhileZombied("oom_score_adj", buf, sizeof(buf)),
              SyscallSucceedsWithValue(sizeof(buf)));

  // FIXME(gvisor.dev/issue/164): Inconsistent behavior between gVisor and linux
  // on proc files.
  //
  // ~4.3: Fails and returns EACCES.
  // gVisor & 4.17: Succeeds and returns 1.
  //
  // EXPECT_THAT(ReadWhileZombied("io", buf, sizeof(buf)),
  //          SyscallFailsWithErrno(EACCES));
}

// Test whether /proc/PID/ files can be read for an exited process.
TEST(ProcPidFile, SubprocessExited) {
  char buf[1];

  // FIXME(gvisor.dev/issue/164): Inconsistent behavior between kernels.
  //
  // ~4.3: Fails and returns ESRCH.
  // gVisor: Fails with ESRCH.
  // 4.17: Succeeds and returns 1.
  //
  // EXPECT_THAT(ReadWhileExited("auxv", buf, sizeof(buf)),
  //            SyscallFailsWithErrno(ESRCH));

  EXPECT_THAT(ReadWhileExited("cmdline", buf, sizeof(buf)),
              SyscallFailsWithErrno(ESRCH));

  if (!IsRunningOnGvisor()) {
    // FIXME(gvisor.dev/issue/164): Succeeds on gVisor.
    EXPECT_THAT(ReadWhileExited("comm", buf, sizeof(buf)),
                SyscallFailsWithErrno(ESRCH));
  }

  EXPECT_THAT(ReadWhileExited("gid_map", buf, sizeof(buf)),
              SyscallSucceedsWithValue(sizeof(buf)));

  if (!IsRunningOnGvisor()) {
    // FIXME(gvisor.dev/issue/164): Succeeds on gVisor.
    EXPECT_THAT(ReadWhileExited("io", buf, sizeof(buf)),
                SyscallFailsWithErrno(ESRCH));
  }

  if (!IsRunningOnGvisor()) {
    // FIXME(gvisor.dev/issue/164): Returns EOF on gVisor.
    EXPECT_THAT(ReadWhileExited("maps", buf, sizeof(buf)),
                SyscallFailsWithErrno(ESRCH));
  }

  if (!IsRunningOnGvisor()) {
    // FIXME(gvisor.dev/issue/164): Succeeds on gVisor.
    EXPECT_THAT(ReadWhileExited("stat", buf, sizeof(buf)),
                SyscallFailsWithErrno(ESRCH));
  }

  if (!IsRunningOnGvisor()) {
    // FIXME(gvisor.dev/issue/164): Succeeds on gVisor.
    EXPECT_THAT(ReadWhileExited("status", buf, sizeof(buf)),
                SyscallFailsWithErrno(ESRCH));
  }

  EXPECT_THAT(ReadWhileExited("uid_map", buf, sizeof(buf)),
              SyscallSucceedsWithValue(sizeof(buf)));

  if (!IsRunningOnGvisor()) {
    // FIXME(gvisor.dev/issue/164): Succeeds on gVisor.
    EXPECT_THAT(ReadWhileExited("oom_score", buf, sizeof(buf)),
                SyscallFailsWithErrno(ESRCH));
  }

  EXPECT_THAT(ReadWhileExited("oom_score_adj", buf, sizeof(buf)),
              SyscallFailsWithErrno(ESRCH));
}

PosixError DirContains(absl::string_view path,
                       const std::vector<std::string>& expect,
                       const std::vector<std::string>& exclude) {
  ASSIGN_OR_RETURN_ERRNO(auto listing, ListDir(path, false));

  for (auto& expected_entry : expect) {
    auto cursor = std::find(listing.begin(), listing.end(), expected_entry);
    if (cursor == listing.end()) {
      return PosixError(
          ENOENT,
          absl::StrCat("Failed to find one or more paths in '", path, "'"));
    }
  }
  for (auto& excluded_entry : exclude) {
    auto cursor = std::find(listing.begin(), listing.end(), excluded_entry);
    if (cursor != listing.end()) {
      return PosixError(ENOENT, absl::StrCat("File '", excluded_entry,
                                             "' found in path '", path, "'"));
    }
  }
  return NoError();
}

PosixError EventuallyDirContains(absl::string_view path,
                                 const std::vector<std::string>& expect,
                                 const std::vector<std::string>& exclude) {
  constexpr int kRetryCount = 100;
  const absl::Duration kRetryDelay = absl::Milliseconds(100);

  for (int i = 0; i < kRetryCount; ++i) {
    auto res = DirContains(path, expect, exclude);
    if (res.ok()) {
      return res;
    } else if (i < kRetryCount - 1) {
      // Sleep if this isn't the final iteration.
      absl::SleepFor(kRetryDelay);
    }
  }
  return PosixError(ETIMEDOUT,
                    "Timed out while waiting for directory to contain files ");
}

std::vector<std::string> TaskFiles(const std::vector<pid_t>& pids) {
  return ApplyVec<std::string>([](const pid_t p) { return absl::StrCat(p); },
                               pids);
}

TEST(ProcTask, Basic) {
  EXPECT_NO_ERRNO(
      DirContains("/proc/self/task", {".", "..", absl::StrCat(getpid())}, {}));
}

// Helper class for creating a new task in the current thread group.
class BlockingChild {
 public:
  BlockingChild() : thread_([=] { Start(); }) {}
  ~BlockingChild() { Join(); }

  pid_t Tid() const {
    absl::MutexLock ml(&mu_);
    mu_.Await(absl::Condition(&tid_ready_));
    return tid_;
  }

  void Join() {
    {
      absl::MutexLock ml(&mu_);
      stop_ = true;
    }
    thread_.Join();
  }

 private:
  void Start() {
    absl::MutexLock ml(&mu_);
    tid_ = syscall(__NR_gettid);
    tid_ready_ = true;
    mu_.Await(absl::Condition(&stop_));
  }

  mutable absl::Mutex mu_;
  bool stop_ ABSL_GUARDED_BY(mu_) = false;
  pid_t tid_;
  bool tid_ready_ ABSL_GUARDED_BY(mu_) = false;

  // Must be last to ensure that the destructor for the thread is run before
  // any other member of the object is destroyed.
  ScopedThread thread_;
};

TEST(ProcTask, NewThreadAppears) {
  BlockingChild child1;
  EXPECT_NO_ERRNO(
      DirContains("/proc/self/task", TaskFiles({child1.Tid()}), {}));
}

TEST(ProcTask, KilledThreadsDisappear) {
  BlockingChild child1;
  EXPECT_NO_ERRNO(
      DirContains("/proc/self/task", TaskFiles({child1.Tid()}), {}));

  // Stat child1's task file. Regression test for b/32097707.
  struct stat statbuf;
  const std::string child1_task_file =
      absl::StrCat("/proc/self/task/", child1.Tid());
  EXPECT_THAT(stat(child1_task_file.c_str(), &statbuf), SyscallSucceeds());

  BlockingChild child2;
  EXPECT_NO_ERRNO(DirContains("/proc/self/task",
                              TaskFiles({child1.Tid(), child2.Tid()}), {}));

  BlockingChild child3;
  BlockingChild child4;
  BlockingChild child5;
  EXPECT_NO_ERRNO(
      DirContains("/proc/self/task",
                  TaskFiles({child1.Tid(), child2.Tid(), child3.Tid(),
                             child4.Tid(), child5.Tid()}),
                  {}));

  child2.Join();
  EXPECT_NO_ERRNO(EventuallyDirContains(
      "/proc/self/task",
      TaskFiles({child1.Tid(), child3.Tid(), child4.Tid(), child5.Tid()}),
      TaskFiles({child2.Tid()})));

  child1.Join();
  child4.Join();
  EXPECT_NO_ERRNO(EventuallyDirContains(
      "/proc/self/task", TaskFiles({child3.Tid(), child5.Tid()}),
      TaskFiles({child2.Tid(), child1.Tid(), child4.Tid()})));

  // Stat child1's task file again.  This time it should fail. See b/32097707.
  EXPECT_THAT(stat(child1_task_file.c_str(), &statbuf),
              SyscallFailsWithErrno(ENOENT));

  child3.Join();
  child5.Join();
  EXPECT_NO_ERRNO(
      EventuallyDirContains("/proc/self/task", {},
                            TaskFiles({child2.Tid(), child1.Tid(), child4.Tid(),
                                       child3.Tid(), child5.Tid()})));
}

TEST(ProcTask, ChildTaskDir) {
  BlockingChild child1;
  EXPECT_NO_ERRNO(
      DirContains("/proc/self/task", TaskFiles({child1.Tid()}), {}));
  EXPECT_NO_ERRNO(DirContains(absl::StrCat("/proc/", child1.Tid(), "/task"),
                              TaskFiles({child1.Tid()}), {}));
}

PosixError VerifyPidDir(std::string path) {
  return DirContains(path, {"exe", "fd", "io", "maps", "ns", "stat", "status"},
                     {});
}

TEST(ProcTask, VerifyTaskDir) {
  EXPECT_NO_ERRNO(VerifyPidDir("/proc/self"));

  EXPECT_NO_ERRNO(VerifyPidDir(absl::StrCat("/proc/self/task/", getpid())));
  BlockingChild child1;
  EXPECT_NO_ERRNO(VerifyPidDir(absl::StrCat("/proc/self/task/", child1.Tid())));

  // Only the first level of task directories should contain the 'task'
  // directory. That is:
  //
  // /proc/1234/task           <- should exist
  // /proc/1234/task/1234/task <- should not exist
  // /proc/1234/task/1235/task <- should not exist (where 1235 is in the same
  //                                                thread group as 1234).
  EXPECT_NO_ERRNO(
      DirContains(absl::StrCat("/proc/self/task/", getpid()), {}, {"task"}));
}

TEST(ProcTask, VerifyTaskChildren) {
  auto path = JoinPath("/proc", absl::StrCat(getpid()), "task",
                       absl::StrCat(gettid()), "children");
  EXPECT_THAT(access(path.c_str(), F_OK), SyscallSucceeds());

  int pid1 = -1, status1 = -1;
  auto cleanup1 =
      ForkAndExec("/bin/sleep", {"sleep", "100"}, {}, nullptr, &pid1, &status1);
  ASSERT_GT(pid1, 0);
  ASSERT_EQ(status1, 0);

  auto proc_children_file = ASSERT_NO_ERRNO_AND_VALUE(GetContents(path));
  EXPECT_EQ(absl::StrCat(pid1, " "), proc_children_file);

  int pid2 = -1, status2 = -1;
  auto cleanup2 =
      ForkAndExec("/bin/sleep", {"sleep", "100"}, {}, nullptr, &pid2, &status2);
  ASSERT_GT(pid2, 0);
  ASSERT_EQ(status2, 0);

  proc_children_file = ASSERT_NO_ERRNO_AND_VALUE(GetContents(path));

  // /children contains space-separated sorted list of thread Ids of children.
  std::string expectedContent;
  if (pid1 < pid2) {
    expectedContent = absl::StrCat(pid1, " ", pid2, " ");
  } else {
    expectedContent = absl::StrCat(pid2, " ", pid1, " ");
  }
  EXPECT_EQ(expectedContent, proc_children_file);
}

TEST(ProcTask, TaskDirCannotBeDeleted) {
  // Drop capabilities that allow us to override file and directory permissions.
  AutoCapability cap(CAP_DAC_OVERRIDE, false);

  EXPECT_THAT(rmdir("/proc/self/task"), SyscallFails());
  EXPECT_THAT(rmdir(absl::StrCat("/proc/self/task/", getpid()).c_str()),
              SyscallFailsWithErrno(EACCES));
}

TEST(ProcTask, TaskDirHasCorrectMetadata) {
  struct stat st;
  EXPECT_THAT(stat("/proc/self/task", &st), SyscallSucceeds());
  EXPECT_TRUE(S_ISDIR(st.st_mode));

  // Verify file is readable and executable by everyone.
  mode_t expected_permissions =
      S_IRUSR | S_IXUSR | S_IRGRP | S_IXGRP | S_IROTH | S_IXOTH;
  mode_t permissions = st.st_mode & (S_IRWXU | S_IRWXG | S_IRWXO);
  EXPECT_EQ(expected_permissions, permissions);
}

TEST(ProcTask, TaskDirCanSeekToEnd) {
  const FileDescriptor dirfd =
      ASSERT_NO_ERRNO_AND_VALUE(Open("/proc/self/task", O_RDONLY));
  EXPECT_THAT(lseek(dirfd.get(), 0, SEEK_END), SyscallSucceeds());
}

TEST(ProcTask, VerifyTaskDirNlinks) {
  const auto fn = [] {
    // A task directory will have 3 links if the taskgroup has a single
    // thread. For example, the following shows where the links to
    // '/proc/12345/task' comes from for a single threaded process with pid
    // 12345:
    //
    //   /proc/12345/task  <-- 1 link for the directory itself
    //     .               <-- link from "."
    //     ..
    //     12345
    //       .
    //       ..            <-- link from ".." to parent.
    //       <other contents of a task dir>
    //
    // We can't assert an absolute number of links since we don't control how
    // many threads the test framework spawns. Instead, we'll ensure creating a
    // new thread increases the number of links as expected.

    // Once we reach the test body, we can count on the thread count being
    // stable unless we spawn a new one.
    const uint64_t initial_links =
        TEST_CHECK_NO_ERRNO_AND_VALUE(Links("/proc/self/task"));
    TEST_CHECK(initial_links >= 3);

    // For each new subtask, we should gain a new link.
    BlockingChild child1;
    uint64_t links = TEST_CHECK_NO_ERRNO_AND_VALUE(Links("/proc/self/task"));
    TEST_CHECK(links == initial_links + 1);

    BlockingChild child2;
    links = TEST_CHECK_NO_ERRNO_AND_VALUE(Links("/proc/self/task"));
    TEST_CHECK(links == initial_links + 2);
  };
  // Run as a forked process to prevent terminating tasks from other tests to
  // show up here and race with the count.
  EXPECT_THAT(InForkedProcess(fn), IsPosixErrorOkAndHolds(0));
}

TEST(ProcTask, CommContainsThreadNameAndTrailingNewline) {
  constexpr char kThreadName[] = "TestThread12345";
  ASSERT_THAT(prctl(PR_SET_NAME, kThreadName), SyscallSucceeds());

  auto thread_name = ASSERT_NO_ERRNO_AND_VALUE(
      GetContents(JoinPath("/proc", absl::StrCat(getpid()), "task",
                           absl::StrCat(syscall(SYS_gettid)), "comm")));
  EXPECT_EQ(absl::StrCat(kThreadName, "\n"), thread_name);
}

TEST(ProcTask, CommCanSetSelfThreadName) {
  auto path = JoinPath("/proc", absl::StrCat(getpid()), "task",
                       absl::StrCat(syscall(SYS_gettid)), "comm");
  constexpr char kThreadName[] = "TestThread12345";
  ASSERT_NO_ERRNO(SetContents(path, kThreadName));

  auto got_thread_name = ASSERT_NO_ERRNO_AND_VALUE(GetContents(path));
  EXPECT_EQ(absl::StrCat(kThreadName, "\n"), got_thread_name);
}

TEST(ProcTask, CommCanSetPeerThreadName) {
  constexpr char kThreadName[] = "TestThread12345";

  // Path correspond to *this* thread's tid. We will changed it from the new
  // thread created below.
  auto path = JoinPath("/proc", absl::StrCat(getpid()), "task",
                       absl::StrCat(syscall(SYS_gettid)), "comm");

  // Start a thread that will set this parent threads name.
  ScopedThread peer_thread(
      [&]() { ASSERT_NO_ERRNO(SetContents(path, kThreadName)); });

  peer_thread.Join();

  // Our thread name should have been updated.
  auto got_thread_name = ASSERT_NO_ERRNO_AND_VALUE(GetContents(path));
  EXPECT_EQ(absl::StrCat(kThreadName, "\n"), got_thread_name);
}

TEST(ProcTask, CommCannotSetAnotherProcessThreadName) {
  // Path correspond to *this* thread's pid and tid.
  auto path = JoinPath("/proc", absl::StrCat(getpid()), "task",
                       absl::StrCat(syscall(SYS_gettid)), "comm");

  auto rest = [&] {
    // New process is allowed to open the file, even for writing, since the
    // owning user is the same.
    int fd;
    TEST_CHECK_SUCCESS(fd = open(path.c_str(), O_WRONLY));

    // Write gets EINVAL since the thread group is different. See Linux
    // fs/proc/base.c:comm_write.
    TEST_CHECK_ERRNO(write(fd, "x", 1), EINVAL);
  };

  EXPECT_THAT(InForkedProcess(rest), IsPosixErrorOkAndHolds(0));
}

TEST(ProcTask, CommLenLimited) {
  auto path = JoinPath("/proc", absl::StrCat(getpid()), "task",
                       absl::StrCat(syscall(SYS_gettid)), "comm");
  // comm is limited by 15 symbols (TASK_COMM_LEN).
  constexpr char kThreadName[] = "0123456789abcde";
  ASSERT_NO_ERRNO(SetContents(path, absl::StrCat(kThreadName, "XYZ")));

  auto got_thread_name = ASSERT_NO_ERRNO_AND_VALUE(GetContents(path));
  EXPECT_EQ(absl::StrCat(kThreadName, "\n"), got_thread_name);
}

TEST(ProcTaskNs, NsDirExistsAndHasCorrectMetadata) {
  EXPECT_NO_ERRNO(DirContains("/proc/self/ns", {"net", "pid", "user"}, {}));

  // Let's just test the 'pid' entry, all of them are very similar.
  struct stat st;
  EXPECT_THAT(lstat("/proc/self/ns/pid", &st), SyscallSucceeds());
  EXPECT_TRUE(S_ISLNK(st.st_mode));

  auto link = ASSERT_NO_ERRNO_AND_VALUE(ReadLink("/proc/self/ns/pid"));
  EXPECT_THAT(link, ::testing::StartsWith("pid:["));
}

TEST(ProcTaskNs, AccessOnNsNodeSucceeds) {
  EXPECT_THAT(access("/proc/self/ns/pid", F_OK), SyscallSucceeds());
}

TEST(ProcSysKernelHostname, Exists) {
  EXPECT_THAT(open("/proc/sys/kernel/hostname", O_RDONLY), SyscallSucceeds());
}

TEST(ProcSysKernelHostname, MatchesUname) {
  struct utsname buf;
  EXPECT_THAT(uname(&buf), SyscallSucceeds());
  const std::string hostname = absl::StrCat(buf.nodename, "\n");
  auto procfs_hostname =
      ASSERT_NO_ERRNO_AND_VALUE(GetContents("/proc/sys/kernel/hostname"));
  EXPECT_EQ(procfs_hostname, hostname);
}

TEST(ProcSysVmMaxmapCount, HasNumericValue) {
  const std::string val_str =
      ASSERT_NO_ERRNO_AND_VALUE(GetContents("/proc/sys/vm/max_map_count"));
  int32_t val;
  EXPECT_TRUE(absl::SimpleAtoi(val_str, &val))
      << "/proc/sys/vm/max_map_count does not contain a numeric value: "
      << val_str;
}

TEST(ProcSysVmMmapMinAddr, HasNumericValue) {
  const std::string mmap_min_addr_str =
      ASSERT_NO_ERRNO_AND_VALUE(GetContents("/proc/sys/vm/mmap_min_addr"));
  uintptr_t mmap_min_addr;
  EXPECT_TRUE(absl::SimpleAtoi(mmap_min_addr_str, &mmap_min_addr))
      << "/proc/sys/vm/mmap_min_addr does not contain a numeric value: "
      << mmap_min_addr_str;
}

TEST(ProcSysVmOvercommitMemory, HasNumericValue) {
  const std::string overcommit_memory_str =
      ASSERT_NO_ERRNO_AND_VALUE(GetContents("/proc/sys/vm/overcommit_memory"));
  uintptr_t overcommit_memory;
  EXPECT_TRUE(absl::SimpleAtoi(overcommit_memory_str, &overcommit_memory))
      << "/proc/sys/vm/overcommit_memory does not contain a numeric value: "
      << overcommit_memory;
}

// Check that link for proc fd entries point the target node, not the
// symlink itself. Regression test for b/31155070.
TEST(ProcTaskFd, FstatatFollowsSymlink) {
  const TempPath file = ASSERT_NO_ERRNO_AND_VALUE(TempPath::CreateFile());
  const FileDescriptor fd =
      ASSERT_NO_ERRNO_AND_VALUE(Open(file.path(), O_RDONLY));

  struct stat sproc = {};
  EXPECT_THAT(
      fstatat(-1, absl::StrCat("/proc/self/fd/", fd.get()).c_str(), &sproc, 0),
      SyscallSucceeds());

  struct stat sfile = {};
  EXPECT_THAT(fstatat(-1, file.path().c_str(), &sfile, 0), SyscallSucceeds());

  // If fstatat follows the fd symlink, the device and inode numbers should
  // match at a minimum.
  EXPECT_EQ(sproc.st_dev, sfile.st_dev);
  EXPECT_EQ(sproc.st_ino, sfile.st_ino);
  EXPECT_EQ(0, memcmp(&sfile, &sproc, sizeof(sfile)));
}

TEST(ProcFilesystems, Bug65172365) {
  std::string proc_filesystems =
      ASSERT_NO_ERRNO_AND_VALUE(GetContents("/proc/filesystems"));
  ASSERT_FALSE(proc_filesystems.empty());
}

// Check that /proc/mounts is a symlink to self/mounts.
TEST(ProcMounts, IsSymlink) {
  auto link = ASSERT_NO_ERRNO_AND_VALUE(ReadLink("/proc/mounts"));
  EXPECT_EQ(link, "self/mounts");
}

TEST(ProcSelfMountinfo, RequiredFieldsArePresent) {
  auto mountinfo =
      ASSERT_NO_ERRNO_AND_VALUE(GetContents("/proc/self/mountinfo"));
  EXPECT_THAT(
      mountinfo,
      AllOf(
          // Root mount.
          ContainsRegex(
              R"([0-9]+ [0-9]+ [0-9]+:[0-9]+ /\S* / (rw|ro).*- \S+ \S+ (rw|ro)\S*)"),
          // Proc mount - always rw.
          ContainsRegex(
              R"([0-9]+ [0-9]+ [0-9]+:[0-9]+ / /proc rw.*- \S+ \S+ rw\S*)")));
}

TEST(ProcSelfMountinfo, ContainsProcfsEntry) {
  const std::vector<ProcMountInfoEntry> entries =
      ASSERT_NO_ERRNO_AND_VALUE(ProcSelfMountInfoEntries());
  bool found = false;
  for (const auto& e : entries) {
    if (e.fstype == "proc") {
      found = true;
      break;
    }
  }
  EXPECT_TRUE(found);
}

// Check that /proc/self/mounts looks something like a real mounts file.
TEST(ProcSelfMounts, RequiredFieldsArePresent) {
  auto mounts = ASSERT_NO_ERRNO_AND_VALUE(GetContents("/proc/self/mounts"));
  EXPECT_THAT(mounts,
              AllOf(
                  // Root mount.
                  ContainsRegex(R"(\S+ / \S+ (rw|ro)\S* [0-9]+ [0-9]+\s)"),
                  // Root mount.
                  ContainsRegex(R"(\S+ /proc \S+ rw\S* [0-9]+ [0-9]+\s)")));
}

TEST(ProcSelfMounts, ContainsProcfsEntry) {
  const std::vector<ProcMountsEntry> entries =
      ASSERT_NO_ERRNO_AND_VALUE(ProcSelfMountsEntries());
  bool found = false;
  for (const auto& e : entries) {
    if (e.fstype == "proc") {
      found = true;
      break;
    }
  }
  EXPECT_TRUE(found);
}

void CheckDuplicatesRecursively(std::string path) {
  std::vector<std::string> child_dirs;

  // There is the known issue of the linux procfs, that two consequent calls of
  // readdir can return the same entry twice if between these calls one or more
  // entries have been removed from this directory.
  int max_attempts = 5;
  for (int i = 0; i < max_attempts; i++) {
    child_dirs.clear();
    errno = 0;
    bool success = true;
    DIR* dir = opendir(path.c_str());
    if (dir == nullptr) {
      // Ignore any directories we can't read or missing directories as the
      // directory could have been deleted/mutated from the time the parent
      // directory contents were read.
      return;
    }
    auto dir_closer = Cleanup([&dir]() { closedir(dir); });
    absl::flat_hash_set<std::string> children;
    while (true) {
      // Readdir(3): If the end of the directory stream is reached, NULL is
      // returned and errno is not changed.  If an error occurs, NULL is
      // returned and errno is set appropriately.  To distinguish end of stream
      // and from an error, set errno to zero before calling readdir() and then
      // check the value of errno if NULL is returned.
      errno = 0;
      struct dirent* dp = readdir(dir);
      if (dp == nullptr) {
        // Linux will return EINVAL when calling getdents on a /proc/tid/net
        // file corresponding to a zombie task.
        // See fs/proc/proc_net.c:proc_tgid_net_readdir().
        //
        // We just ignore the directory in this case.
        if (errno == EINVAL && absl::StartsWith(path, "/proc/") &&
            absl::EndsWith(path, "/net")) {
          break;
        }
        // We may also see permission failures traversing some files.
        if (errno == EACCES && absl::StartsWith(path, "/proc/")) {
          break;
        }

        // Otherwise, no errors are allowed.
        ASSERT_EQ(errno, 0) << path;
        break;  // We're done.
      }

      const std::string name = dp->d_name;

      if (name == "." || name == "..") {
        continue;
      }

      // Ignore a duplicate entry if it isn't the last attempt.
      if (i == max_attempts - 1) {
        ASSERT_EQ(children.find(name), children.end())
            << absl::StrCat(path, "/", name);
      } else if (children.find(name) != children.end()) {
        std::cerr << "Duplicate entry: " << i << ":"
                  << absl::StrCat(path, "/", name) << std::endl;
        success = false;
        break;
      }
      children.insert(name);

      if (dp->d_type == DT_DIR) {
        child_dirs.push_back(name);
      }
    }
    if (success) {
      break;
    }
  }
  for (auto dname = child_dirs.begin(); dname != child_dirs.end(); dname++) {
    CheckDuplicatesRecursively(absl::StrCat(path, "/", *dname));
  }
}

TEST(Proc, NoDuplicates) { CheckDuplicatesRecursively("/proc"); }

// Most /proc/PID files are owned by the task user with SUID_DUMP_USER.
TEST(ProcPid, UserDumpableOwner) {
  int before;
  ASSERT_THAT(before = prctl(PR_GET_DUMPABLE), SyscallSucceeds());
  auto cleanup = Cleanup([before] {
    ASSERT_THAT(prctl(PR_SET_DUMPABLE, before), SyscallSucceeds());
  });

  EXPECT_THAT(prctl(PR_SET_DUMPABLE, SUID_DUMP_USER), SyscallSucceeds());

  // This applies to the task directory itself and files inside.
  struct stat st;
  ASSERT_THAT(stat("/proc/self/", &st), SyscallSucceeds());
  EXPECT_EQ(st.st_uid, geteuid());
  EXPECT_EQ(st.st_gid, getegid());

  ASSERT_THAT(stat("/proc/self/stat", &st), SyscallSucceeds());
  EXPECT_EQ(st.st_uid, geteuid());
  EXPECT_EQ(st.st_gid, getegid());
}

// /proc/PID files are owned by root with SUID_DUMP_DISABLE.
TEST(ProcPid, RootDumpableOwner) {
  int before;
  ASSERT_THAT(before = prctl(PR_GET_DUMPABLE), SyscallSucceeds());
  auto cleanup = Cleanup([before] {
    ASSERT_THAT(prctl(PR_SET_DUMPABLE, before), SyscallSucceeds());
  });

  EXPECT_THAT(prctl(PR_SET_DUMPABLE, SUID_DUMP_DISABLE), SyscallSucceeds());

  // This *does not* applies to the task directory itself (or other 0555
  // directories), but does to files inside.
  struct stat st;
  ASSERT_THAT(stat("/proc/self/", &st), SyscallSucceeds());
  EXPECT_EQ(st.st_uid, geteuid());
  EXPECT_EQ(st.st_gid, getegid());

  // This file is owned by root. Also allow nobody in case this test is running
  // in a userns without root mapped.
  ASSERT_THAT(stat("/proc/self/stat", &st), SyscallSucceeds());
  EXPECT_THAT(st.st_uid, AnyOf(Eq(0), Eq(65534)));
  EXPECT_THAT(st.st_gid, AnyOf(Eq(0), Eq(65534)));
}

TEST(Proc, GetdentsEnoent) {
  FileDescriptor fd;
  ASSERT_NO_ERRNO(WithSubprocess(
      [&](int pid) -> PosixError {
        // Running.
        ASSIGN_OR_RETURN_ERRNO(fd, Open(absl::StrCat("/proc/", pid, "/task"),
                                        O_RDONLY | O_DIRECTORY));

        return NoError();
      },
      nullptr, nullptr));
  char buf[1024];
  ASSERT_THAT(syscall(SYS_getdents64, fd.get(), buf, sizeof(buf)),
              SyscallFailsWithErrno(ENOENT));
}

void CheckSyscwFromIOFile(const std::string& path, const std::string& regex) {
  std::string output;
  ASSERT_NO_ERRNO(GetContents(path, &output));
  ASSERT_THAT(output, ContainsRegex(absl::StrCat("syscw:\\s+", regex, "\n")));
}

// Checks that there is variable accounting of IO between threads/tasks.
TEST(Proc, PidTidIOAccounting) {
  absl::Notification notification;

  // Run a thread with a bunch of writes. Check that io account records exactly
  // the number of write calls. File open/close is there to prevent buffering.
  ScopedThread writer([&notification] {
    const int num_writes = 100;
    for (int i = 0; i < num_writes; i++) {
      auto path = ASSERT_NO_ERRNO_AND_VALUE(TempPath::CreateFile());
      ASSERT_NO_ERRNO(SetContents(path.path(), "a"));
    }
    notification.Notify();
    const std::string& writer_dir =
        absl::StrCat("/proc/", getpid(), "/task/", gettid(), "/io");

    CheckSyscwFromIOFile(writer_dir, std::to_string(num_writes));
  });

  // Run a thread and do no writes. Check that no writes are recorded.
  ScopedThread noop([&notification] {
    notification.WaitForNotification();
    const std::string& noop_dir =
        absl::StrCat("/proc/", getpid(), "/task/", gettid(), "/io");

    CheckSyscwFromIOFile(noop_dir, "0");
  });

  writer.Join();
  noop.Join();
}

TEST(Proc, Statfs) {
  struct statfs st;
  EXPECT_THAT(statfs("/proc", &st), SyscallSucceeds());
  EXPECT_EQ(st.f_type, PROC_SUPER_MAGIC);
  EXPECT_EQ(st.f_bsize, getpagesize());
  EXPECT_EQ(st.f_namelen, NAME_MAX);
}

// Tests that /proc/[pid]/fd/[num] can resolve to a path inside /proc.
TEST(Proc, ResolveSymlinkToProc) {
  const auto proc = ASSERT_NO_ERRNO_AND_VALUE(Open("/proc/self/cmdline", 0));
  const auto path = JoinPath("/proc/self/fd/", absl::StrCat(proc.get()));
  const auto target = ASSERT_NO_ERRNO_AND_VALUE(ReadLink(path));
  EXPECT_EQ(target, JoinPath("/proc/", absl::StrCat(getpid()), "/cmdline"));
}

// NOTE(b/236035339): Tests that opening /proc/[pid]/fd/[eventFDNum] with
// O_DIRECTORY leads to ENOTDIR.
TEST(Proc, RegressionTestB236035339) {
  FileDescriptor efd =
      ASSERT_NO_ERRNO_AND_VALUE(NewEventFD(0, EFD_NONBLOCK | EFD_CLOEXEC));
  const auto path = JoinPath("/proc/self/fd/", absl::StrCat(efd.get()));
  EXPECT_THAT(open(path.c_str(), O_RDONLY | O_CLOEXEC | O_DIRECTORY),
              SyscallFailsWithErrno(ENOTDIR));
}

// NOTE(b/338393279): Tests that after execve() from a non-leader thread
// changes which thread owns the thread group ID, the new thread group leader
// can access its /proc/self.
TEST(Proc, PidReuse) {
  const ExecveArray owned_child_argv = {"/proc/self/exe",
                                        "--proc_pid_reuse_child"};
  char* const* const child_argv = owned_child_argv.get();

  const auto rest = [child_argv] {
    struct stat statbuf;
    TEST_PCHECK(stat("/proc/self/cwd", &statbuf) == 0);

    ScopedThread([child_argv] {
      execve(child_argv[0], child_argv, /* envp = */ nullptr);
      TEST_PCHECK_MSG(false, "Survived execve to test child");
    });
  };
  EXPECT_THAT(InForkedProcess(rest), IsPosixErrorOkAndHolds(0));
}

[[noreturn]] void RunProcPidReuseChild() {
  struct stat statbuf;
  TEST_PCHECK(stat("/proc/self/cwd", &statbuf) == 0);
  _exit(0);
}

TEST(ProcFilesystems, ReadCapLastCap) {
  std::string lastCapStr =
      ASSERT_NO_ERRNO_AND_VALUE(GetContents("/proc/sys/kernel/cap_last_cap"));

  uint64_t lastCap;
  ASSERT_TRUE(absl::SimpleAtoi(lastCapStr, &lastCap));
  EXPECT_TRUE(lastCap > 32 && lastCap < 64);
}

TEST(ProcFilesystems, OverflowID) {
  std::string overflowGidStr =
      ASSERT_NO_ERRNO_AND_VALUE(GetContents("/proc/sys/kernel/overflowgid"));
  std::string overflowUidStr =
      ASSERT_NO_ERRNO_AND_VALUE(GetContents("/proc/sys/kernel/overflowuid"));
  uint64_t overflowGid, overflowUid;
  ASSERT_TRUE(absl::SimpleAtoi(overflowGidStr, &overflowGid));
  ASSERT_TRUE(absl::SimpleAtoi(overflowUidStr, &overflowUid));

  const uint64_t defaultOverflowID = 65534;
  EXPECT_EQ(overflowGid, defaultOverflowID);
  EXPECT_EQ(overflowUid, defaultOverflowID);
}

}  // namespace
}  // namespace testing
}  // namespace gvisor

int main(int argc, char** argv) {
  for (int i = 0; i < argc; ++i) {
    gvisor::testing::saved_argv.emplace_back(std::string(argv[i]));
  }

  gvisor::testing::TestInit(&argc, &argv);

  if (absl::GetFlag(FLAGS_proc_pid_reuse_child)) {
    gvisor::testing::RunProcPidReuseChild();
  }

  return gvisor::testing::RunAllTests();
}
