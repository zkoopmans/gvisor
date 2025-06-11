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

// Package nccl_test runs through NCCL tests.
package nccl_test

import (
	"context"
	"flag"
	"fmt"
	"os"
	"testing"

	"gvisor.dev/gvisor/pkg/test/dockerutil"
)

var (
	ncclTimeout = flag.Int64("nccl_timeout", 0, "passes this as the timeout (s) flag to the nccl container")
)

// runNCCL runs the given script and command in a NCCL container.
func runNCCL(ctx context.Context, t *testing.T, testName string) {
	t.Helper()
	numGPU := dockerutil.NumGPU()
	c := dockerutil.MakeContainer(ctx, t)
	opts, err := dockerutil.GPURunOpts(dockerutil.SniffGPUOpts{})
	if err != nil {
		t.Fatalf("Failed to get GPU run options: %v", err)
	}
	opts.Image = "gpu/nccl-tests"
	cmd := fmt.Sprintf("/nccl-tests/build/%s --ngpus %d", testName, numGPU)
	if *ncclTimeout > 0 {
		cmd = fmt.Sprintf("%s --timeout %s", *ncclTimeout)
	}
	out, err := c.Run(ctx, opts, cmd)
	if err != nil {
		t.Errorf("Failed: %v\nContainer output:\n%s", err, out)
	} else {
		t.Logf("Container output:\n%s", out)
	}
}

func TestNCCL(t *testing.T) {
	testNames := []string{
		"all_gather_perf",
		"all_reduce_perf",
		"alltoall_perf",
		"broadcast_perf",
		"gather_perf",
		"hypercube_perf",
		"reduce_perf",
		"reduce_scatter_perf",
		"scatter_perf",
		"sendrecv_perf",
	}

	ctx := context.Background()
	for _, test := range testNames {
		t.Run(test, func(t *testing.T) {
			runNCCL(ctx, t, test)
		})
	}
}

func TestMain(m *testing.M) {
	dockerutil.EnsureSupportedDockerVersion()
	flag.Parse()
	os.Exit(m.Run())
}