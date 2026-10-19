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

//go:build linux

package log

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestOpenLogFileNormal(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "sub", "test.log")

	f, err := openLogFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC)
	if err != nil {
		t.Fatalf("openLogFile failed: %v", err)
	}
	defer f.Close()

	if _, err := f.WriteString("hello log\n"); err != nil {
		t.Fatalf("WriteString failed: %v", err)
	}
}

func TestOpenLogFileSymlinkInDirectory(t *testing.T) {
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	if err := os.Mkdir(realDir, 0755); err != nil {
		t.Fatalf("Mkdir failed: %v", err)
	}
	symDir := filepath.Join(dir, "sym")
	if err := os.Symlink(realDir, symDir); err != nil {
		t.Fatalf("Symlink failed: %v", err)
	}

	logPath := filepath.Join(symDir, "test.log")
	f, err := openLogFile(logPath, os.O_CREATE|os.O_WRONLY)
	if err == nil {
		f.Close()
		t.Fatalf("openLogFile unexpectedly succeeded for symlinked directory: %s", logPath)
	}
}

func TestOpenLogFileSymlinkTarget(t *testing.T) {
	dir := t.TempDir()
	targetFile := filepath.Join(dir, "target.log")
	if err := os.WriteFile(targetFile, []byte("target\n"), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	symFile := filepath.Join(dir, "sym.log")
	if err := os.Symlink(targetFile, symFile); err != nil {
		t.Fatalf("Symlink failed: %v", err)
	}

	f, err := openLogFile(symFile, os.O_WRONLY)
	if err == nil {
		f.Close()
		t.Fatalf("openLogFile unexpectedly succeeded for symlinked file: %s", symFile)
	}
}

func TestOpenLogFileProcfsRejected(t *testing.T) {
	// Attempting to open /proc/self/mem as a log file must fail.
	procMem := fmt.Sprintf("/proc/%d/mem", os.Getpid())
	f, err := openLogFile(procMem, os.O_WRONLY)
	if err == nil {
		f.Close()
		t.Fatalf("openLogFile unexpectedly succeeded for procfs process memory: %s", procMem)
	}
	if !strings.Contains(err.Error(), "kernel-control filesystem") && !strings.Contains(err.Error(), "SYMLINKS") && !strings.Contains(err.Error(), "open") {
		t.Logf("Expected rejection of procfs file, got: %v", err)
	}
}

func TestOpenLogFileHardlinkRejected(t *testing.T) {
	dir := t.TempDir()
	file1 := filepath.Join(dir, "file1.log")
	if err := os.WriteFile(file1, []byte("test\n"), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	file2 := filepath.Join(dir, "file2.log")
	if err := os.Link(file1, file2); err != nil {
		t.Fatalf("Link failed: %v", err)
	}

	f, err := openLogFile(file1, os.O_WRONLY)
	if err == nil {
		f.Close()
		t.Fatalf("openLogFile unexpectedly succeeded for multi-link file: %s", file1)
	}
	if !strings.Contains(err.Error(), "must be a regular file with one link") {
		t.Fatalf("Expected one link error, got: %v", err)
	}
}

func TestOpenLogFileFifoRejected(t *testing.T) {
	dir := t.TempDir()
	fifoPath := filepath.Join(dir, "test.fifo")
	if err := unix.Mkfifo(fifoPath, 0644); err != nil {
		t.Fatalf("Mkfifo failed: %v", err)
	}

	f, err := openLogFile(fifoPath, os.O_RDWR)
	if err == nil {
		f.Close()
		t.Fatalf("openLogFile unexpectedly succeeded for FIFO: %s", fifoPath)
	}
}
