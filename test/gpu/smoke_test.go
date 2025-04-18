// Copyright 2023 The gVisor Authors.
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

// Package smoke_test tests basic GPU functionality.
package smoke_test

import (
	"context"
	"testing"

	"gvisor.dev/gvisor/pkg/test/dockerutil"
)

/*

func TestNvidiaSmi(t *testing.T) {
	ctx := context.Background()
	c := dockerutil.MakeContainer(ctx, t)
	defer c.CleanUp(ctx)

	opts, err := dockerutil.GPURunOpts(dockerutil.SniffGPUOpts{
		Capabilities: "all",
	})
	if err != nil {
		t.Fatalf("failed to get GPU run options: %v", err)
	}
	opts.Image = "gpu/cuda-tests"
	cmd := "nvidia-smi"
	out, err := c.Run(ctx, opts, cmd)
	t.Logf("%q output:", cmd)
	for _, line := range strings.Split(string(out), "\n") {
		t.Logf("%q: %s", cmd, line)
	}
	if err != nil {
		t.Fatalf("could not run %q: %v", cmd, err)
	}
}

func TestGPUHello(t *testing.T) {
	ctx := context.Background()
	c := dockerutil.MakeContainer(ctx, t)
	defer c.CleanUp(ctx)

	opts, err := dockerutil.GPURunOpts(dockerutil.SniffGPUOpts{
		Capabilities: "all",
	})
	if err != nil {
		t.Fatalf("failed to get GPU run options: %v", err)
	}
	opts.Image = "gpu/cuda-tests"
	out, err := c.Run(ctx, opts, "/run_sample", "--timeout=120s", "0_Introduction/vectorAdd")
	t.Logf("0_Introduction/vectorAdd output: %s", string(out))
	if err != nil {
		t.Fatalf("could not run 0_Introduction/vectorAdd: %v", err)
	}
}

func TestCUDASmokeTests(t *testing.T) {
	ctx := context.Background()
	c := dockerutil.MakeContainer(ctx, t)
	defer c.CleanUp(ctx)

	opts, err := dockerutil.GPURunOpts(dockerutil.SniffGPUOpts{AllowIncompatibleIoctl: true})
	if err != nil {
		t.Fatalf("failed to get GPU run options: %v", err)
	}
	opts.Image = "gpu/cuda-tests"
	out, err := c.Run(ctx, opts, "/run_smoke.sh")
	t.Logf("cuda-tests smoke tests output: %s", string(out))
	if err != nil {
		t.Fatalf("could not run cuda-tests smoke tests: %v", err)
	}
}
*/

func TestCUDASmokeTests(t *testing.T) {
	ctx := context.Background()
	cudaVersion, err := dockerutil.GetCudaVersion(ctx, t)
	if err != nil {
		t.Fatalf("failed to get cuda version: %v", err)
	}

	image := "gpu/cuda-tests"
	if cudaVersion.IsAtLeast(dockerutil.CudaVersion{
		Major: 12,
		Minor: 8,
	}) {
		image = "gpu/cuda-tests-12-8"
	}

	c := dockerutil.MakeContainer(ctx, t)
	defer c.CleanUp(ctx)

	opts, err := dockerutil.GPURunOpts(dockerutil.SniffGPUOpts{AllowIncompatibleIoctl: true})
	if err != nil {
		t.Fatalf("failed to get GPU run options: %v", err)
	}
	opts.Image = image
	out, err := c.Run(ctx, opts, "/run_smoke.sh")
	t.Logf("cuda-tests smoke tests output: %s", string(out))
	if err != nil {
		t.Fatalf("could not run cuda-tests smoke tests: %v", err)
	}
}

func TestCUDAVersion(t *testing.T) {

	out := `NVIDIA-SMI version  : 570.86.15
NVML version        : 570.86
DRIVER version      : 570.86.15
CUDA Version        : 12.8`

	version, err := dockerutil.NewCudaVersionFromOutput(out)
	if err != nil {
		t.Fatalf("Failed to parse cuda version: %v", err)
	}
	t.Logf("CUDA VERSION: %+v", version)

	if !version.IsAtLeast(&dockerutil.CudaVersion{Major: 1, Minor: 0}) {
		t.Fatalf("shoud greater than 1.0")
	}

	if !version.IsAtLeast(&dockerutil.CudaVersion{Major: 12, Minor: 8}) {
		t.Fatalf("should be equal")
	}

	if version.IsAtLeast(&dockerutil.CudaVersion{Major: 12, Minor: 9}) {
		t.Fatalf("should be less than 12.9")
	}

	if version.IsAtLeast(&dockerutil.CudaVersion{Major: 13, Minor: 0}) {
		t.Fatalf("should be less than 13.0")
	}
}
