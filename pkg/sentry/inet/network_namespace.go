// Copyright 2020 The gVisor Authors.
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

package inet

// NSID is the network namespace ID type.
type NSID int32

// NetworkNamespace represents a network namespace. See network_namespaces(7).
//
// +stateify savable
type NetworkNamespace struct {
	// nsid is the network namespace ID. It is the thread ID of the creating
	// process in the root PID namespace.
	nsid NSID

	// stack is the network stack implementation of this network namespace.
	stack Stack `state:"nosave"`

	// creator allows kernel to create new network stack for network namespaces.
	// If nil, no networking will function if network is namespaced.
	creator NetworkStackCreator
}

// NewRootNetworkNamespace creates the root network namespace, with creator
// allowing new network namespace to be created. If creator is nil, no
// networking will function if network is namespaced.
func NewRootNetworkNamespace(stack Stack, creator NetworkStackCreator) *NetworkNamespace {
	return &NetworkNamespace{
		stack:   stack,
		creator: creator,
	}
}

// NewNetworkNamespace creates new network namespace from the root. nsid should
// be the creating thread ID in the root pid namespace.
func NewNetworkNamespace(root *NetworkNamespace, nsid NSID) *NetworkNamespace {
	n := &NetworkNamespace{
		nsid:    nsid,
		creator: root.creator,
	}
	n.init()
	return n
}

// ID returns the network namespace ID of n.
func (n *NetworkNamespace) ID() NSID {
	return n.nsid
}

// Stack returns the network stack of n. Stack may return nil if no network
// stack is configured.
func (n *NetworkNamespace) Stack() Stack {
	return n.stack
}

// IsRoot returns whether n is the root network namespace.
func (n *NetworkNamespace) IsRoot() bool {
	return n.nsid == 0
}

// RestoreRootStack restores the root network namespace with stack. This should
// only be called when restoring kernel.
func (n *NetworkNamespace) RestoreRootStack(stack Stack) {
	if n.nsid != 0 {
		panic("RestoreRootStack can only be called on root network namespace")
	}
	if n.stack != nil {
		panic("RestoreRootStack called after a stack has already been set")
	}
	n.stack = stack
}

func (n *NetworkNamespace) init() {
	// Root network namespace will have stack assigned later.
	if n.nsid == 0 {
		return
	}
	if n.creator != nil {
		var err error
		n.stack, err = n.creator.CreateStack()
		if err != nil {
			panic(err)
		}
	}
}

// afterLoad is invoked by stateify.
func (n *NetworkNamespace) afterLoad() {
	n.init()
}

// NetworkStackCreator allows new instance of network stack to be created. It is
// used by the kernel to create new network namespaces when requested.
type NetworkStackCreator interface {
	// CreateStack creates a new network stack for network namespace.
	CreateStack() (Stack, error)
}
