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

package root

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gvisor.dev/gvisor/runsc/criutil"
	"gvisor.dev/gvisor/runsc/dockerutil"
	"gvisor.dev/gvisor/runsc/specutils"
	"gvisor.dev/gvisor/runsc/testutil"
	"gvisor.dev/gvisor/test/root/testdata"
)

// Tests for crictl have to be run as root (rather than in a user namespace)
// because crictl creates named network namespaces in /var/run/netns/.

// TestCrictlSanity refers to b/112433158.
func TestCrictlSanity(t *testing.T) {
	// Setup containerd and crictl.
	crictl, cleanup, err := setup(t)
	if err != nil {
		t.Fatalf("failed to setup crictl: %v", err)
	}
	defer cleanup()
	podID, contID, err := crictl.StartPodAndContainer("httpd", testdata.Sandbox, testdata.Httpd)
	if err != nil {
		t.Fatal(err)
	}

	// Look for the httpd page.
	if err = httpGet(crictl, podID, "index.html"); err != nil {
		t.Fatalf("failed to get page: %v", err)
	}

	// Stop everything.
	if err := crictl.StopPodAndContainer(podID, contID); err != nil {
		t.Fatal(err)
	}
}

// TestMountPaths refers to b/117635704.
func TestMountPaths(t *testing.T) {
	// Setup containerd and crictl.
	crictl, cleanup, err := setup(t)
	if err != nil {
		t.Fatalf("failed to setup crictl: %v", err)
	}
	defer cleanup()
	podID, contID, err := crictl.StartPodAndContainer("httpd", testdata.Sandbox, testdata.HttpdMountPaths)
	if err != nil {
		t.Fatal(err)
	}

	// Look for the directory available at /test.
	if err = httpGet(crictl, podID, "test"); err != nil {
		t.Fatalf("failed to get page: %v", err)
	}

	// Stop everything.
	if err := crictl.StopPodAndContainer(podID, contID); err != nil {
		t.Fatal(err)
	}
}

// TestMountPaths refers to b/118728671.
func TestMountOverSymlinks(t *testing.T) {
	// Setup containerd and crictl.
	crictl, cleanup, err := setup(t)
	if err != nil {
		t.Fatalf("failed to setup crictl: %v", err)
	}
	defer cleanup()
	podID, contID, err := crictl.StartPodAndContainer("k8s.gcr.io/busybox", testdata.Sandbox, testdata.MountOverSymlink)
	if err != nil {
		t.Fatal(err)
	}

	out, err := crictl.Exec(contID, "readlink", "/etc/resolv.conf")
	if err != nil {
		t.Fatal(err)
	}
	if want := "/tmp/resolv.conf"; !strings.Contains(string(out), want) {
		t.Fatalf("/etc/resolv.conf is not pointing to %q: %q", want, string(out))
	}

	etc, err := crictl.Exec(contID, "cat", "/etc/resolv.conf")
	if err != nil {
		t.Fatal(err)
	}
	tmp, err := crictl.Exec(contID, "cat", "/tmp/resolv.conf")
	if err != nil {
		t.Fatal(err)
	}
	if tmp != etc {
		t.Fatalf("file content doesn't match:\n\t/etc/resolv.conf: %s\n\t/tmp/resolv.conf: %s", string(etc), string(tmp))
	}

	// Stop everything.
	if err := crictl.StopPodAndContainer(podID, contID); err != nil {
		t.Fatal(err)
	}
}

// TestHomeDir tests that the HOME environment variable is set for
// multi-containers.
func TestHomeDir(t *testing.T) {
	// Setup containerd and crictl.
	crictl, cleanup, err := setup(t)
	if err != nil {
		t.Fatalf("failed to setup crictl: %v", err)
	}
	defer cleanup()
	contSpec := testdata.SimpleSpec("root", "k8s.gcr.io/busybox", []string{"sleep", "1000"})
	podID, contID, err := crictl.StartPodAndContainer("k8s.gcr.io/busybox", testdata.Sandbox, contSpec)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("root container", func(t *testing.T) {
		out, err := crictl.Exec(contID, "sh", "-c", "echo $HOME")
		if err != nil {
			t.Fatal(err)
		}
		if got, want := strings.TrimSpace(string(out)), "/root"; got != want {
			t.Fatalf("Home directory invalid. Got %q, Want : %q", got, want)
		}
	})

	t.Run("sub-container", func(t *testing.T) {
		// Create a sub container in the same pod.
		subContSpec := testdata.SimpleSpec("subcontainer", "k8s.gcr.io/busybox", []string{"sleep", "1000"})
		subContID, err := crictl.StartContainer(podID, "k8s.gcr.io/busybox", testdata.Sandbox, subContSpec)
		if err != nil {
			t.Fatal(err)
		}

		out, err := crictl.Exec(subContID, "sh", "-c", "echo $HOME")
		if err != nil {
			t.Fatal(err)
		}
		if got, want := strings.TrimSpace(string(out)), "/root"; got != want {
			t.Fatalf("Home directory invalid. Got %q, Want: %q", got, want)
		}

		if err := crictl.StopContainer(subContID); err != nil {
			t.Fatal(err)
		}
	})

	// Stop everything.
	if err := crictl.StopPodAndContainer(podID, contID); err != nil {
		t.Fatal(err)
	}

}

// setup sets up before a test. Specifically it:
// * Creates directories and a socket for containerd to utilize.
// * Runs containerd and waits for it to reach a "ready" state for testing.
// * Returns a cleanup function that should be called at the end of the test.
func setup(t *testing.T) (*criutil.Crictl, func(), error) {
	var cleanups []func()
	cleanupFunc := func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}
	cleanup := specutils.MakeCleanup(cleanupFunc)
	defer cleanup.Clean()

	// Create temporary containerd root and state directories, and a socket
	// via which crictl and containerd communicate.
	containerdRoot, err := ioutil.TempDir(testutil.TmpDir(), "containerd-root")
	if err != nil {
		t.Fatalf("failed to create containerd root: %v", err)
	}
	cleanups = append(cleanups, func() { os.RemoveAll(containerdRoot) })
	containerdState, err := ioutil.TempDir(testutil.TmpDir(), "containerd-state")
	if err != nil {
		t.Fatalf("failed to create containerd state: %v", err)
	}
	cleanups = append(cleanups, func() { os.RemoveAll(containerdState) })
	sockAddr := filepath.Join(testutil.TmpDir(), "containerd-test.sock")

	// We rewrite a configuration. This is based on the current docker
	// configuration for the runtime under test.
	runtime, err := dockerutil.RuntimePath()
	if err != nil {
		t.Fatalf("error discovering runtime path: %v", err)
	}
	config, err := testutil.WriteTmpFile("containerd-config", testdata.ContainerdConfig(runtime))
	if err != nil {
		t.Fatalf("failed to write containerd config")
	}
	cleanups = append(cleanups, func() { os.RemoveAll(config) })

	// Start containerd.
	containerd := exec.Command(getContainerd(),
		"--config", config,
		"--log-level", "debug",
		"--root", containerdRoot,
		"--state", containerdState,
		"--address", sockAddr)
	cleanups = append(cleanups, func() {
		if err := testutil.KillCommand(containerd); err != nil {
			log.Printf("error killing containerd: %v", err)
		}
	})
	containerdStderr, err := containerd.StderrPipe()
	if err != nil {
		t.Fatalf("failed to get containerd stderr: %v", err)
	}
	containerdStdout, err := containerd.StdoutPipe()
	if err != nil {
		t.Fatalf("failed to get containerd stdout: %v", err)
	}
	if err := containerd.Start(); err != nil {
		t.Fatalf("failed running containerd: %v", err)
	}

	// Wait for containerd to boot. Then put all containerd output into a
	// buffer to be logged at the end of the test.
	testutil.WaitUntilRead(containerdStderr, "Start streaming server", nil, 10*time.Second)
	stdoutBuf := &bytes.Buffer{}
	stderrBuf := &bytes.Buffer{}
	go func() { io.Copy(stdoutBuf, containerdStdout) }()
	go func() { io.Copy(stderrBuf, containerdStderr) }()
	cleanups = append(cleanups, func() {
		t.Logf("containerd stdout: %s", string(stdoutBuf.Bytes()))
		t.Logf("containerd stderr: %s", string(stderrBuf.Bytes()))
	})

	cleanup.Release()
	return criutil.NewCrictl(20*time.Second, sockAddr), cleanupFunc, nil
}

// httpGet GETs the contents of a file served from a pod on port 80.
func httpGet(crictl *criutil.Crictl, podID, filePath string) error {
	// Get the IP of the httpd server.
	ip, err := crictl.PodIP(podID)
	if err != nil {
		return fmt.Errorf("failed to get IP from pod %q: %v", podID, err)
	}

	// GET the page. We may be waiting for the server to start, so retry
	// with a timeout.
	var resp *http.Response
	cb := func() error {
		r, err := http.Get(fmt.Sprintf("http://%s", path.Join(ip, filePath)))
		resp = r
		return err
	}
	if err := testutil.Poll(cb, 20*time.Second); err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("bad status returned: %d", resp.StatusCode)
	}
	return nil
}

func getContainerd() string {
	// Use the local path if it exists, otherwise, use the system one.
	if _, err := os.Stat("/usr/local/bin/containerd"); err == nil {
		return "/usr/local/bin/containerd"
	}
	return "/usr/bin/containerd"
}
