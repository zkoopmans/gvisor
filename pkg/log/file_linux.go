// Copyright 2026 The gVisor Authors.
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

package log

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// openLogFile opens a regular file without following symlinks. The resulting
// descriptor may be donated to a less privileged process, so validate the
// opened object before truncating it or returning it to the caller.
func openLogFile(logPath string, flags int) (*os.File, error) {
	absPath, err := filepath.Abs(logPath)
	if err != nil {
		return nil, err
	}
	dirFD, err := openLogDir(filepath.Dir(absPath))
	if err != nil {
		return nil, fmt.Errorf("opening parent directory: %w", err)
	}
	defer unix.Close(dirFD)

	how := unix.OpenHow{
		Flags:   uint64(flags&^os.O_TRUNC | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_NOCTTY),
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_XDEV,
	}
	if flags&os.O_CREATE != 0 {
		how.Mode = 0644
	}
	fd, err := unix.Openat2(dirFD, filepath.Base(absPath), &how)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), logPath)
	if err := validateLogFile(fd); err != nil {
		f.Close()
		return nil, err
	}
	if flags&os.O_TRUNC != 0 {
		if err := f.Truncate(0); err != nil {
			f.Close()
			return nil, err
		}
	}
	return f, nil
}

// openLogDir creates missing directories while holding each parent open. Using
// directory-relative operations avoids symlink races during parent creation.
// dir must be an absolute, cleaned path.
func openLogDir(dir string) (int, error) {
	const flags = unix.O_PATH | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	fd, err := unix.Open("/", flags, 0)
	if err != nil {
		return -1, err
	}
	for _, component := range strings.Split(strings.TrimPrefix(dir, "/"), "/") {
		if component == "" {
			continue
		}
		next, err := unix.Openat(fd, component, flags, 0)
		if err == unix.ENOENT {
			if err = unix.Mkdirat(fd, component, 0755); err == nil || err == unix.EEXIST {
				next, err = unix.Openat(fd, component, flags, 0)
			}
		}
		unix.Close(fd)
		if err != nil {
			return -1, err
		}
		fd = next
	}
	return fd, nil
}

func validateLogFile(fd int) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 {
		return fmt.Errorf("log destination must be a regular file with one link")
	}
	var fs unix.Statfs_t
	if err := unix.Fstatfs(fd, &fs); err != nil {
		return err
	}
	// In particular, procfs process-memory files report S_IFREG. File mode
	// alone does not make a kernel-control file a safe logging capability.
	switch uint32(fs.Type) {
	case unix.PROC_SUPER_MAGIC, unix.SYSFS_MAGIC, unix.CGROUP_SUPER_MAGIC,
		unix.CGROUP2_SUPER_MAGIC, unix.DEBUGFS_MAGIC, unix.TRACEFS_MAGIC,
		unix.SECURITYFS_MAGIC, unix.BPF_FS_MAGIC, unix.SELINUX_MAGIC,
		unix.SMACK_MAGIC, unix.NSFS_MAGIC, unix.DEVPTS_SUPER_MAGIC,
		unix.BINFMTFS_MAGIC, unix.EFIVARFS_MAGIC, unix.PSTOREFS_MAGIC:
		return fmt.Errorf("log destination is on a kernel-control filesystem")
	}
	return nil
}
