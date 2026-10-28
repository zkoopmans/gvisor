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

package client

import (
	"context"
	"os"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/container"
)

func TestClientNew(t *testing.T) {
	// Test New with nil config
	cNil := New(nil)
	if got, want := cNil.RootDir(), config.DefaultRootDir(); got != want {
		t.Errorf("New(nil).RootDir() = %q, want %q", got, want)
	}

	// Test New with empty RootDir
	cEmpty := New(&config.Config{})
	if got, want := cEmpty.RootDir(), config.DefaultRootDir(); got != want {
		t.Errorf("New(&Config{}).RootDir() = %q, want %q", got, want)
	}

	// Test New with config
	conf := &config.Config{
		RootDir: "/tmp/runsc-test",
		Debug:   true,
	}
	c := New(conf)
	if got := c.RootDir(); got != "/tmp/runsc-test" {
		t.Errorf("RootDir() = %q, want %q", got, "/tmp/runsc-test")
	}
	if got := c.Config(); got != conf {
		t.Errorf("Config() = %v, want %v", got, conf)
	}
	if !c.Config().Debug {
		t.Errorf("Config().Debug = false, want true")
	}

	// Test NewFromRootDir
	cRoot := NewFromRootDir("/tmp/custom-root")
	if got := cRoot.RootDir(); got != "/tmp/custom-root" {
		t.Errorf("NewFromRootDir().RootDir() = %q, want %q", got, "/tmp/custom-root")
	}

	// Test default options: exact=false, skipCheck=false
	baseOpts := c.baseLoadOpts()
	if baseOpts.Exact {
		t.Errorf("default baseLoadOpts().Exact = true, want false")
	}
	if baseOpts.SkipCheck {
		t.Errorf("default baseLoadOpts().SkipCheck = true, want false")
	}

	// Test ClientOption overrides
	cOpts := New(conf, WithExact(true), WithSkipCheck(true))
	customOpts := cOpts.baseLoadOpts()
	if !customOpts.Exact {
		t.Errorf("customOpts.Exact = false, want true")
	}
	if !customOpts.SkipCheck {
		t.Errorf("customOpts.SkipCheck = false, want true")
	}
}

func TestClientLoadNonExistent(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "runsc-client-test-")
	if err != nil {
		t.Fatalf("MkdirTemp failed: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	c := NewFromRootDir(tmpDir)
	_, err = c.Load(context.Background(), "nonexistent-container-id", container.LoadOpts{})
	if err == nil {
		t.Fatalf("Load() expected error for nonexistent container, got nil")
	}
}

func TestClientPauseResumeStateNonExistent(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "runsc-client-test-")
	if err != nil {
		t.Fatalf("MkdirTemp failed: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	c := NewFromRootDir(tmpDir)
	ctx := context.Background()

	if err := c.Pause(ctx, "nonexistent"); err == nil {
		t.Errorf("Pause() expected error for nonexistent container, got nil")
	}
	if err := c.Resume(ctx, "nonexistent"); err == nil {
		t.Errorf("Resume() expected error for nonexistent container, got nil")
	}
	if _, err := c.State(ctx, "nonexistent"); err == nil {
		t.Errorf("State() expected error for nonexistent container, got nil")
	}
	if _, err := c.Spec(ctx, "nonexistent"); err == nil {
		t.Errorf("Spec() expected error for nonexistent container, got nil")
	}
}

func TestClientListEmpty(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "runsc-client-test-")
	if err != nil {
		t.Fatalf("MkdirTemp failed: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	c := NewFromRootDir(tmpDir)
	list, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("List() unexpected error: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("List() returned %d items, want 0", len(list))
	}
}

func TestClientContextCancellation(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "runsc-client-test-")
	if err != nil {
		t.Fatalf("MkdirTemp failed: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	c := NewFromRootDir(tmpDir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = c.Load(ctx, "any-id", container.LoadOpts{})
	if err != context.Canceled {
		t.Errorf("Load() with canceled context: got %v, want %v", err, context.Canceled)
	}

	_, err = c.List(ctx)
	if err != context.Canceled {
		t.Errorf("List() with canceled context: got %v, want %v", err, context.Canceled)
	}
}

func TestContainerMethods(t *testing.T) {
	c := New(nil)
	cont := &container.Container{
		ID:     "test-container",
		Status: container.Stopped,
		Spec:   &specs.Spec{},
	}

	state := c.StateContainer(cont)
	if state.ID != "test-container" {
		t.Errorf("StateContainer().ID = %q, want test-container", state.ID)
	}

	spec := c.SpecContainer(cont)
	if spec == nil {
		t.Errorf("SpecContainer() = nil, want non-nil")
	}
}

func TestParseSignal(t *testing.T) {
	tests := []struct {
		input   string
		want    unix.Signal
		wantErr bool
	}{
		{"KILL", unix.SIGKILL, false},
		{"SIGKILL", unix.SIGKILL, false},
		{"9", unix.SIGKILL, false},
		{"TERM", unix.SIGTERM, false},
		{"SIGTERM", unix.SIGTERM, false},
		{"15", unix.SIGTERM, false},
		{"HUP", unix.SIGHUP, false},
		{"SIGHUP", unix.SIGHUP, false},
		{"1", unix.SIGHUP, false},
		{"INVALID_SIGNAL", 0, true},
		{"9999", 0, true},
	}

	for _, tt := range tests {
		got, err := ParseSignal(tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseSignal(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && got != tt.want {
			t.Errorf("ParseSignal(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestClientKillInvalidOpts(t *testing.T) {
	c := New(nil)
	ctx := context.Background()

	// More than one mode specified
	if err := c.Kill(ctx, "id", KillOpts{All: true, PID: 123}); err == nil {
		t.Errorf("Kill() with All and PID: want error, got nil")
	}
	if err := c.Kill(ctx, "id", KillOpts{All: true, PGID: 123}); err == nil {
		t.Errorf("Kill() with All and PGID: want error, got nil")
	}
	if err := c.Kill(ctx, "id", KillOpts{PID: 123, PGID: 123}); err == nil {
		t.Errorf("Kill() with PID and PGID: want error, got nil")
	}
}

func TestClientKillDeleteNonExistent(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "runsc-client-test-")
	if err != nil {
		t.Fatalf("MkdirTemp failed: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	c := NewFromRootDir(tmpDir)
	ctx := context.Background()

	// Kill non-existent container should fail
	if err := c.Kill(ctx, "nonexistent", KillOpts{}); err == nil {
		t.Errorf("Kill() on non-existent container: want error, got nil")
	}

	// Delete non-existent without force should fail
	if err := c.Delete(ctx, "nonexistent", DeleteOpts{Force: false}); err == nil {
		t.Errorf("Delete() on non-existent container without force: want error, got nil")
	}

	// Delete non-existent with force should succeed (no-op)
	if err := c.Delete(ctx, "nonexistent", DeleteOpts{Force: true}); err != nil {
		t.Errorf("Delete() on non-existent container with force: want nil, got %v", err)
	}
}

func TestClientKillDeleteCancellation(t *testing.T) {
	c := New(nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := c.Kill(ctx, "id", KillOpts{}); err != context.Canceled {
		t.Errorf("Kill() with canceled context: got %v, want %v", err, context.Canceled)
	}
	if err := c.Delete(ctx, "id", DeleteOpts{}); err != context.Canceled {
		t.Errorf("Delete() with canceled context: got %v, want %v", err, context.Canceled)
	}

	fakeCont := &container.Container{ID: "fake"}
	if err := c.KillContainer(ctx, fakeCont, KillOpts{}); err != context.Canceled {
		t.Errorf("KillContainer() with canceled context: got %v, want %v", err, context.Canceled)
	}
	if err := c.DeleteContainer(ctx, fakeCont, DeleteOpts{}); err != context.Canceled {
		t.Errorf("DeleteContainer() with canceled context: got %v, want %v", err, context.Canceled)
	}
}
