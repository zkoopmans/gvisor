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

package cmd

import (
	"flag"
	"fmt"
	"os"
	"slices"
	"testing"

	"github.com/moby/sys/capability"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/test/testutil"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/container"
	"gvisor.dev/gvisor/runsc/specutils"
)

func init() {
	log.SetLevel(log.Debug)
	if err := testutil.ConfigureExePath(); err != nil {
		panic(err.Error())
	}
}

func checkProcessCaps(pid int, wantCaps *specs.LinuxCapabilities) error {
	curCaps, err := capability.NewPid2(pid)
	if err != nil {
		return fmt.Errorf("capability.NewPid2(%d) failed: %v", pid, err)
	}
	if err := curCaps.Load(); err != nil {
		return fmt.Errorf("unable to load capabilities: %v", err)
	}
	fmt.Printf("Capabilities (PID: %d): %v\n", pid, curCaps)

	for _, c := range allCapTypes {
		if err := checkCaps(c, curCaps, wantCaps); err != nil {
			return err
		}
	}
	return nil
}

func checkCaps(which capability.CapType, curCaps capability.Capabilities, wantCaps *specs.LinuxCapabilities) error {
	wantNames := getCaps(which, wantCaps)
	for name, c := range capFromName {
		want := slices.Contains(wantNames, name)
		got := curCaps.Get(which, c)
		if want != got {
			if want {
				return fmt.Errorf("capability %v:%s should be set", which, name)
			}
			return fmt.Errorf("capability %v:%s should NOT be set", which, name)
		}
	}
	return nil
}

func TestCapabilities(t *testing.T) {
	t.Run("directfs", func(t *testing.T) { testCapabilities(t, true) })
	t.Run("lisafs", func(t *testing.T) { testCapabilities(t, false) })
}

func testCapabilities(t *testing.T, directfs bool) {
	stop := testutil.StartReaper()
	defer stop()

	spec := testutil.NewSpecWithArgs("/bin/sleep", "10000")
	caps := []string{
		"CAP_CHOWN",
		"CAP_SYS_ADMIN",
		"CAP_NET_ADMIN",
	}
	spec.Process.Capabilities = &specs.LinuxCapabilities{
		Permitted: caps,
		Bounding:  caps,
		Effective: caps,
	}

	conf := testutil.TestConfig(t)
	conf.DirectFS = directfs

	// Use --network=host to make sandbox use spec's capabilities.
	conf.Network = config.NetworkHost

	_, bundleDir, cleanup, err := testutil.SetupContainer(spec, conf)
	if err != nil {
		t.Fatalf("error setting up container: %v", err)
	}
	defer cleanup()

	// Create and start the container.
	args := container.Args{
		ID:        testutil.RandomContainerID(),
		Spec:      spec,
		BundleDir: bundleDir,
	}
	c, err := container.New(conf, args)
	if err != nil {
		t.Fatalf("error creating container: %v", err)
	}
	defer c.Destroy()
	if err := c.Start(conf); err != nil {
		t.Fatalf("error starting container: %v", err)
	}

	caps = []string{
		"CAP_SYS_PTRACE", // ptrace is added due to the platform choice.
		"CAP_NET_ADMIN",
		"CAP_NET_BIND_SERVICE",
	}
	wantSandboxCaps := &specs.LinuxCapabilities{
		Permitted: caps,
		Bounding:  caps,
		Effective: caps,
	}
	if directfs {
		// With directfs, the sandbox has additional capabilities.
		wantSandboxCaps = specutils.MergeCapabilities(wantSandboxCaps, directfsSandboxLinuxCaps)
	}
	// Check that sandbox and gofer have the proper capabilities.
	if err := checkProcessCaps(c.Sandbox.Getpid(), wantSandboxCaps); err != nil {
		t.Error(err)
	}
	if err := checkProcessCaps(c.GoferPid, goferCaps); err != nil {
		t.Error(err)
	}
}

func TestMain(m *testing.M) {
	flag.Parse()
	if err := specutils.MaybeRunAsRoot(); err != nil {
		fmt.Fprintf(os.Stderr, "Error running as root: %v", err)
		os.Exit(123)
	}
	os.Exit(m.Run())
}
