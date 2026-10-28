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
	"context"
	"fmt"

	"github.com/google/subcommands"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.dev/gvisor/runsc/client"
	"gvisor.dev/gvisor/runsc/cmd/util"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/container"
	"gvisor.dev/gvisor/runsc/flag"
)

// Kill implements subcommands.Command for the "kill" command.
type Kill struct {
	containerLoader
	all  bool
	pid  int
	pgid int
}

// Name implements subcommands.Command.Name.
func (*Kill) Name() string {
	return "kill"
}

// Synopsis implements subcommands.Command.Synopsis.
func (*Kill) Synopsis() string {
	return "sends a signal to the container"
}

// Usage implements subcommands.Command.Usage.
func (*Kill) Usage() string {
	return "kill <container id> [signal]\n"
}

// SetFlags implements subcommands.Command.SetFlags.
func (k *Kill) SetFlags(f *flag.FlagSet) {
	f.BoolVar(&k.all, "all", false, "send the specified signal to all processes inside the container")
	f.IntVar(&k.pid, "pid", 0, "send the specified signal to a specific process. pid is relative to the root PID namespace")
	f.IntVar(&k.pgid, "pgid", 0, "send the specified signal to all processes in the given process group. pgid is relative to the root PID namespace")
}

// FetchSpec implements util.SubCommand.FetchSpec.
func (k *Kill) FetchSpec(conf *config.Config, f *flag.FlagSet) (string, *specs.Spec, error) {
	c, err := k.loadContainer(conf, f, container.LoadOpts{})
	if err != nil {
		return "", nil, fmt.Errorf("loading container: %w", err)
	}
	return c.ID, c.Spec, nil
}

// Execute implements subcommands.Command.Execute.
func (k *Kill) Execute(ctx context.Context, f *flag.FlagSet, args ...any) subcommands.ExitStatus {
	if f.NArg() == 0 || f.NArg() > 2 {
		f.Usage()
		return subcommands.ExitUsageError
	}

	conf := args[0].(*config.Config)

	signalStr := f.Arg(1)
	if signalStr == "" {
		signalStr = "TERM"
	}

	sig, err := client.ParseSignal(signalStr)
	if err != nil {
		util.Fatalf("%v", err)
	}

	c, err := k.loadContainer(conf, f, container.LoadOpts{})
	if err != nil {
		util.Fatalf("loading container: %v", err)
	}

	cl := client.New(conf)
	if err := cl.KillContainer(ctx, c, client.KillOpts{
		Signal: sig,
		All:    k.all,
		PID:    int32(k.pid),
		PGID:   int32(k.pgid),
	}); err != nil {
		util.Fatalf("%v", err)
	}

	return subcommands.ExitSuccess
}
