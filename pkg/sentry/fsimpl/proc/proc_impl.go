// Copyright 2024 The gVisor Authors.
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

//go:build !false
// +build !false

package proc

import (
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/kernfs"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
)

// ExtraInternalData is an empty struct that could contain extra data for the procfs.
//
// +stateify savable
type ExtraInternalData struct{}

func (fs *filesystem) newTasksInodeExtra(ctx context.Context, root *auth.Credentials, internalData *InternalData, _ *kernel.Kernel, nodes map[string]kernfs.Inode) {
	if internalData.GVisorMarkerFile {
		nodes["gvisor"] = fs.newStaticDir(ctx, root, map[string]kernfs.Inode{
			"kernel_is_gvisor": newStaticFile("gvisor\n"),
		})
	}
}
