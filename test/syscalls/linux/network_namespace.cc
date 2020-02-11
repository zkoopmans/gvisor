// Copyright 2020 The gVisor Authors.
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

#include <net/if.h>
#include <sched.h>
#include <sys/ioctl.h>
#include <sys/socket.h>
#include <sys/types.h>

#include "gmock/gmock.h"
#include "gtest/gtest.h"
#include "test/syscalls/linux/socket_test_util.h"
#include "test/util/capability_util.h"
#include "test/util/memory_util.h"
#include "test/util/test_util.h"

namespace gvisor {
namespace testing {

namespace {

using ExitStatus = int;
using VoidFunc = std::function<void()>;
using RunFunc = std::function<PosixErrorOr<ExitStatus>(VoidFunc)>;

struct NamespaceStrategy {
  RunFunc run;

  static NamespaceStrategy Of(RunFunc run) {
    NamespaceStrategy s;
    s.run = run;
    return s;
  }
};

PosixErrorOr<ExitStatus> RunWithUnshare(VoidFunc fn) {
  pid_t child = fork();
  if (child < 0) {
    return PosixError(errno);
  }
  if (child == 0) {
    fn();
    _exit(0);
  }
  int status;
  if (waitpid(child, &status, 0) < 0) {
    return PosixError(errno);
  }
  return WEXITSTATUS(status);
}

PosixErrorOr<ExitStatus> RunWithClone(VoidFunc fn) {
  ASSIGN_OR_RETURN_ERRNO(
      Mapping child_stack,
      MmapAnon(kPageSize, PROT_READ | PROT_WRITE, MAP_PRIVATE));
  pid_t child = clone(
      +[](void *arg) {
        // Make a copy of fn.
        auto fn = *reinterpret_cast<VoidFunc *>(arg);
        fn();
        return 0;
      },
      reinterpret_cast<void *>(child_stack.addr() + kPageSize),
      CLONE_NEWNET | SIGCHLD, &fn);
  if (child < 0) {
    return PosixError(errno);
  }
  int status;
  if (waitpid(child, &status, 0) < 0) {
    return PosixError(errno);
  }
  return WEXITSTATUS(status);
}

class NetworkNamespaceTest
    : public ::testing::TestWithParam<NamespaceStrategy> {};

TEST_P(NetworkNamespaceTest, LoopbackExists) {
  SKIP_IF(!ASSERT_NO_ERRNO_AND_VALUE(HaveCapability(CAP_NET_ADMIN)));

  auto exit_result = GetParam().run([]() {
    TEST_CHECK_MSG(unshare(CLONE_NEWNET) == 0, "unshare() failed");

    // TODO(gvisor.dev/issue/1833): Update this to test that only "lo" exists.
    // Check loopback device exists.
    int sock = socket(AF_INET, SOCK_DGRAM, 0);
    TEST_PCHECK_MSG(sock > 0, "socket() failed");
    struct ifreq ifr;
    snprintf(ifr.ifr_name, IFNAMSIZ, "lo");
    TEST_CHECK_MSG(ioctl(sock, SIOCGIFINDEX, &ifr) == 0, "ioctl() failed");
    _exit(0);
  });

  EXPECT_THAT(exit_result, IsPosixErrorOkAndHolds(0));
}

INSTANTIATE_TEST_SUITE_P(
    AllNetworkNamespaceTest, NetworkNamespaceTest,
    ::testing::Values(NamespaceStrategy::Of(RunWithUnshare),
                      NamespaceStrategy::Of(RunWithClone)));

}  // namespace

}  // namespace testing
}  // namespace gvisor
