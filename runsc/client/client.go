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

// Package client provides programmatic access to gVisor sandboxes and
// containers without spawning runsc subprocesses.
package client

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/container"
)

// ClientOption configures a Client instance.
type ClientOption func(*Client)

// WithExact configures whether container ID lookups require exact matches.
// Defaults to true for programmatic client operations to avoid prefix directory scanning.
func WithExact(exact bool) ClientOption {
	return func(c *Client) {
		c.exact = exact
	}
}

// WithSkipCheck configures whether container operations skip checking if the
// container is still running. Defaults to false.
func WithSkipCheck(skipCheck bool) ClientOption {
	return func(c *Client) {
		c.skipCheck = skipCheck
	}
}

// Client provides programmatic access to containers managed in a runsc root directory.
type Client struct {
	conf      *config.Config
	exact     bool
	skipCheck bool
}

// New creates a new Client configured with the specified Config and options.
// If conf is nil or has an empty RootDir, config.DefaultRootDir() is used.
func New(conf *config.Config, opts ...ClientOption) *Client {
	if conf == nil {
		conf = &config.Config{}
	}
	if conf.RootDir == "" {
		conf.RootDir = config.DefaultRootDir()
	}
	c := &Client{
		conf: conf,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// NewFromRootDir creates a new Client configured for the specified root directory.
// If rootDir is empty, config.DefaultRootDir() is used.
func NewFromRootDir(rootDir string, opts ...ClientOption) *Client {
	return New(&config.Config{RootDir: rootDir}, opts...)
}

func (c *Client) baseLoadOpts() container.LoadOpts {
	return container.LoadOpts{
		Exact:     c.exact,
		SkipCheck: c.skipCheck,
	}
}

// RootDir returns the root directory associated with this client.
func (c *Client) RootDir() string {
	return c.conf.RootDir
}

// Config returns the configuration associated with this client.
func (c *Client) Config() *config.Config {
	return c.conf
}

// Load loads a container by ID from the root directory.
func (c *Client) Load(ctx context.Context, id string, opts container.LoadOpts) (*container.Container, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return container.Load(c.conf.RootDir, container.FullID{ContainerID: id}, opts)
}

// List returns the full IDs of all containers in the root directory.
func (c *Client) List(ctx context.Context) ([]container.FullID, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return container.List(c.conf.RootDir)
}

// State returns the OCI state metadata for the container with the given ID.
func (c *Client) State(ctx context.Context, id string) (specs.State, error) {
	if err := ctx.Err(); err != nil {
		return specs.State{}, err
	}
	opts := c.baseLoadOpts()
	opts.ReadOnly = true
	cont, err := c.Load(ctx, id, opts)
	if err != nil {
		return specs.State{}, fmt.Errorf("loading container %q: %w", id, err)
	}
	return c.StateContainer(cont), nil
}

// StateContainer returns the OCI state metadata for an already-loaded container.
func (c *Client) StateContainer(cont *container.Container) specs.State {
	return cont.State()
}

// Spec returns the OCI specification for the container with the given ID.
func (c *Client) Spec(ctx context.Context, id string) (*specs.Spec, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	opts := c.baseLoadOpts()
	opts.ReadOnly = true
	opts.SkipCheck = true
	cont, err := c.Load(ctx, id, opts)
	if err != nil {
		return nil, fmt.Errorf("loading container %q: %w", id, err)
	}
	return c.SpecContainer(cont), nil
}

// SpecContainer returns the OCI specification for an already-loaded container.
func (c *Client) SpecContainer(cont *container.Container) *specs.Spec {
	return cont.Spec
}

// Pause pauses the container with the given ID.
func (c *Client) Pause(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cont, err := c.Load(ctx, id, c.baseLoadOpts())
	if err != nil {
		return fmt.Errorf("loading container %q: %w", id, err)
	}
	return c.PauseContainer(ctx, cont)
}

// PauseContainer pauses an already-loaded container.
func (c *Client) PauseContainer(ctx context.Context, cont *container.Container) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return cont.Pause()
}

// Resume resumes the container with the given ID.
func (c *Client) Resume(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cont, err := c.Load(ctx, id, c.baseLoadOpts())
	if err != nil {
		return fmt.Errorf("loading container %q: %w", id, err)
	}
	return c.ResumeContainer(ctx, cont)
}

// ResumeContainer resumes an already-loaded container.
func (c *Client) ResumeContainer(ctx context.Context, cont *container.Container) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return cont.Resume()
}

// DeleteOpts configures container deletion behavior.
type DeleteOpts struct {
	// Force specifies whether to force-terminate running containers.
	Force bool
}

// Delete deletes resources held by the container with the given ID.
func (c *Client) Delete(ctx context.Context, id string, opts DeleteOpts) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cont, err := c.Load(ctx, id, c.baseLoadOpts())
	if err != nil {
		if os.IsNotExist(err) && opts.Force {
			return nil
		}
		return fmt.Errorf("loading container %q: %w", id, err)
	}
	return c.DeleteContainer(ctx, cont, opts)
}

// DeleteContainer deletes resources held by an already-loaded container.
func (c *Client) DeleteContainer(ctx context.Context, cont *container.Container, opts DeleteOpts) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !opts.Force && cont.Status != container.Created && cont.Status != container.Stopped {
		if err := RequireForce(c.conf.RootDir, c.conf, cont); err != nil {
			return err
		}
	}
	if err := cont.Destroy(); err != nil {
		return fmt.Errorf("destroying container %q: %w", cont.ID, err)
	}
	return nil
}

// RequireForce returns an error unless the target is a sandbox with no root
// container and no containers running.
func RequireForce(rootDir string, conf *config.Config, c *container.Container) error {
	if c.Sandbox == nil || !c.Sandbox.NoRootContainer || !c.Sandbox.IsRootContainer(c.ID) {
		return fmt.Errorf("cannot delete container that is not stopped without --force flag")
	}
	confRootDir := rootDir
	if conf != nil && conf.RootDir != "" {
		confRootDir = conf.RootDir
	}
	containers, err := container.LoadSandbox(confRootDir, c.Sandbox.ID, container.LoadOpts{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("loading containers of sandbox %q: %w", c.Sandbox.ID, err)
	}
	for _, sc := range containers {
		if sc.ID == c.ID {
			continue
		}
		// LoadSandbox does not refresh the status.
		if sc.Status == container.Running {
			if err := sc.CheckStopped(); err != nil {
				return fmt.Errorf("checking status of container %q: %w", sc.ID, err)
			}
		}
		if sc.Status == container.Running || sc.Status == container.Paused {
			return fmt.Errorf("cannot delete sandbox %q while container %q is running without --force flag", c.Sandbox.ID, sc.ID)
		}
	}
	return nil
}

// KillOpts configures signal delivery parameters.
type KillOpts struct {
	// Signal is the signal to deliver. If 0, unix.SIGTERM is used by default.
	Signal unix.Signal

	// All delivers the signal to all processes inside the container.
	All bool

	// PID targets a specific process relative to the container's root PID namespace.
	PID int32

	// PGID targets all processes in the given process group.
	PGID int32
}

// Kill delivers a signal to the container or its processes.
func (c *Client) Kill(ctx context.Context, id string, opts KillOpts) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cont, err := c.Load(ctx, id, c.baseLoadOpts())
	if err != nil {
		return fmt.Errorf("loading container %q: %w", id, err)
	}
	return c.KillContainer(ctx, cont, opts)
}

// KillContainer delivers a signal to an already-loaded container or its processes.
func (c *Client) KillContainer(ctx context.Context, cont *container.Container, opts KillOpts) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	modes := 0
	if opts.All {
		modes++
	}
	if opts.PID != 0 {
		modes++
	}
	if opts.PGID != 0 {
		modes++
	}
	if modes > 1 {
		return fmt.Errorf("it is invalid to specify more than one of All, PID, and PGID")
	}

	sig := opts.Signal
	if sig == 0 {
		sig = unix.SIGTERM
	}

	if opts.PID != 0 {
		if err := cont.SignalProcess(syscall.Signal(sig), opts.PID); err != nil {
			return fmt.Errorf("failed to signal pid %d: %w", opts.PID, err)
		}
		return nil
	}
	if opts.PGID != 0 {
		if err := cont.SignalProcessGroup(syscall.Signal(sig), opts.PGID); err != nil {
			return fmt.Errorf("failed to signal process group %d: %w", opts.PGID, err)
		}
		return nil
	}
	return cont.SignalContainer(syscall.Signal(sig), opts.All)
}

// ParseSignal parses a signal name (e.g. "KILL", "SIGTERM", "TERM") or numeric string into a unix.Signal.
func ParseSignal(s string) (unix.Signal, error) {
	n, err := strconv.Atoi(s)
	if err == nil {
		sig := unix.Signal(n)
		for _, msig := range signalMap {
			if sig == msig {
				return sig, nil
			}
		}
		return -1, fmt.Errorf("unknown signal %q", s)
	}
	if sig, ok := signalMap[strings.TrimPrefix(strings.ToUpper(s), "SIG")]; ok {
		return sig, nil
	}
	return -1, fmt.Errorf("unknown signal %q", s)
}

var signalMap = map[string]unix.Signal{
	"ABRT":   unix.SIGABRT,
	"ALRM":   unix.SIGALRM,
	"BUS":    unix.SIGBUS,
	"CHLD":   unix.SIGCHLD,
	"CLD":    unix.SIGCLD,
	"CONT":   unix.SIGCONT,
	"FPE":    unix.SIGFPE,
	"HUP":    unix.SIGHUP,
	"ILL":    unix.SIGILL,
	"INT":    unix.SIGINT,
	"IO":     unix.SIGIO,
	"IOT":    unix.SIGIOT,
	"KILL":   unix.SIGKILL,
	"PIPE":   unix.SIGPIPE,
	"POLL":   unix.SIGPOLL,
	"PROF":   unix.SIGPROF,
	"PWR":    unix.SIGPWR,
	"QUIT":   unix.SIGQUIT,
	"SEGV":   unix.SIGSEGV,
	"STKFLT": unix.SIGSTKFLT,
	"STOP":   unix.SIGSTOP,
	"SYS":    unix.SIGSYS,
	"TERM":   unix.SIGTERM,
	"TRAP":   unix.SIGTRAP,
	"TSTP":   unix.SIGTSTP,
	"TTIN":   unix.SIGTTIN,
	"TTOU":   unix.SIGTTOU,
	"URG":    unix.SIGURG,
	"USR1":   unix.SIGUSR1,
	"USR2":   unix.SIGUSR2,
	"VTALRM": unix.SIGVTALRM,
	"WINCH":  unix.SIGWINCH,
	"XCPU":   unix.SIGXCPU,
	"XFSZ":   unix.SIGXFSZ,
}
