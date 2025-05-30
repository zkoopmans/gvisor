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

package tun

import (
	"fmt"

	"gvisor.dev/gvisor/pkg/atomicbitops"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/packetsocket"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/waiter"
)

const (
	// drivers/net/tun.c:tun_net_init()
	defaultDevMtu = 1500

	// Queue length for outbound packet, arriving at fd side for read. Overflow
	// causes packet drops. gVisor implementation-specific.
	defaultDevOutQueueLen = 1024
)

var zeroMAC [6]byte

// Device is an opened /dev/net/tun device.
//
// +stateify savable
type Device struct {
	waiter.Queue

	mu           deviceRWMutex `state:"nosave"`
	endpoint     *tunEndpoint
	notifyHandle *channel.NotificationHandle
	flags        Flags
}

// Flags set properties of a Device
//
// +stateify savable
type Flags struct {
	TUN          bool
	TAP          bool
	NoPacketInfo bool
	Exclusive    bool
}

// beforeSave is invoked by stateify.
func (d *Device) beforeSave() {
	d.mu.Lock()
	defer d.mu.Unlock()
	// TODO(b/110961832): Restore the device to stack. At this moment, the stack
	// is not savable.
	if d.endpoint != nil {
		panic("/dev/net/tun does not support save/restore when a device is associated with it.")
	}
}

func (d *Device) SetPersistent(v bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.endpoint == nil {
		return linuxerr.EBADFD
	}

	d.endpoint.setPersistent(v)

	return nil
}

// Release implements fs.FileOperations.Release.
func (d *Device) Release(ctx context.Context) {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Decrease refcount if there is an endpoint associated with this file.
	if d.endpoint != nil {
		d.endpoint.Drain()
		d.endpoint.RemoveNotify(d.notifyHandle)
		d.endpoint.DecRef(ctx)
		d.endpoint = nil
	}
}

// SetIff services TUNSETIFF ioctl(2) request.
func (d *Device) SetIff(ctx context.Context, s *stack.Stack, name string, flags Flags) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.endpoint != nil {
		return linuxerr.EINVAL
	}

	// Input validation.
	if (flags.TAP && flags.TUN) || (!flags.TAP && !flags.TUN) {
		return linuxerr.EINVAL
	}

	prefix := "tun"
	if flags.TAP {
		prefix = "tap"
	}

	linkCaps := stack.CapabilityNone
	if flags.TAP {
		linkCaps |= stack.CapabilityResolutionRequired
	}

	endpoint, err := attachOrCreateNIC(ctx, s, name, prefix, linkCaps, flags)
	if err != nil {
		return err
	}

	d.endpoint = endpoint
	d.notifyHandle = d.endpoint.AddNotify(d)
	d.flags = flags
	return nil
}

func attachOrCreateNIC(ctx context.Context, s *stack.Stack, name, prefix string, linkCaps stack.LinkEndpointCapabilities, flags Flags) (*tunEndpoint, error) {
	for {
		// 1. Try to attach to an existing NIC.
		if name != "" && !flags.Exclusive {
			if linkEP := s.GetLinkEndpointByName(name); linkEP != nil {
				packetEndpoint, ok := linkEP.(*packetsocket.Endpoint)
				if !ok {
					// Not a NIC created by tun device.
					return nil, linuxerr.EOPNOTSUPP
				}
				endpoint, ok := packetEndpoint.Child().(*tunEndpoint)
				if !ok {
					// Not a NIC created by tun device.
					return nil, linuxerr.EOPNOTSUPP
				}
				if !endpoint.TryIncRef() {
					// Race detected: NIC got deleted in between.
					continue
				}
				return endpoint, nil
			}
		}

		// 2. Creating a new NIC.
		id := s.NextNICID()
		endpoint := &tunEndpoint{
			Endpoint: channel.New(defaultDevOutQueueLen, defaultDevMtu, ""),
			stack:    s,
			nicID:    id,
			name:     name,
			isTap:    prefix == "tap",
		}
		endpoint.InitRefs()
		endpoint.Endpoint.LinkEPCapabilities = linkCaps
		if endpoint.name == "" {
			endpoint.name = fmt.Sprintf("%s%d", prefix, id)
		}
		err := s.CreateNICWithOptions(endpoint.nicID, packetsocket.New(endpoint), stack.NICOptions{
			Name: endpoint.name,
		})
		switch err.(type) {
		case nil:
			return endpoint, nil
		case *tcpip.ErrDuplicateNICID:
			endpoint.DecRef(ctx)
			if !flags.Exclusive {
				// Race detected: A NIC has been created in between.
				continue
			}
			return nil, linuxerr.EEXIST
		default:
			endpoint.DecRef(ctx)
			return nil, linuxerr.EINVAL
		}
	}
}

// MTU returns the tun endpoint MTU (maximum transmission unit).
func (d *Device) MTU() (uint32, error) {
	d.mu.RLock()
	endpoint := d.endpoint
	d.mu.RUnlock()
	if endpoint == nil {
		return 0, linuxerr.EBADFD
	}
	if !endpoint.IsAttached() {
		return 0, linuxerr.EIO
	}
	return endpoint.MTU(), nil
}

// Write inject one inbound packet to the network interface.
func (d *Device) Write(data *buffer.View) (int64, error) {
	d.mu.RLock()
	endpoint := d.endpoint
	d.mu.RUnlock()
	if endpoint == nil {
		return 0, linuxerr.EBADFD
	}
	if !endpoint.IsAttached() {
		return 0, linuxerr.EIO
	}

	dataLen := int64(data.Size())

	// Packet information.
	var pktInfoHdr PacketInfoHeader
	if !d.flags.NoPacketInfo {
		if dataLen < PacketInfoHeaderSize {
			// Ignore bad packet.
			return dataLen, nil
		}
		pktInfoHdrView := data.Clone()
		defer pktInfoHdrView.Release()
		pktInfoHdrView.CapLength(PacketInfoHeaderSize)
		pktInfoHdr = PacketInfoHeader(pktInfoHdrView.AsSlice())
		data.TrimFront(PacketInfoHeaderSize)
	}

	// Ethernet header (TAP only).
	var ethHdr header.Ethernet
	if d.flags.TAP {
		if data.Size() < header.EthernetMinimumSize {
			// Ignore bad packet.
			return dataLen, nil
		}
		ethHdrView := data.Clone()
		defer ethHdrView.Release()
		ethHdrView.CapLength(header.EthernetMinimumSize)
		ethHdr = header.Ethernet(ethHdrView.AsSlice())
		data.TrimFront(header.EthernetMinimumSize)
	}

	// Try to determine network protocol number, default zero.
	var protocol tcpip.NetworkProtocolNumber
	switch {
	case pktInfoHdr != nil:
		protocol = pktInfoHdr.Protocol()
	case ethHdr != nil:
		protocol = ethHdr.Type()
	case d.flags.TUN:
		// TUN interface with IFF_NO_PI enabled, thus
		// we need to determine protocol from version field
		version := data.AsSlice()[0] >> 4
		if version == 4 {
			protocol = header.IPv4ProtocolNumber
		} else if version == 6 {
			protocol = header.IPv6ProtocolNumber
		}
	}

	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
		ReserveHeaderBytes: len(ethHdr),
		Payload:            buffer.MakeWithView(data.Clone()),
	})
	defer pkt.DecRef()
	copy(pkt.LinkHeader().Push(len(ethHdr)), ethHdr)
	endpoint.InjectInbound(protocol, pkt)
	return dataLen, nil
}

// Read reads one outgoing packet from the network interface.
func (d *Device) Read() (*buffer.View, error) {
	d.mu.RLock()
	endpoint := d.endpoint
	d.mu.RUnlock()
	if endpoint == nil {
		return nil, linuxerr.EBADFD
	}

	pkt := endpoint.Read()
	if pkt == nil {
		return nil, linuxerr.ErrWouldBlock
	}
	v := d.encodePkt(pkt)
	pkt.DecRef()
	return v, nil
}

// encodePkt encodes packet for fd side.
func (d *Device) encodePkt(pkt *stack.PacketBuffer) *buffer.View {
	var view *buffer.View

	// Packet information.
	if !d.flags.NoPacketInfo {
		view = buffer.NewView(PacketInfoHeaderSize + pkt.Size())
		view.Grow(PacketInfoHeaderSize)
		hdr := PacketInfoHeader(view.AsSlice())
		hdr.Encode(&PacketInfoFields{
			Protocol: pkt.NetworkProtocolNumber,
		})
		pktView := pkt.ToView()
		view.Write(pktView.AsSlice())
		pktView.Release()
	} else {
		view = pkt.ToView()
	}

	return view
}

// Name returns the name of the attached network interface. Empty string if
// unattached.
func (d *Device) Name() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.endpoint != nil {
		return d.endpoint.name
	}
	return ""
}

// Flags returns the flags set for d. Zero value if unset.
func (d *Device) Flags() Flags {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.flags
}

// Readiness implements watier.Waitable.Readiness.
func (d *Device) Readiness(mask waiter.EventMask) waiter.EventMask {
	if mask&waiter.ReadableEvents != 0 {
		d.mu.RLock()
		endpoint := d.endpoint
		d.mu.RUnlock()
		if endpoint != nil && endpoint.NumQueued() == 0 {
			mask &= ^waiter.ReadableEvents
		}
	}
	return mask & (waiter.ReadableEvents | waiter.WritableEvents)
}

// WriteNotify implements channel.Notification.WriteNotify.
func (d *Device) WriteNotify() {
	d.Notify(waiter.ReadableEvents)
}

// tunEndpoint is the link endpoint for the NIC created by the tun device.
//
// It is ref-counted as multiple opening files can attach to the same NIC.
// The last owner is responsible for deleting the NIC.
//
// +stateify savable
type tunEndpoint struct {
	tunEndpointRefs
	*channel.Endpoint

	stack      *stack.Stack
	nicID      tcpip.NICID
	name       string
	isTap      bool
	persistent atomicbitops.Bool
	closed     atomicbitops.Bool

	mu            endpointMutex `state:"nosave"`
	onCloseAction func()        `state:"nosave"`
}

func (e *tunEndpoint) setPersistent(v bool) {
	old := e.persistent.Swap(v)
	if old == v {
		return
	}
	if v {
		e.IncRef()
	} else {
		e.DecRef(context.Background())
	}
}

func (e *tunEndpoint) Close() {
	if e.closed.Swap(true) {
		return
	}

	if e.persistent.Load() {
		e.DecRef(context.Background())
	}
	e.mu.Lock()
	action := e.onCloseAction
	e.onCloseAction = nil
	e.mu.Unlock()
	if action != nil {
		action()
	}
	e.Endpoint.Close()
}

// SetOnCloseAction implements stack.LinkEndpoint.
func (e *tunEndpoint) SetOnCloseAction(action func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.onCloseAction = action
}

// DecRef decrements refcount of e, removing NIC if it reaches 0.
func (e *tunEndpoint) DecRef(ctx context.Context) {
	e.tunEndpointRefs.DecRef(func() {
		e.Close()
	})
}

// ARPHardwareType implements stack.LinkEndpoint.ARPHardwareType.
func (e *tunEndpoint) ARPHardwareType() header.ARPHardwareType {
	if e.isTap {
		return header.ARPHardwareEther
	}
	return header.ARPHardwareNone
}

// AddHeader implements stack.LinkEndpoint.AddHeader.
func (e *tunEndpoint) AddHeader(pkt *stack.PacketBuffer) {
	if !e.isTap {
		return
	}
	eth := header.Ethernet(pkt.LinkHeader().Push(header.EthernetMinimumSize))
	eth.Encode(&header.EthernetFields{
		SrcAddr: pkt.EgressRoute.LocalLinkAddress,
		DstAddr: pkt.EgressRoute.RemoteLinkAddress,
		Type:    pkt.NetworkProtocolNumber,
	})
}

// MaxHeaderLength returns the maximum size of the link layer header.
func (e *tunEndpoint) MaxHeaderLength() uint16 {
	if e.isTap {
		return header.EthernetMinimumSize
	}
	return 0
}
