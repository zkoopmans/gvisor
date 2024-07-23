// Copyright 2021 The gVisor Authors.
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

package proc

import (
	"bytes"
	"fmt"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/atomicbitops"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/kernfs"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/usermem"
)

func (fs *filesystem) newYAMAPtraceScopeFile(ctx context.Context, k *kernel.Kernel, creds *auth.Credentials) kernfs.Inode {
	s := &yamaPtraceScope{level: &k.YAMAPtraceScope}
	s.Init(ctx, creds, linux.UNNAMED_MAJOR, fs.devMinor, fs.NextIno(), s, 0644)
	return s
}

// yamaPtraceScope implements vfs.WritableDynamicBytesSource for
// /sys/kernel/yama/ptrace_scope.
//
// +stateify savable
type yamaPtraceScope struct {
	kernfs.DynamicBytesFile

	// level is the ptrace_scope level.
	level *atomicbitops.Int32
}

var _ vfs.WritableDynamicBytesSource = (*yamaPtraceScope)(nil)

// Generate implements vfs.DynamicBytesSource.Generate.
func (s *yamaPtraceScope) Generate(ctx context.Context, buf *bytes.Buffer) error {
	_, err := fmt.Fprintf(buf, "%d\n", s.level.Load())
	return err
}

// Write implements vfs.WritableDynamicBytesSource.Write.
func (s *yamaPtraceScope) Write(ctx context.Context, _ *vfs.FileDescription, src usermem.IOSequence, offset int64) (int64, error) {
	if offset != 0 {
		// Ignore partial writes.
		return 0, linuxerr.EINVAL
	}
	buf := make([]int32, 1)
	n, err := ParseInt32Vec(ctx, src, buf)
	if err != nil || n == 0 {
		return 0, err
	}

	// We do not support YAMA levels > YAMA_SCOPE_RELATIONAL.
	if buf[0] < linux.YAMA_SCOPE_DISABLED || buf[0] > linux.YAMA_SCOPE_RELATIONAL {
		return 0, linuxerr.EINVAL
	}

	s.level.Store(buf[0])
	return n, nil
}
