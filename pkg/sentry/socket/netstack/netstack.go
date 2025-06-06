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

// Package netstack provides an implementation of the socket.Socket interface
// that is backed by a tcpip.Endpoint.
//
// It does not depend on any particular endpoint implementation, and thus can
// be used to expose certain endpoints to the sentry while leaving others out,
// for example, TCP endpoints and Unix-domain endpoints.
//
// Lock ordering: netstack => mm: ioSequenceReadWriter copies user memory inside
// tcpip.Endpoint.Write(). Netstack is allowed to (and does) hold locks during
// this operation.
package netstack

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"reflect"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/abi/linux/errno"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/eventchannel"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/marshal"
	"gvisor.dev/gvisor/pkg/marshal/primitive"
	"gvisor.dev/gvisor/pkg/metric"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/sockfs"
	"gvisor.dev/gvisor/pkg/sentry/inet"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/ktime"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/socket"
	"gvisor.dev/gvisor/pkg/sentry/socket/netfilter"
	epb "gvisor.dev/gvisor/pkg/sentry/socket/netstack/events_go_proto"
	"gvisor.dev/gvisor/pkg/sentry/socket/netstack/packetmmap"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/sync"
	"gvisor.dev/gvisor/pkg/syserr"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/usermem"
	"gvisor.dev/gvisor/pkg/waiter"
)

const bitsPerUint32 = 32

// statCounterValue returns a function usable as callback function when defining a gVisor Sentry
// metric that contains the value counted by the StatCounter.
// This avoids a dependency loop in the tcpip package.
func statCounterValue(cm *tcpip.StatCounter) func(...*metric.FieldValue) uint64 {
	return func(...*metric.FieldValue) uint64 {
		return cm.Value()
	}
}

func mustCreateMetric(name, description string) *tcpip.StatCounter {
	var cm tcpip.StatCounter
	metric.MustRegisterCustomUint64Metric(name,
		metric.Uint64Metadata{
			Cumulative:  true,
			Description: description,
		}, statCounterValue(&cm))
	return &cm
}

func mustCreateGauge(name, description string) *tcpip.StatCounter {
	var cm tcpip.StatCounter
	metric.MustRegisterCustomUint64Metric(name,
		metric.Uint64Metadata{
			Cumulative:  false,
			Description: description,
		}, statCounterValue(&cm))
	return &cm
}

const (
	maxSocketOptionNameValue = 100
)

var (
	socketLevelSocketFieldValue = metric.FieldValue{"SOCKET"}
	socketLevelTCPFieldValue    = metric.FieldValue{"TCP"}
	socketLevelICMPV6FieldValue = metric.FieldValue{"ICMPV6"}
	socketLevelIPFieldValue     = metric.FieldValue{"IP"}
	socketLevelIPV6FieldValue   = metric.FieldValue{"IPV6"}
	socketLevelPacketFieldValue = metric.FieldValue{"PACKET"}

	allowedSocketOptionLevels     = []*metric.FieldValue{&socketLevelSocketFieldValue, &socketLevelTCPFieldValue, &socketLevelICMPV6FieldValue, &socketLevelIPFieldValue, &socketLevelIPV6FieldValue, &socketLevelPacketFieldValue}
	allowedSocketOptionNameValues = initSocketMetricFields()

	unimplementedSetSocketOptionMetric = mustCreateSocketMetric("/netstack/socket/unimplemented_set_socket_option", "Number of times SetSocketOption was called with an unimplemented option.", metric.NewField("level", allowedSocketOptionLevels[:]...), metric.NewField("name", allowedSocketOptionNameValues[:]...))
	unknownSetSocketOptionMetric       = mustCreateSocketMetric("/netstack/socket/unknown_set_socket_option", "Number of times SetSocketOption was called with an unknown option.", metric.NewField("level", allowedSocketOptionLevels[:]...))
)

func initSocketMetricFields() []*metric.FieldValue {
	values := make([]*metric.FieldValue, maxSocketOptionNameValue+1)
	for i := 0; i <= maxSocketOptionNameValue; i++ {
		values[i] = &metric.FieldValue{strconv.Itoa(i)}
	}
	return values
}

func mustCreateSocketMetric(name, description string, fields ...metric.Field) *metric.Uint64Metric {
	return metric.MustCreateNewUint64Metric(name,
		metric.Uint64Metadata{
			Cumulative:  true,
			Sync:        true,
			Description: description,
			Fields:      fields,
		})
}

// Metrics contains metrics exported by netstack.
var Metrics = tcpip.Stats{
	DroppedPackets: mustCreateMetric("/netstack/dropped_packets", "Number of packets dropped at the transport layer."),
	NICs: tcpip.NICStats{
		MalformedL4RcvdPackets: mustCreateMetric("/netstack/nic/malformed_l4_received_packets", "Number of packets received that failed L4 header parsing."),
		Tx: tcpip.NICPacketStats{
			Packets: mustCreateMetric("/netstack/nic/tx/packets", "Number of packets transmitted."),
			Bytes:   mustCreateMetric("/netstack/nic/tx/bytes", "Number of bytes transmitted."),
		},
		TxPacketsDroppedNoBufferSpace: mustCreateMetric("/netstack/nic/tx_packets_dropped_no_buffer_space", "Number of TX packets dropped as a result of no buffer space errors."),
		Rx: tcpip.NICPacketStats{
			Packets: mustCreateMetric("/netstack/nic/rx/packets", "Number of packets received."),
			Bytes:   mustCreateMetric("/netstack/nic/rx/bytes", "Number of bytes received."),
		},
		DisabledRx: tcpip.NICPacketStats{
			Packets: mustCreateMetric("/netstack/nic/disabled_rx/packets", "Number of packets received on disabled NICs."),
			Bytes:   mustCreateMetric("/netstack/nic/disabled_rx/bytes", "Number of bytes received on disabled NICs."),
		},
		Neighbor: tcpip.NICNeighborStats{
			UnreachableEntryLookups:                    mustCreateMetric("/netstack/nic/neighbor/unreachable_entry_loopups", "Number of lookups performed on a neighbor entry in Unreachable state."),
			DroppedConfirmationForNoninitiatedNeighbor: mustCreateMetric("/netstack/nic/neighbor/dropped_confirmation_for_noninitiated_neighbor", "Number of advertisements received that don't match an entry in the neighbor cache."),
			DroppedInvalidLinkAddressConfirmations:     mustCreateMetric("/netstack/nic/neighbor/dropped_invalid_link_address_confirmations", "Number of advertisements dropped because they have empty source link-layer addresses"),
		},
	},
	ICMP: tcpip.ICMPStats{
		V4: tcpip.ICMPv4Stats{
			PacketsSent: tcpip.ICMPv4SentPacketStats{
				ICMPv4PacketStats: tcpip.ICMPv4PacketStats{
					EchoRequest:    mustCreateMetric("/netstack/icmp/v4/packets_sent/echo_request", "Number of ICMPv4 echo request packets sent."),
					EchoReply:      mustCreateMetric("/netstack/icmp/v4/packets_sent/echo_reply", "Number of ICMPv4 echo reply packets sent."),
					DstUnreachable: mustCreateMetric("/netstack/icmp/v4/packets_sent/dst_unreachable", "Number of ICMPv4 destination unreachable packets sent."),
					SrcQuench:      mustCreateMetric("/netstack/icmp/v4/packets_sent/src_quench", "Number of ICMPv4 source quench packets sent."),
					Redirect:       mustCreateMetric("/netstack/icmp/v4/packets_sent/redirect", "Number of ICMPv4 redirect packets sent."),
					TimeExceeded:   mustCreateMetric("/netstack/icmp/v4/packets_sent/time_exceeded", "Number of ICMPv4 time exceeded packets sent."),
					ParamProblem:   mustCreateMetric("/netstack/icmp/v4/packets_sent/param_problem", "Number of ICMPv4 parameter problem packets sent."),
					Timestamp:      mustCreateMetric("/netstack/icmp/v4/packets_sent/timestamp", "Number of ICMPv4 timestamp packets sent."),
					TimestampReply: mustCreateMetric("/netstack/icmp/v4/packets_sent/timestamp_reply", "Number of ICMPv4 timestamp reply packets sent."),
					InfoRequest:    mustCreateMetric("/netstack/icmp/v4/packets_sent/info_request", "Number of ICMPv4 information request packets sent."),
					InfoReply:      mustCreateMetric("/netstack/icmp/v4/packets_sent/info_reply", "Number of ICMPv4 information reply packets sent."),
				},
				Dropped:     mustCreateMetric("/netstack/icmp/v4/packets_sent/dropped", "Number of ICMPv4 packets dropped due to link layer errors."),
				RateLimited: mustCreateMetric("/netstack/icmp/v4/packets_sent/rate_limited", "Number of ICMPv4 packets dropped due to rate limit being exceeded."),
			},
			PacketsReceived: tcpip.ICMPv4ReceivedPacketStats{
				ICMPv4PacketStats: tcpip.ICMPv4PacketStats{
					EchoRequest:    mustCreateMetric("/netstack/icmp/v4/packets_received/echo_request", "Number of ICMPv4 echo request packets received."),
					EchoReply:      mustCreateMetric("/netstack/icmp/v4/packets_received/echo_reply", "Number of ICMPv4 echo reply packets received."),
					DstUnreachable: mustCreateMetric("/netstack/icmp/v4/packets_received/dst_unreachable", "Number of ICMPv4 destination unreachable packets received."),
					SrcQuench:      mustCreateMetric("/netstack/icmp/v4/packets_received/src_quench", "Number of ICMPv4 source quench packets received."),
					Redirect:       mustCreateMetric("/netstack/icmp/v4/packets_received/redirect", "Number of ICMPv4 redirect packets received."),
					TimeExceeded:   mustCreateMetric("/netstack/icmp/v4/packets_received/time_exceeded", "Number of ICMPv4 time exceeded packets received."),
					ParamProblem:   mustCreateMetric("/netstack/icmp/v4/packets_received/param_problem", "Number of ICMPv4 parameter problem packets received."),
					Timestamp:      mustCreateMetric("/netstack/icmp/v4/packets_received/timestamp", "Number of ICMPv4 timestamp packets received."),
					TimestampReply: mustCreateMetric("/netstack/icmp/v4/packets_received/timestamp_reply", "Number of ICMPv4 timestamp reply packets received."),
					InfoRequest:    mustCreateMetric("/netstack/icmp/v4/packets_received/info_request", "Number of ICMPv4 information request packets received."),
					InfoReply:      mustCreateMetric("/netstack/icmp/v4/packets_received/info_reply", "Number of ICMPv4 information reply packets received."),
				},
				Invalid: mustCreateMetric("/netstack/icmp/v4/packets_received/invalid", "Number of ICMPv4 packets received that the transport layer could not parse."),
			},
		},
		V6: tcpip.ICMPv6Stats{
			PacketsSent: tcpip.ICMPv6SentPacketStats{
				ICMPv6PacketStats: tcpip.ICMPv6PacketStats{
					EchoRequest:             mustCreateMetric("/netstack/icmp/v6/packets_sent/echo_request", "Number of ICMPv6 echo request packets sent."),
					EchoReply:               mustCreateMetric("/netstack/icmp/v6/packets_sent/echo_reply", "Number of ICMPv6 echo reply packets sent."),
					DstUnreachable:          mustCreateMetric("/netstack/icmp/v6/packets_sent/dst_unreachable", "Number of ICMPv6 destination unreachable packets sent."),
					PacketTooBig:            mustCreateMetric("/netstack/icmp/v6/packets_sent/packet_too_big", "Number of ICMPv6 packet too big packets sent."),
					TimeExceeded:            mustCreateMetric("/netstack/icmp/v6/packets_sent/time_exceeded", "Number of ICMPv6 time exceeded packets sent."),
					ParamProblem:            mustCreateMetric("/netstack/icmp/v6/packets_sent/param_problem", "Number of ICMPv6 parameter problem packets sent."),
					RouterSolicit:           mustCreateMetric("/netstack/icmp/v6/packets_sent/router_solicit", "Number of ICMPv6 router solicit packets sent."),
					RouterAdvert:            mustCreateMetric("/netstack/icmp/v6/packets_sent/router_advert", "Number of ICMPv6 router advert packets sent."),
					NeighborSolicit:         mustCreateMetric("/netstack/icmp/v6/packets_sent/neighbor_solicit", "Number of ICMPv6 neighbor solicit packets sent."),
					NeighborAdvert:          mustCreateMetric("/netstack/icmp/v6/packets_sent/neighbor_advert", "Number of ICMPv6 neighbor advert packets sent."),
					RedirectMsg:             mustCreateMetric("/netstack/icmp/v6/packets_sent/redirect_msg", "Number of ICMPv6 redirect message packets sent."),
					MulticastListenerQuery:  mustCreateMetric("/netstack/icmp/v6/packets_sent/multicast_listener_query", "Number of ICMPv6 multicast listener query packets sent."),
					MulticastListenerReport: mustCreateMetric("/netstack/icmp/v6/packets_sent/multicast_listener_report", "Number of ICMPv6 multicast listener report packets sent."),
					MulticastListenerDone:   mustCreateMetric("/netstack/icmp/v6/packets_sent/multicast_listener_done", "Number of ICMPv6 multicast listener done packets sent."),
				},
				Dropped:     mustCreateMetric("/netstack/icmp/v6/packets_sent/dropped", "Number of ICMPv6 packets dropped due to link layer errors."),
				RateLimited: mustCreateMetric("/netstack/icmp/v6/packets_sent/rate_limited", "Number of ICMPv6 packets dropped due to rate limit being exceeded."),
			},
			PacketsReceived: tcpip.ICMPv6ReceivedPacketStats{
				ICMPv6PacketStats: tcpip.ICMPv6PacketStats{
					EchoRequest:             mustCreateMetric("/netstack/icmp/v6/packets_received/echo_request", "Number of ICMPv6 echo request packets received."),
					EchoReply:               mustCreateMetric("/netstack/icmp/v6/packets_received/echo_reply", "Number of ICMPv6 echo reply packets received."),
					DstUnreachable:          mustCreateMetric("/netstack/icmp/v6/packets_received/dst_unreachable", "Number of ICMPv6 destination unreachable packets received."),
					PacketTooBig:            mustCreateMetric("/netstack/icmp/v6/packets_received/packet_too_big", "Number of ICMPv6 packet too big packets received."),
					TimeExceeded:            mustCreateMetric("/netstack/icmp/v6/packets_received/time_exceeded", "Number of ICMPv6 time exceeded packets received."),
					ParamProblem:            mustCreateMetric("/netstack/icmp/v6/packets_received/param_problem", "Number of ICMPv6 parameter problem packets received."),
					RouterSolicit:           mustCreateMetric("/netstack/icmp/v6/packets_received/router_solicit", "Number of ICMPv6 router solicit packets received."),
					RouterAdvert:            mustCreateMetric("/netstack/icmp/v6/packets_received/router_advert", "Number of ICMPv6 router advert packets received."),
					NeighborSolicit:         mustCreateMetric("/netstack/icmp/v6/packets_received/neighbor_solicit", "Number of ICMPv6 neighbor solicit packets received."),
					NeighborAdvert:          mustCreateMetric("/netstack/icmp/v6/packets_received/neighbor_advert", "Number of ICMPv6 neighbor advert packets received."),
					RedirectMsg:             mustCreateMetric("/netstack/icmp/v6/packets_received/redirect_msg", "Number of ICMPv6 redirect message packets received."),
					MulticastListenerQuery:  mustCreateMetric("/netstack/icmp/v6/packets_received/multicast_listener_query", "Number of ICMPv6 multicast listener query packets received."),
					MulticastListenerReport: mustCreateMetric("/netstack/icmp/v6/packets_received/multicast_listener_report", "Number of ICMPv6 multicast listener report packets sent."),
					MulticastListenerDone:   mustCreateMetric("/netstack/icmp/v6/packets_received/multicast_listener_done", "Number of ICMPv6 multicast listener done packets sent."),
				},
				Unrecognized:                   mustCreateMetric("/netstack/icmp/v6/packets_received/unrecognized", "Number of ICMPv6 packets received that the transport layer does not know how to parse."),
				Invalid:                        mustCreateMetric("/netstack/icmp/v6/packets_received/invalid", "Number of ICMPv6 packets received that the transport layer could not parse."),
				RouterOnlyPacketsDroppedByHost: mustCreateMetric("/netstack/icmp/v6/packets_received/router_only_packets_dropped_by_host", "Number of ICMPv6 packets dropped due to being router-specific packets."),
			},
		},
	},
	IGMP: tcpip.IGMPStats{
		PacketsSent: tcpip.IGMPSentPacketStats{
			IGMPPacketStats: tcpip.IGMPPacketStats{
				MembershipQuery:    mustCreateMetric("/netstack/igmp/packets_sent/membership_query", "Number of IGMP Membership Query messages sent."),
				V1MembershipReport: mustCreateMetric("/netstack/igmp/packets_sent/v1_membership_report", "Number of IGMPv1 Membership Report messages sent."),
				V2MembershipReport: mustCreateMetric("/netstack/igmp/packets_sent/v2_membership_report", "Number of IGMPv2 Membership Report messages sent."),
				LeaveGroup:         mustCreateMetric("/netstack/igmp/packets_sent/leave_group", "Number of IGMP Leave Group messages sent."),
			},
			Dropped: mustCreateMetric("/netstack/igmp/packets_sent/dropped", "Number of IGMP packets dropped due to link layer errors."),
		},
		PacketsReceived: tcpip.IGMPReceivedPacketStats{
			IGMPPacketStats: tcpip.IGMPPacketStats{
				MembershipQuery:    mustCreateMetric("/netstack/igmp/packets_received/membership_query", "Number of IGMP Membership Query messages received."),
				V1MembershipReport: mustCreateMetric("/netstack/igmp/packets_received/v1_membership_report", "Number of IGMPv1 Membership Report messages received."),
				V2MembershipReport: mustCreateMetric("/netstack/igmp/packets_received/v2_membership_report", "Number of IGMPv2 Membership Report messages received."),
				LeaveGroup:         mustCreateMetric("/netstack/igmp/packets_received/leave_group", "Number of IGMP Leave Group messages received."),
			},
			Invalid:        mustCreateMetric("/netstack/igmp/packets_received/invalid", "Number of IGMP packets received that could not be parsed."),
			ChecksumErrors: mustCreateMetric("/netstack/igmp/packets_received/checksum_errors", "Number of received IGMP packets with bad checksums."),
			Unrecognized:   mustCreateMetric("/netstack/igmp/packets_received/unrecognized", "Number of unrecognized IGMP packets received."),
		},
	},
	IP: tcpip.IPStats{
		PacketsReceived:                     mustCreateMetric("/netstack/ip/packets_received", "Number of IP packets received from the link layer in nic.DeliverNetworkPacket."),
		DisabledPacketsReceived:             mustCreateMetric("/netstack/ip/disabled_packets_received", "Number of IP packets received from the link layer when the IP layer is disabled."),
		InvalidDestinationAddressesReceived: mustCreateMetric("/netstack/ip/invalid_addresses_received", "Number of IP packets received with an unknown or invalid destination address."),
		InvalidSourceAddressesReceived:      mustCreateMetric("/netstack/ip/invalid_source_addresses_received", "Number of IP packets received with an unknown or invalid source address."),
		PacketsDelivered:                    mustCreateMetric("/netstack/ip/packets_delivered", "Number of incoming IP packets that are successfully delivered to the transport layer via HandlePacket."),
		PacketsSent:                         mustCreateMetric("/netstack/ip/packets_sent", "Number of IP packets sent via WritePacket."),
		OutgoingPacketErrors:                mustCreateMetric("/netstack/ip/outgoing_packet_errors", "Number of IP packets which failed to write to a link-layer endpoint."),
		MalformedPacketsReceived:            mustCreateMetric("/netstack/ip/malformed_packets_received", "Number of IP packets which failed IP header validation checks."),
		MalformedFragmentsReceived:          mustCreateMetric("/netstack/ip/malformed_fragments_received", "Number of IP fragments which failed IP fragment validation checks."),
		IPTablesPreroutingDropped:           mustCreateMetric("/netstack/ip/iptables/prerouting_dropped", "Number of IP packets dropped in the Prerouting chain."),
		IPTablesInputDropped:                mustCreateMetric("/netstack/ip/iptables/input_dropped", "Number of IP packets dropped in the Input chain."),
		IPTablesOutputDropped:               mustCreateMetric("/netstack/ip/iptables/output_dropped", "Number of IP packets dropped in the Output chain."),
		OptionTimestampReceived:             mustCreateMetric("/netstack/ip/options/timestamp_received", "Number of timestamp options found in received IP packets."),
		OptionRecordRouteReceived:           mustCreateMetric("/netstack/ip/options/record_route_received", "Number of record route options found in received IP packets."),
		OptionRouterAlertReceived:           mustCreateMetric("/netstack/ip/options/router_alert_received", "Number of router alert options found in received IP packets."),
		OptionUnknownReceived:               mustCreateMetric("/netstack/ip/options/unknown_received", "Number of unknown options found in received IP packets."),
		Forwarding: tcpip.IPForwardingStats{
			Unrouteable:            mustCreateMetric("/netstack/ip/forwarding/unrouteable", "Number of IP packets received which couldn't be routed and thus were not forwarded."),
			ExhaustedTTL:           mustCreateMetric("/netstack/ip/forwarding/exhausted_ttl", "Number of IP packets received which could not be forwarded due to an exhausted TTL."),
			LinkLocalSource:        mustCreateMetric("/netstack/ip/forwarding/link_local_source_address", "Number of IP packets received which could not be forwarded due to a link-local source address."),
			LinkLocalDestination:   mustCreateMetric("/netstack/ip/forwarding/link_local_destination_address", "Number of IP packets received which could not be forwarded due to a link-local destination address."),
			ExtensionHeaderProblem: mustCreateMetric("/netstack/ip/forwarding/extension_header_problem", "Number of IP packets received which could not be forwarded due to a problem processing their IPv6 extension headers."),
			PacketTooBig:           mustCreateMetric("/netstack/ip/forwarding/packet_too_big", "Number of IP packets received which could not be forwarded because they could not fit within the outgoing MTU."),
			HostUnreachable:        mustCreateMetric("/netstack/ip/forwarding/host_unreachable", "Number of IP packets received which could not be forwarded due to unresolvable next hop."),
			Errors:                 mustCreateMetric("/netstack/ip/forwarding/errors", "Number of IP packets which couldn't be forwarded."),
		},
	},
	ARP: tcpip.ARPStats{
		PacketsReceived:                                 mustCreateMetric("/netstack/arp/packets_received", "Number of ARP packets received from the link layer."),
		DisabledPacketsReceived:                         mustCreateMetric("/netstack/arp/disabled_packets_received", "Number of ARP packets received from the link layer when the ARP layer is disabled."),
		MalformedPacketsReceived:                        mustCreateMetric("/netstack/arp/malformed_packets_received", "Number of ARP packets which failed ARP header validation checks."),
		RequestsReceived:                                mustCreateMetric("/netstack/arp/requests_received", "Number of ARP requests received."),
		RequestsReceivedUnknownTargetAddress:            mustCreateMetric("/netstack/arp/requests_received_unknown_addr", "Number of ARP requests received with an unknown target address."),
		OutgoingRequestInterfaceHasNoLocalAddressErrors: mustCreateMetric("/netstack/arp/outgoing_requests_iface_has_no_addr", "Number of failed attempts to send an ARP request with an interface that has no network address."),
		OutgoingRequestBadLocalAddressErrors:            mustCreateMetric("/netstack/arp/outgoing_requests_invalid_local_addr", "Number of failed attempts to send an ARP request with a provided local address that is invalid."),
		OutgoingRequestsDropped:                         mustCreateMetric("/netstack/arp/outgoing_requests_dropped", "Number of ARP requests which failed to write to a link-layer endpoint."),
		OutgoingRequestsSent:                            mustCreateMetric("/netstack/arp/outgoing_requests_sent", "Number of ARP requests sent."),
		RepliesReceived:                                 mustCreateMetric("/netstack/arp/replies_received", "Number of ARP replies received."),
		OutgoingRepliesDropped:                          mustCreateMetric("/netstack/arp/outgoing_replies_dropped", "Number of ARP replies which failed to write to a link-layer endpoint."),
		OutgoingRepliesSent:                             mustCreateMetric("/netstack/arp/outgoing_replies_sent", "Number of ARP replies sent."),
	},
	TCP: tcpip.TCPStats{
		ActiveConnectionOpenings:           mustCreateMetric("/netstack/tcp/active_connection_openings", "Number of connections opened successfully via Connect."),
		PassiveConnectionOpenings:          mustCreateMetric("/netstack/tcp/passive_connection_openings", "Number of connections opened successfully via Listen."),
		CurrentEstablished:                 mustCreateGauge("/netstack/tcp/current_established", "Number of connections in ESTABLISHED state now."),
		CurrentConnected:                   mustCreateGauge("/netstack/tcp/current_open", "Number of connections that are in connected state."),
		EstablishedResets:                  mustCreateMetric("/netstack/tcp/established_resets", "Number of times TCP connections have made a direct transition to the CLOSED state from either the ESTABLISHED state or the CLOSE-WAIT state"),
		EstablishedClosed:                  mustCreateMetric("/netstack/tcp/established_closed", "Number of times established TCP connections made a transition to CLOSED state."),
		EstablishedTimedout:                mustCreateMetric("/netstack/tcp/established_timedout", "Number of times  an established connection was reset because of keep-alive time out."),
		ListenOverflowSynDrop:              mustCreateMetric("/netstack/tcp/listen_overflow_syn_drop", "Number of times the listen queue overflowed and a SYN was dropped."),
		ListenOverflowAckDrop:              mustCreateMetric("/netstack/tcp/listen_overflow_ack_drop", "Number of times the listen queue overflowed and the final ACK in the handshake was dropped."),
		ListenOverflowSynCookieSent:        mustCreateMetric("/netstack/tcp/listen_overflow_syn_cookie_sent", "Number of times a SYN cookie was sent."),
		ListenOverflowSynCookieRcvd:        mustCreateMetric("/netstack/tcp/listen_overflow_syn_cookie_rcvd", "Number of times a SYN cookie was received."),
		ListenOverflowInvalidSynCookieRcvd: mustCreateMetric("/netstack/tcp/listen_overflow_invalid_syn_cookie_rcvd", "Number of times an invalid SYN cookie was received."),
		FailedConnectionAttempts:           mustCreateMetric("/netstack/tcp/failed_connection_attempts", "Number of calls to Connect or Listen (active and passive openings, respectively) that end in an error."),
		ValidSegmentsReceived:              mustCreateMetric("/netstack/tcp/valid_segments_received", "Number of TCP segments received that the transport layer successfully parsed."),
		InvalidSegmentsReceived:            mustCreateMetric("/netstack/tcp/invalid_segments_received", "Number of TCP segments received that the transport layer could not parse."),
		SegmentsSent:                       mustCreateMetric("/netstack/tcp/segments_sent", "Number of TCP segments sent."),
		SegmentSendErrors:                  mustCreateMetric("/netstack/tcp/segment_send_errors", "Number of TCP segments failed to be sent."),
		ResetsSent:                         mustCreateMetric("/netstack/tcp/resets_sent", "Number of TCP resets sent."),
		ResetsReceived:                     mustCreateMetric("/netstack/tcp/resets_received", "Number of TCP resets received."),
		Retransmits:                        mustCreateMetric("/netstack/tcp/retransmits", "Number of TCP segments retransmitted."),
		FastRecovery:                       mustCreateMetric("/netstack/tcp/fast_recovery", "Number of times fast recovery was used to recover from packet loss."),
		SACKRecovery:                       mustCreateMetric("/netstack/tcp/sack_recovery", "Number of times SACK recovery was used to recover from packet loss."),
		TLPRecovery:                        mustCreateMetric("/netstack/tcp/tlp_recovery", "Number of times tail loss probe triggers recovery from tail loss."),
		SlowStartRetransmits:               mustCreateMetric("/netstack/tcp/slow_start_retransmits", "Number of segments retransmitted in slow start mode."),
		FastRetransmit:                     mustCreateMetric("/netstack/tcp/fast_retransmit", "Number of TCP segments which were fast retransmitted."),
		Timeouts:                           mustCreateMetric("/netstack/tcp/timeouts", "Number of times RTO expired."),
		ChecksumErrors:                     mustCreateMetric("/netstack/tcp/checksum_errors", "Number of segments dropped due to bad checksums."),
		FailedPortReservations:             mustCreateMetric("/netstack/tcp/failed_port_reservations", "Number of time TCP failed to reserve a port."),
		SegmentsAckedWithDSACK:             mustCreateMetric("/netstack/tcp/segments_acked_with_dsack", "Number of segments for which DSACK was received."),
		SpuriousRecovery:                   mustCreateMetric("/netstack/tcp/spurious_recovery", "Number of times the connection entered loss recovery spuriously."),
		SpuriousRTORecovery:                mustCreateMetric("/netstack/tcp/spurious_rto_recovery", "Number of times the connection entered RTO spuriously."),
		ForwardMaxInFlightDrop:             mustCreateMetric("/netstack/tcp/forward_max_in_flight_drop", "Number of connection requests dropped due to exceeding in-flight limit."),
	},
	UDP: tcpip.UDPStats{
		PacketsReceived:          mustCreateMetric("/netstack/udp/packets_received", "Number of UDP datagrams received via HandlePacket."),
		UnknownPortErrors:        mustCreateMetric("/netstack/udp/unknown_port_errors", "Number of incoming UDP datagrams dropped because they did not have a known destination port."),
		ReceiveBufferErrors:      mustCreateMetric("/netstack/udp/receive_buffer_errors", "Number of incoming UDP datagrams dropped due to the receiving buffer being in an invalid state."),
		MalformedPacketsReceived: mustCreateMetric("/netstack/udp/malformed_packets_received", "Number of incoming UDP datagrams dropped due to the UDP header being in a malformed state."),
		PacketsSent:              mustCreateMetric("/netstack/udp/packets_sent", "Number of UDP datagrams sent."),
		PacketSendErrors:         mustCreateMetric("/netstack/udp/packet_send_errors", "Number of UDP datagrams failed to be sent."),
		ChecksumErrors:           mustCreateMetric("/netstack/udp/checksum_errors", "Number of UDP datagrams dropped due to bad checksums."),
	},
}

// DefaultTTL is linux's default TTL. All network protocols in all stacks used
// with this package must have this value set as their default TTL.
const DefaultTTL = 64

const sizeOfInt32 int = 4

var errStackType = syserr.New("expected but did not receive a netstack.Stack", errno.EINVAL)

// commonEndpoint represents the intersection of a tcpip.Endpoint and a
// transport.Endpoint.
type commonEndpoint interface {
	// Readiness implements tcpip.Endpoint.Readiness and
	// transport.Endpoint.Readiness.
	Readiness(mask waiter.EventMask) waiter.EventMask

	// SetSockOpt implements tcpip.Endpoint.SetSockOpt and
	// transport.Endpoint.SetSockOpt.
	SetSockOpt(tcpip.SettableSocketOption) tcpip.Error

	// SetSockOptInt implements tcpip.Endpoint.SetSockOptInt and
	// transport.Endpoint.SetSockOptInt.
	SetSockOptInt(opt tcpip.SockOptInt, v int) tcpip.Error

	// GetSockOpt implements tcpip.Endpoint.GetSockOpt and
	// transport.Endpoint.GetSockOpt.
	GetSockOpt(tcpip.GettableSocketOption) tcpip.Error

	// GetSockOptInt implements tcpip.Endpoint.GetSockOptInt and
	// transport.Endpoint.GetSockOpt.
	GetSockOptInt(opt tcpip.SockOptInt) (int, tcpip.Error)

	// State returns a socket's lifecycle state. The returned value is
	// protocol-specific and is primarily used for diagnostics.
	State() uint32

	// LastError implements tcpip.Endpoint.LastError and
	// transport.Endpoint.LastError.
	LastError() tcpip.Error

	// SocketOptions implements tcpip.Endpoint.SocketOptions and
	// transport.Endpoint.SocketOptions.
	SocketOptions() *tcpip.SocketOptions
}

// sock encapsulates all the state needed to represent a network stack
// endpoint in the kernel context.
//
// +stateify savable
type sock struct {
	vfsfd vfs.FileDescription
	vfs.FileDescriptionDefaultImpl
	vfs.DentryMetadataFileDescriptionImpl
	vfs.LockFD
	socket.SendReceiveTimeout
	*waiter.Queue

	family   int
	Endpoint tcpip.Endpoint
	skType   linux.SockType
	protocol int

	namespace *inet.Namespace

	mu sync.Mutex `state:"nosave"`
	// readWriter is an optimization to avoid allocations.
	// +checklocks:mu
	readWriter usermem.IOSequenceReadWriter `state:"nosave"`

	// readMu protects access to the below fields.
	readMu sync.Mutex `state:"nosave"`

	// sockOptTimestamp corresponds to SO_TIMESTAMP. When true, timestamps
	// of returned messages can be returned via control messages. When
	// false, the same timestamp is instead stored and can be read via the
	// SIOCGSTAMP ioctl. It is protected by readMu. See socket(7).
	sockOptTimestamp bool
	// timestampValid indicates whether timestamp for SIOCGSTAMP has been
	// set. It is protected by readMu.
	timestampValid bool
	// timestamp holds the timestamp to use with SIOCTSTAMP. It is only
	// valid when timestampValid is true. It is protected by readMu.
	timestamp time.Time `state:".(int64)"`

	// TODO(b/153685824): Move this to SocketOptions.
	// sockOptInq corresponds to TCP_INQ.
	sockOptInq bool
}

var _ = socket.Socket(&sock{})

// New creates a new endpoint socket.
func New(t *kernel.Task, family int, skType linux.SockType, protocol int, queue *waiter.Queue, endpoint tcpip.Endpoint) (*vfs.FileDescription, *syserr.Error) {
	if skType == linux.SOCK_STREAM {
		endpoint.SocketOptions().SetDelayOption(true)
	}

	mnt := t.Kernel().SocketMount()
	d := sockfs.NewDentry(t, mnt)
	defer d.DecRef(t)

	namespace := t.NetworkNamespace()
	s := &sock{
		Queue:     queue,
		family:    family,
		Endpoint:  endpoint,
		skType:    skType,
		protocol:  protocol,
		namespace: namespace,
	}
	s.LockFD.Init(&vfs.FileLocks{})
	vfsfd := &s.vfsfd
	if err := vfsfd.Init(s, linux.O_RDWR, mnt, d, &vfs.FileDescriptionOptions{
		DenyPRead:         true,
		DenyPWrite:        true,
		UseDentryMetadata: true,
	}); err != nil {
		return nil, syserr.FromError(err)
	}
	namespace.IncRef()
	return vfsfd, nil
}

// Release implements vfs.FileDescriptionImpl.Release.
func (s *sock) Release(ctx context.Context) {
	kernel.KernelFromContext(ctx).DeleteSocket(&s.vfsfd)
	e, ch := waiter.NewChannelEntry(waiter.EventHUp | waiter.EventErr)
	s.EventRegister(&e)
	defer s.EventUnregister(&e)

	s.Endpoint.Close()

	// SO_LINGER option is valid only for TCP. For other socket types
	// return after endpoint close.
	if family, skType, _ := s.Type(); skType == linux.SOCK_STREAM && (family == linux.AF_INET || family == linux.AF_INET6) {
		v := s.Endpoint.SocketOptions().GetLinger()
		// The case for zero timeout is handled in tcp endpoint close function.
		// Close is blocked until either:
		// 1. The endpoint state is not in any of the states: FIN-WAIT1,
		// CLOSING and LAST_ACK.
		// 2. Timeout is reached.
		if v.Enabled && v.Timeout != 0 {
			t := kernel.TaskFromContext(ctx)
			start := t.Kernel().MonotonicClock().Now()
			deadline := start.Add(v.Timeout)
			_ = t.BlockWithDeadline(ch, true, deadline)
		}
	}
	s.namespace.DecRef(ctx)
}

// Epollable implements FileDescriptionImpl.Epollable.
func (s *sock) Epollable() bool {
	return true
}

// Read implements vfs.FileDescriptionImpl.
func (s *sock) Read(ctx context.Context, dst usermem.IOSequence, opts vfs.ReadOptions) (int64, error) {
	// All flags other than RWF_NOWAIT should be ignored.
	// TODO(gvisor.dev/issue/2601): Support RWF_NOWAIT.
	if opts.Flags != 0 {
		return 0, linuxerr.EOPNOTSUPP
	}

	if dst.NumBytes() == 0 {
		return 0, nil
	}
	n, _, _, _, _, err := s.nonBlockingRead(ctx, dst, false, false, false)
	if err == syserr.ErrWouldBlock {
		return int64(n), linuxerr.ErrWouldBlock
	}
	if err != nil {
		return 0, err.ToError()
	}
	return int64(n), nil
}

// Write implements vfs.FileDescriptionImpl.
func (s *sock) Write(ctx context.Context, src usermem.IOSequence, opts vfs.WriteOptions) (int64, error) {
	// All flags other than RWF_NOWAIT should be ignored.
	// TODO(gvisor.dev/issue/2601): Support RWF_NOWAIT.
	if opts.Flags != 0 {
		return 0, linuxerr.EOPNOTSUPP
	}

	var n int64
	var err tcpip.Error
	switch s.Endpoint.(type) {
	case *tcp.Endpoint:
		s.mu.Lock()
		s.readWriter.Init(ctx, src)
		n, err = s.Endpoint.Write(&s.readWriter, tcpip.WriteOptions{})
		s.mu.Unlock()
	default:
		n, err = s.Endpoint.Write(src.Reader(ctx), tcpip.WriteOptions{})
	}
	if _, ok := err.(*tcpip.ErrWouldBlock); ok {
		return 0, linuxerr.ErrWouldBlock
	}
	if err != nil {
		return 0, syserr.TranslateNetstackError(err).ToError()
	}

	if n < src.NumBytes() {
		return n, linuxerr.ErrWouldBlock
	}

	return n, nil
}

// Accept implements the linux syscall accept(2) for sockets backed by
// tcpip.Endpoint.
func (s *sock) Accept(t *kernel.Task, peerRequested bool, flags int, blocking bool) (int32, linux.SockAddr, uint32, *syserr.Error) {
	// Issue the accept request to get the new endpoint.
	var peerAddr *tcpip.FullAddress
	if peerRequested {
		peerAddr = &tcpip.FullAddress{}
	}
	ep, wq, terr := s.Endpoint.Accept(peerAddr)
	if terr != nil {
		if _, ok := terr.(*tcpip.ErrWouldBlock); !ok || !blocking {
			return 0, nil, 0, syserr.TranslateNetstackError(terr)
		}

		var err *syserr.Error
		ep, wq, err = s.blockingAccept(t, peerAddr)
		if err != nil {
			return 0, nil, 0, err
		}
	}

	ns, err := New(t, s.family, s.skType, s.protocol, wq, ep)
	if err != nil {
		return 0, nil, 0, err
	}
	defer ns.DecRef(t)

	if err := ns.SetStatusFlags(t, t.Credentials(), uint32(flags&linux.SOCK_NONBLOCK)); err != nil {
		return 0, nil, 0, syserr.FromError(err)
	}

	var addr linux.SockAddr
	var addrLen uint32
	if peerAddr != nil {
		// Get address of the peer and write it to peer slice.
		addr, addrLen = socket.ConvertAddress(s.family, *peerAddr)
	}

	fd, e := t.NewFDFrom(0, ns, kernel.FDFlags{
		CloseOnExec: flags&linux.SOCK_CLOEXEC != 0,
	})

	t.Kernel().RecordSocket(ns)

	return fd, addr, addrLen, syserr.FromError(e)
}

// GetSockOpt implements the linux syscall getsockopt(2) for sockets backed by
// tcpip.Endpoint.
func (s *sock) GetSockOpt(t *kernel.Task, level, name int, outPtr hostarch.Addr, outLen int) (marshal.Marshallable, *syserr.Error) {
	// TODO(b/78348848): Unlike other socket options, SO_TIMESTAMP is
	// implemented specifically for netstack.Socket rather than
	// commonEndpoint. commonEndpoint should be extended to support socket
	// options where the implementation is not shared, as unix sockets need
	// their own support for SO_TIMESTAMP.
	if level == linux.SOL_SOCKET && name == linux.SO_TIMESTAMP {
		if outLen < sizeOfInt32 {
			return nil, syserr.ErrInvalidArgument
		}
		val := primitive.Int32(0)
		s.readMu.Lock()
		defer s.readMu.Unlock()
		if s.sockOptTimestamp {
			val = 1
		}
		return &val, nil
	}
	if level == linux.SOL_TCP && name == linux.TCP_INQ {
		if outLen < sizeOfInt32 {
			return nil, syserr.ErrInvalidArgument
		}
		val := primitive.Int32(0)
		s.readMu.Lock()
		defer s.readMu.Unlock()
		if s.sockOptInq {
			val = 1
		}
		return &val, nil
	}

	return GetSockOpt(t, s, s.Endpoint, s.family, s.skType, level, name, outPtr, outLen)
}

// SetSockOpt implements the linux syscall setsockopt(2) for sockets backed by
// tcpip.Endpoint.
func (s *sock) SetSockOpt(t *kernel.Task, level int, name int, optVal []byte) *syserr.Error {
	// TODO(b/78348848): Unlike other socket options, SO_TIMESTAMP is
	// implemented specifically for netstack.Socket rather than
	// commonEndpoint. commonEndpoint should be extended to support socket
	// options where the implementation is not shared, as unix sockets need
	// their own support for SO_TIMESTAMP.
	if level == linux.SOL_SOCKET && name == linux.SO_TIMESTAMP {
		if len(optVal) < sizeOfInt32 {
			return syserr.ErrInvalidArgument
		}
		s.readMu.Lock()
		defer s.readMu.Unlock()
		s.sockOptTimestamp = hostarch.ByteOrder.Uint32(optVal) != 0
		return nil
	}
	if level == linux.SOL_TCP && name == linux.TCP_INQ {
		if len(optVal) < sizeOfInt32 {
			return syserr.ErrInvalidArgument
		}
		s.readMu.Lock()
		defer s.readMu.Unlock()
		s.sockOptInq = hostarch.ByteOrder.Uint32(optVal) != 0
		return nil
	}

	return SetSockOpt(t, s, s.Endpoint, level, name, optVal)
}

// minSockAddrLen returns the minimum length in bytes of a socket address for
// the socket's family.
func (s *sock) minSockAddrLen() int {
	const addressFamilySize = 2

	switch s.family {
	case linux.AF_UNIX:
		return addressFamilySize
	case linux.AF_INET:
		return linux.SockAddrInetSize
	case linux.AF_INET6:
		return linux.SockAddrInet6Size
	case linux.AF_PACKET:
		return linux.SockAddrLinkSize
	case linux.AF_UNSPEC:
		return addressFamilySize
	default:
		panic(fmt.Sprintf("s.family unrecognized = %d", s.family))
	}
}

func (s *sock) isPacketBased() bool {
	return s.skType == linux.SOCK_DGRAM || s.skType == linux.SOCK_SEQPACKET || s.skType == linux.SOCK_RDM || s.skType == linux.SOCK_RAW
}

// Readiness returns a mask of ready events for socket s.
func (s *sock) Readiness(mask waiter.EventMask) waiter.EventMask {
	return s.Endpoint.Readiness(mask)
}

// checkFamily returns true iff the specified address family may be used with
// the socket.
//
// If exact is true, then the specified address family must be an exact match
// with the socket's family.
func (s *sock) checkFamily(family uint16, exact bool) bool {
	if family == uint16(s.family) {
		return true
	}
	if !exact && family == linux.AF_INET && s.family == linux.AF_INET6 {
		if !s.Endpoint.SocketOptions().GetV6Only() {
			return true
		}
	}
	return false
}

// mapFamily maps the AF_INET ANY address to the IPv4-mapped IPv6 ANY if the
// receiver's family is AF_INET6.
//
// This is a hack to work around the fact that both IPv4 and IPv6 ANY are
// represented by the empty string.
//
// TODO(gvisor.dev/issue/1556): remove this function.
func (s *sock) mapFamily(addr tcpip.FullAddress, family uint16) tcpip.FullAddress {
	if addr.Addr.BitLen() == 0 && s.family == linux.AF_INET6 && family == linux.AF_INET {
		addr.Addr = tcpip.AddrFrom16([16]byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xff, 0xff, 0x00, 0x00, 0x00, 0x00})
	}
	return addr
}

// Connect implements the linux syscall connect(2) for sockets backed by
// tpcip.Endpoint.
func (s *sock) Connect(t *kernel.Task, sockaddr []byte, blocking bool) *syserr.Error {
	addr, family, err := socket.AddressAndFamily(sockaddr)
	if err != nil {
		return err
	}

	if family == linux.AF_UNSPEC {
		err := s.Endpoint.Disconnect()
		if _, ok := err.(*tcpip.ErrNotSupported); ok {
			return syserr.ErrAddressFamilyNotSupported
		}
		return syserr.TranslateNetstackError(err)
	}

	if !s.checkFamily(family, false /* exact */) {
		return syserr.ErrInvalidArgument
	}
	addr = s.mapFamily(addr, family)

	// Always return right away in the non-blocking case.
	if !blocking {
		return syserr.TranslateNetstackError(s.Endpoint.Connect(addr))
	}

	// Register for notification when the endpoint becomes writable, then
	// initiate the connection.
	e, ch := waiter.NewChannelEntry(waiter.WritableEvents)
	s.EventRegister(&e)
	defer s.EventUnregister(&e)

	switch err := s.Endpoint.Connect(addr); err.(type) {
	case *tcpip.ErrConnectStarted, *tcpip.ErrAlreadyConnecting:
	case *tcpip.ErrNoPortAvailable:
		if (s.family == unix.AF_INET || s.family == unix.AF_INET6) && s.skType == linux.SOCK_STREAM {
			// TCP unlike UDP returns EADDRNOTAVAIL when it can't
			// find an available local ephemeral port.
			return syserr.ErrAddressNotAvailable
		}
		return syserr.TranslateNetstackError(err)
	default:
		return syserr.TranslateNetstackError(err)
	}

	// It's pending, so we have to wait for a notification, and fetch the
	// result once the wait completes.
	if err := t.Block(ch); err != nil {
		return syserr.FromError(err)
	}

	// Call Connect() again after blocking to find connect's result.
	return syserr.TranslateNetstackError(s.Endpoint.Connect(addr))
}

// Bind implements the linux syscall bind(2) for sockets backed by
// tcpip.Endpoint.
func (s *sock) Bind(_ *kernel.Task, sockaddr []byte) *syserr.Error {
	if len(sockaddr) < 2 {
		return syserr.ErrInvalidArgument
	}

	family := hostarch.ByteOrder.Uint16(sockaddr)
	var addr tcpip.FullAddress

	// Bind for AF_PACKET requires only family, protocol and ifindex.
	// In function AddressAndFamily, we check the address length which is
	// not needed for AF_PACKET bind.
	if family == linux.AF_PACKET {
		var a linux.SockAddrLink
		if len(sockaddr) < linux.SockAddrLinkSize {
			return syserr.ErrInvalidArgument
		}
		a.UnmarshalBytes(sockaddr)

		addr = tcpip.FullAddress{
			NIC: tcpip.NICID(a.InterfaceIndex),
			Addr: tcpip.AddrFrom16Slice(append(
				a.HardwareAddr[:header.EthernetAddressSize],
				[]byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}...,
			)),
			Port: socket.Ntohs(a.Protocol),
		}
	} else {
		if s.minSockAddrLen() > len(sockaddr) {
			return syserr.ErrInvalidArgument
		}

		var err *syserr.Error
		addr, family, err = socket.AddressAndFamily(sockaddr)
		if err != nil {
			return err
		}

		if !s.checkFamily(family, true /* exact */) {
			return syserr.ErrAddressFamilyNotSupported
		}

		addr = s.mapFamily(addr, family)
	}

	// Issue the bind request to the endpoint.
	err := s.Endpoint.Bind(addr)
	if _, ok := err.(*tcpip.ErrNoPortAvailable); ok {
		// Bind always returns EADDRINUSE irrespective of if the specified port was
		// already bound or if an ephemeral port was requested but none were
		// available.
		//
		// *tcpip.ErrNoPortAvailable is mapped to EAGAIN in syserr package because
		// UDP connect returns EAGAIN on ephemeral port exhaustion.
		//
		// TCP connect returns EADDRNOTAVAIL on ephemeral port exhaustion.
		err = &tcpip.ErrPortInUse{}
	}

	return syserr.TranslateNetstackError(err)
}

// Listen implements the linux syscall listen(2) for sockets backed by
// tcpip.Endpoint.
func (s *sock) Listen(_ *kernel.Task, backlog int) *syserr.Error {
	if err := s.Endpoint.Listen(backlog); err != nil {
		return syserr.TranslateNetstackError(err)
	}
	if !socket.IsTCP(s) {
		return nil
	}

	// Emit SentryTCPListenEvent with the bound port for tcp sockets.
	addr, err := s.Endpoint.GetLocalAddress()
	if err != nil {
		panic(fmt.Sprintf("GetLocalAddress failed for tcp socket: %s", err))
	}
	eventchannel.Emit(&epb.SentryTcpListenEvent{
		Port: proto.Int32(int32(addr.Port)),
	})
	return nil
}

// blockingAccept implements a blocking version of accept(2), that is, if no
// connections are ready to be accept, it will block until one becomes ready.
func (s *sock) blockingAccept(t *kernel.Task, peerAddr *tcpip.FullAddress) (tcpip.Endpoint, *waiter.Queue, *syserr.Error) {
	// Register for notifications.
	e, ch := waiter.NewChannelEntry(waiter.ReadableEvents)
	s.EventRegister(&e)
	defer s.EventUnregister(&e)

	// Try to accept the connection again; if it fails, then wait until we
	// get a notification.
	for {
		ep, wq, err := s.Endpoint.Accept(peerAddr)
		if _, ok := err.(*tcpip.ErrWouldBlock); !ok {
			return ep, wq, syserr.TranslateNetstackError(err)
		}

		if err := t.Block(ch); err != nil {
			return nil, nil, syserr.FromError(err)
		}
	}
}

// ConvertShutdown converts Linux shutdown flags into tcpip shutdown flags.
func ConvertShutdown(how int) (tcpip.ShutdownFlags, *syserr.Error) {
	var f tcpip.ShutdownFlags
	switch how {
	case linux.SHUT_RD:
		f = tcpip.ShutdownRead
	case linux.SHUT_WR:
		f = tcpip.ShutdownWrite
	case linux.SHUT_RDWR:
		f = tcpip.ShutdownRead | tcpip.ShutdownWrite
	default:
		return 0, syserr.ErrInvalidArgument
	}
	return f, nil
}

// Shutdown implements the linux syscall shutdown(2) for sockets backed by
// tcpip.Endpoint.
func (s *sock) Shutdown(_ *kernel.Task, how int) *syserr.Error {
	f, err := ConvertShutdown(how)
	if err != nil {
		return err
	}

	// Issue shutdown request.
	return syserr.TranslateNetstackError(s.Endpoint.Shutdown(f))
}

// GetSockOpt can be used to implement the linux syscall getsockopt(2) for
// sockets backed by a commonEndpoint.
func GetSockOpt(t *kernel.Task, s socket.Socket, ep commonEndpoint, family int, skType linux.SockType, level, name int, outPtr hostarch.Addr, outLen int) (marshal.Marshallable, *syserr.Error) {
	switch level {
	case linux.SOL_SOCKET:
		return getSockOptSocket(t, s, ep, family, skType, name, outLen)

	case linux.SOL_TCP:
		return getSockOptTCP(t, s, ep, name, outLen)

	case linux.SOL_IPV6:
		return getSockOptIPv6(t, s, ep, name, outPtr, outLen)

	case linux.SOL_IP:
		return getSockOptIP(t, s, ep, name, outPtr, outLen, family)

	case linux.SOL_ICMPV6:
		return getSockOptICMPv6(t, s, ep, name, outLen)

	case linux.SOL_PACKET:
		return getSockOptPacket(t, s, ep, name, outPtr, outLen)
	case linux.SOL_UDP, linux.SOL_RAW:
		// Not supported.
	}

	return nil, syserr.ErrProtocolNotAvailable
}

func boolToInt32(v bool) int32 {
	if v {
		return 1
	}
	return 0
}

// getSockOptSocket implements GetSockOpt when level is SOL_SOCKET.
func getSockOptSocket(t *kernel.Task, s socket.Socket, ep commonEndpoint, family int, _ linux.SockType, name, outLen int) (marshal.Marshallable, *syserr.Error) {
	// TODO(b/124056281): Stop rejecting short optLen values in getsockopt.
	switch name {
	case linux.SO_ERROR:
		if outLen < sizeOfInt32 {
			return nil, syserr.ErrInvalidArgument
		}

		// Get the last error and convert it.
		err := ep.SocketOptions().GetLastError()
		if err == nil {
			optP := primitive.Int32(0)
			return &optP, nil
		}

		optP := primitive.Int32(syserr.TranslateNetstackError(err).ToLinux())
		return &optP, nil

	case linux.SO_PEERCRED:
		if family != linux.AF_UNIX || outLen < unix.SizeofUcred {
			return nil, syserr.ErrInvalidArgument
		}

		tcred := t.Credentials()
		creds := linux.ControlMessageCredentials{
			PID: int32(t.ThreadGroup().ID()),
			UID: uint32(tcred.EffectiveKUID.In(tcred.UserNamespace).OrOverflow()),
			GID: uint32(tcred.EffectiveKGID.In(tcred.UserNamespace).OrOverflow()),
		}
		return &creds, nil

	case linux.SO_PASSCRED:
		if outLen < sizeOfInt32 {
			return nil, syserr.ErrInvalidArgument
		}

		v := primitive.Int32(boolToInt32(ep.SocketOptions().GetPassCred()))
		return &v, nil

	case linux.SO_SNDBUF:
		if outLen < sizeOfInt32 {
			return nil, syserr.ErrInvalidArgument
		}

		size := ep.SocketOptions().GetSendBufferSize()

		if size > math.MaxInt32 {
			size = math.MaxInt32
		}

		sizeP := primitive.Int32(size)
		return &sizeP, nil

	case linux.SO_RCVBUF:
		if outLen < sizeOfInt32 {
			return nil, syserr.ErrInvalidArgument
		}

		size := ep.SocketOptions().GetReceiveBufferSize()

		if size > math.MaxInt32 {
			size = math.MaxInt32
		}

		sizeP := primitive.Int32(size)
		return &sizeP, nil

	case linux.SO_REUSEADDR:
		if outLen < sizeOfInt32 {
			return nil, syserr.ErrInvalidArgument
		}

		v := primitive.Int32(boolToInt32(ep.SocketOptions().GetReuseAddress()))
		return &v, nil

	case linux.SO_REUSEPORT:
		if outLen < sizeOfInt32 {
			return nil, syserr.ErrInvalidArgument
		}

		v := primitive.Int32(boolToInt32(ep.SocketOptions().GetReusePort()))
		return &v, nil

	case linux.SO_BINDTODEVICE:
		v := ep.SocketOptions().GetBindToDevice()
		if v == 0 {
			var b primitive.ByteSlice
			return &b, nil
		}
		if outLen < linux.IFNAMSIZ {
			return nil, syserr.ErrInvalidArgument
		}
		s := t.NetworkContext()
		if s == nil {
			return nil, syserr.ErrNoDevice
		}
		nic, ok := s.Interfaces()[int32(v)]
		if !ok {
			// The NICID no longer indicates a valid interface, probably because that
			// interface was removed.
			return nil, syserr.ErrUnknownDevice
		}

		name := primitive.ByteSlice(append([]byte(nic.Name), 0))
		return &name, nil

	case linux.SO_BROADCAST:
		if outLen < sizeOfInt32 {
			return nil, syserr.ErrInvalidArgument
		}

		v := primitive.Int32(boolToInt32(ep.SocketOptions().GetBroadcast()))
		return &v, nil

	case linux.SO_KEEPALIVE:
		if outLen < sizeOfInt32 {
			return nil, syserr.ErrInvalidArgument
		}

		v := primitive.Int32(boolToInt32(ep.SocketOptions().GetKeepAlive()))
		return &v, nil

	case linux.SO_LINGER:
		if outLen < linux.SizeOfLinger {
			return nil, syserr.ErrInvalidArgument
		}

		var linger linux.Linger
		v := ep.SocketOptions().GetLinger()

		if v.Enabled {
			linger.OnOff = 1
		}
		linger.Linger = int32(v.Timeout.Seconds())
		return &linger, nil

	case linux.SO_SNDTIMEO:
		// TODO(igudger): Linux allows shorter lengths for partial results.
		if outLen < linux.SizeOfTimeval {
			return nil, syserr.ErrInvalidArgument
		}

		sendTimeout := linux.NsecToTimeval(s.SendTimeout())
		return &sendTimeout, nil

	case linux.SO_RCVTIMEO:
		// TODO(igudger): Linux allows shorter lengths for partial results.
		if outLen < linux.SizeOfTimeval {
			return nil, syserr.ErrInvalidArgument
		}

		recvTimeout := linux.NsecToTimeval(s.RecvTimeout())
		return &recvTimeout, nil

	case linux.SO_OOBINLINE:
		if outLen < sizeOfInt32 {
			return nil, syserr.ErrInvalidArgument
		}

		v := primitive.Int32(boolToInt32(ep.SocketOptions().GetOutOfBandInline()))
		return &v, nil

	case linux.SO_NO_CHECK:
		if outLen < sizeOfInt32 {
			return nil, syserr.ErrInvalidArgument
		}

		v := primitive.Int32(boolToInt32(ep.SocketOptions().GetNoChecksum()))
		return &v, nil

	case linux.SO_ACCEPTCONN:
		if outLen < sizeOfInt32 {
			return nil, syserr.ErrInvalidArgument
		}

		v := primitive.Int32(boolToInt32(ep.SocketOptions().GetAcceptConn()))
		return &v, nil

	case linux.SO_RCVLOWAT:
		if outLen < sizeOfInt32 {
			return nil, syserr.ErrInvalidArgument
		}

		v := primitive.Int32(ep.SocketOptions().GetRcvlowat())
		return &v, nil
	default:
		if v, err, handled := getSockOptSocketCustom(t, s, ep, name, outLen); handled {
			return v, err
		}
	}
	return nil, syserr.ErrProtocolNotAvailable
}

// getSockOptTCP implements GetSockOpt when level is SOL_TCP.
func getSockOptTCP(t *kernel.Task, s socket.Socket, ep commonEndpoint, name, outLen int) (marshal.Marshallable, *syserr.Error) {
	if !socket.IsTCP(s) {
		return nil, syserr.ErrUnknownProtocolOption
	}

	switch name {
	case linux.TCP_NODELAY:
		if outLen < sizeOfInt32 {
			return nil, syserr.ErrInvalidArgument
		}

		v := primitive.Int32(boolToInt32(!ep.SocketOptions().GetDelayOption()))
		return &v, nil

	case linux.TCP_CORK:
		if outLen < sizeOfInt32 {
			return nil, syserr.ErrInvalidArgument
		}

		v := primitive.Int32(boolToInt32(ep.SocketOptions().GetCorkOption()))
		return &v, nil

	case linux.TCP_QUICKACK:
		if outLen < sizeOfInt32 {
			return nil, syserr.ErrInvalidArgument
		}

		v := primitive.Int32(boolToInt32(ep.SocketOptions().GetQuickAck()))
		return &v, nil

	case linux.TCP_MAXSEG:
		if outLen < sizeOfInt32 {
			return nil, syserr.ErrInvalidArgument
		}

		v, err := ep.GetSockOptInt(tcpip.MaxSegOption)
		if err != nil {
			return nil, syserr.TranslateNetstackError(err)
		}
		vP := primitive.Int32(v)
		return &vP, nil

	case linux.TCP_KEEPIDLE:
		if outLen < sizeOfInt32 {
			return nil, syserr.ErrInvalidArgument
		}

		var v tcpip.KeepaliveIdleOption
		if err := ep.GetSockOpt(&v); err != nil {
			return nil, syserr.TranslateNetstackError(err)
		}
		keepAliveIdle := primitive.Int32(time.Duration(v) / time.Second)
		return &keepAliveIdle, nil

	case linux.TCP_KEEPINTVL:
		if outLen < sizeOfInt32 {
			return nil, syserr.ErrInvalidArgument
		}

		var v tcpip.KeepaliveIntervalOption
		if err := ep.GetSockOpt(&v); err != nil {
			return nil, syserr.TranslateNetstackError(err)
		}
		keepAliveInterval := primitive.Int32(time.Duration(v) / time.Second)
		return &keepAliveInterval, nil

	case linux.TCP_KEEPCNT:
		if outLen < sizeOfInt32 {
			return nil, syserr.ErrInvalidArgument
		}

		v, err := ep.GetSockOptInt(tcpip.KeepaliveCountOption)
		if err != nil {
			return nil, syserr.TranslateNetstackError(err)
		}
		vP := primitive.Int32(v)
		return &vP, nil

	case linux.TCP_USER_TIMEOUT:
		if outLen < sizeOfInt32 {
			return nil, syserr.ErrInvalidArgument
		}

		var v tcpip.TCPUserTimeoutOption
		if err := ep.GetSockOpt(&v); err != nil {
			return nil, syserr.TranslateNetstackError(err)
		}
		tcpUserTimeout := primitive.Int32(time.Duration(v) / time.Millisecond)
		return &tcpUserTimeout, nil

	case linux.TCP_INFO:
		var v tcpip.TCPInfoOption
		if err := ep.GetSockOpt(&v); err != nil {
			return nil, syserr.TranslateNetstackError(err)
		}

		info := linux.TCPInfo{
			State:       uint8(v.State),
			RTO:         uint32(v.RTO / time.Microsecond),
			RTT:         uint32(v.RTT / time.Microsecond),
			RTTVar:      uint32(v.RTTVar / time.Microsecond),
			SndSsthresh: v.SndSsthresh,
			SndCwnd:     v.SndCwnd,
		}
		switch v.CcState {
		case tcpip.RTORecovery:
			info.CaState = linux.TCP_CA_Loss
		case tcpip.FastRecovery, tcpip.SACKRecovery:
			info.CaState = linux.TCP_CA_Recovery
		case tcpip.Disorder:
			info.CaState = linux.TCP_CA_Disorder
		case tcpip.Open:
			info.CaState = linux.TCP_CA_Open
		}

		// In netstack reorderSeen is updated only when RACK is enabled.
		// We only track whether the reordering is seen, which is
		// different than Linux where reorderSeen is not specific to
		// RACK and is incremented when a reordering event is seen.
		if v.ReorderSeen {
			info.ReordSeen = 1
		}

		// Linux truncates the output binary to outLen.
		buf := t.CopyScratchBuffer(info.SizeBytes())
		info.MarshalUnsafe(buf)
		if len(buf) > outLen {
			buf = buf[:outLen]
		}
		bufP := primitive.ByteSlice(buf)
		return &bufP, nil

	case linux.TCP_CC_INFO,
		linux.TCP_NOTSENT_LOWAT,
		linux.TCP_ZEROCOPY_RECEIVE:

		// Not supported.

	case linux.TCP_CONGESTION:
		if outLen <= 0 {
			return nil, syserr.ErrInvalidArgument
		}

		var v tcpip.CongestionControlOption
		if err := ep.GetSockOpt(&v); err != nil {
			return nil, syserr.TranslateNetstackError(err)
		}

		// We match linux behaviour here where it returns the lower of
		// TCP_CA_NAME_MAX bytes or the value of the option length.
		//
		// This is Linux's net/tcp.h TCP_CA_NAME_MAX.
		const tcpCANameMax = 16

		toCopy := tcpCANameMax
		if outLen < tcpCANameMax {
			toCopy = outLen
		}
		b := make([]byte, toCopy)
		copy(b, v)

		bP := primitive.ByteSlice(b)
		return &bP, nil

	case linux.TCP_LINGER2:
		if outLen < sizeOfInt32 {
			return nil, syserr.ErrInvalidArgument
		}

		var v tcpip.TCPLingerTimeoutOption
		if err := ep.GetSockOpt(&v); err != nil {
			return nil, syserr.TranslateNetstackError(err)
		}
		var lingerTimeout primitive.Int32
		if v >= 0 {
			lingerTimeout = primitive.Int32(time.Duration(v) / time.Second)
		} else {
			lingerTimeout = -1
		}
		return &lingerTimeout, nil

	case linux.TCP_DEFER_ACCEPT:
		if outLen < sizeOfInt32 {
			return nil, syserr.ErrInvalidArgument
		}

		var v tcpip.TCPDeferAcceptOption
		if err := ep.GetSockOpt(&v); err != nil {
			return nil, syserr.TranslateNetstackError(err)
		}

		tcpDeferAccept := primitive.Int32(time.Duration(v) / time.Second)
		return &tcpDeferAccept, nil

	case linux.TCP_SYNCNT:
		if outLen < sizeOfInt32 {
			return nil, syserr.ErrInvalidArgument
		}

		v, err := ep.GetSockOptInt(tcpip.TCPSynCountOption)
		if err != nil {
			return nil, syserr.TranslateNetstackError(err)
		}
		vP := primitive.Int32(v)
		return &vP, nil

	case linux.TCP_WINDOW_CLAMP:
		if outLen < sizeOfInt32 {
			return nil, syserr.ErrInvalidArgument
		}

		v, err := ep.GetSockOptInt(tcpip.TCPWindowClampOption)
		if err != nil {
			return nil, syserr.TranslateNetstackError(err)
		}
		vP := primitive.Int32(v)
		return &vP, nil
	}
	return nil, syserr.ErrProtocolNotAvailable
}

func getSockOptICMPv6(t *kernel.Task, s socket.Socket, ep commonEndpoint, name int, outLen int) (marshal.Marshallable, *syserr.Error) {
	if _, ok := ep.(tcpip.Endpoint); !ok {
		log.Warningf("SOL_ICMPV6 options not supported on endpoints other than tcpip.Endpoint: option = %d", name)
		return nil, syserr.ErrUnknownProtocolOption
	}

	if family, _, _ := s.Type(); family != linux.AF_INET6 {
		return nil, syserr.ErrNotSupported
	}

	switch name {
	case linux.ICMPV6_FILTER:
		var v tcpip.ICMPv6Filter
		if err := ep.GetSockOpt(&v); err != nil {
			return nil, syserr.TranslateNetstackError(err)
		}

		filter := linux.ICMP6Filter{Filter: v.DenyType}

		// Linux truncates the output to outLen.
		buf := t.CopyScratchBuffer(filter.SizeBytes())
		filter.MarshalUnsafe(buf)
		if len(buf) > outLen {
			buf = buf[:outLen]
		}
		bufP := primitive.ByteSlice(buf)
		return &bufP, nil
	}
	return nil, syserr.ErrProtocolNotAvailable
}

func defaultTTL(t *kernel.Task, network tcpip.NetworkProtocolNumber) (primitive.Int32, tcpip.Error) {
	var opt tcpip.DefaultTTLOption
	stack := inet.StackFromContext(t)
	if err := stack.(*Stack).Stack.NetworkProtocolOption(network, &opt); err != nil {
		return 0, err
	}
	return primitive.Int32(opt), nil
}

// getSockOptIPv6 implements GetSockOpt when level is SOL_IPV6.
func getSockOptIPv6(t *kernel.Task, s socket.Socket, ep commonEndpoint, name int, outPtr hostarch.Addr, outLen int) (marshal.Marshallable, *syserr.Error) {
	if _, ok := ep.(tcpip.Endpoint); !ok {
		log.Warningf("SOL_IPV6 options not supported on endpoints other than tcpip.Endpoint: option = %d", name)
		return nil, syserr.ErrUnknownProtocolOption
	}

	family, skType, _ := s.Type()
	if family != linux.AF_INET6 {
		return nil, syserr.ErrNotSupported
	}

	switch name {
	case linux.IPV6_CHECKSUM:
		if outLen < sizeOfInt32 {
			return nil, syserr.ErrInvalidArgument
		}

		v, err := ep.GetSockOptInt(tcpip.IPv6Checksum)
		if err != nil {
			return nil, syserr.TranslateNetstackError(err)
		}

		vP := primitive.Int32(v)
		return &vP, nil

	case linux.IPV6_V6ONLY:
		if outLen < sizeOfInt32 {
			return nil, syserr.ErrInvalidArgument
		}

		v := primitive.Int32(boolToInt32(ep.SocketOptions().GetV6Only()))
		return &v, nil

	case linux.IPV6_UNICAST_HOPS:
		if outLen < sizeOfInt32 {
			return nil, syserr.ErrInvalidArgument
		}

		v, err := ep.GetSockOptInt(tcpip.IPv6HopLimitOption)
		if err != nil {
			return nil, syserr.TranslateNetstackError(err)
		}

		// Fill in the default value, if needed.
		vP := primitive.Int32(v)
		if vP == -1 {
			vP, err = defaultTTL(t, header.IPv6ProtocolNumber)
			if err != nil {
				return nil, syserr.TranslateNetstackError(err)
			}
		}

		return &vP, nil

	case linux.IPV6_RECVHOPLIMIT:
		if outLen < sizeOfInt32 {
			return nil, syserr.ErrInvalidArgument
		}

		v := primitive.Int32(boolToInt32(ep.SocketOptions().GetReceiveHopLimit()))
		return &v, nil

	case linux.IPV6_PATHMTU:
		// Not supported.

	case linux.IPV6_TCLASS:
		// Length handling for parity with Linux.
		if outLen == 0 {
			var b primitive.ByteSlice
			return &b, nil
		}
		v, err := ep.GetSockOptInt(tcpip.IPv6TrafficClassOption)
		if err != nil {
			return nil, syserr.TranslateNetstackError(err)
		}

		uintv := primitive.Uint32(v)
		// Linux truncates the output binary to outLen.
		ib := t.CopyScratchBuffer(uintv.SizeBytes())
		uintv.MarshalUnsafe(ib)
		// Handle cases where outLen is lesser than sizeOfInt32.
		if len(ib) > outLen {
			ib = ib[:outLen]
		}
		ibP := primitive.ByteSlice(ib)
		return &ibP, nil

	case linux.IPV6_RECVTCLASS:
		if outLen < sizeOfInt32 {
			return nil, syserr.ErrInvalidArgument
		}

		v := primitive.Int32(boolToInt32(ep.SocketOptions().GetReceiveTClass()))
		return &v, nil
	case linux.IPV6_RECVERR:
		if outLen < sizeOfInt32 {
			return nil, syserr.ErrInvalidArgument
		}

		v := primitive.Int32(boolToInt32(ep.SocketOptions().GetIPv6RecvError()))
		return &v, nil

	case linux.IPV6_RECVORIGDSTADDR:
		if outLen < sizeOfInt32 {
			return nil, syserr.ErrInvalidArgument
		}

		v := primitive.Int32(boolToInt32(ep.SocketOptions().GetReceiveOriginalDstAddress()))
		return &v, nil

	case linux.IPV6_RECVPKTINFO:
		if outLen < sizeOfInt32 {
			return nil, syserr.ErrInvalidArgument
		}

		v := primitive.Int32(boolToInt32(ep.SocketOptions().GetIPv6ReceivePacketInfo()))
		return &v, nil

	case linux.IP6T_ORIGINAL_DST:
		if outLen < linux.SockAddrInet6Size {
			return nil, syserr.ErrInvalidArgument
		}

		var v tcpip.OriginalDestinationOption
		if err := ep.GetSockOpt(&v); err != nil {
			return nil, syserr.TranslateNetstackError(err)
		}

		a, _ := socket.ConvertAddress(linux.AF_INET6, tcpip.FullAddress(v))
		return a.(*linux.SockAddrInet6), nil

	case linux.IP6T_SO_GET_INFO:
		if outLen < linux.SizeOfIPTGetinfo {
			return nil, syserr.ErrInvalidArgument
		}

		// Only valid for raw IPv6 sockets.
		if skType != linux.SOCK_RAW {
			return nil, syserr.ErrProtocolNotAvailable
		}

		stk := inet.StackFromContext(t)
		if stk == nil {
			return nil, syserr.ErrNoDevice
		}
		info, err := netfilter.GetInfo(t, stk.(*Stack).Stack, outPtr, true)
		if err != nil {
			return nil, err
		}
		return &info, nil

	case linux.IP6T_SO_GET_ENTRIES:
		// IPTGetEntries is reused for IPv6.
		if outLen < linux.SizeOfIPTGetEntries {
			return nil, syserr.ErrInvalidArgument
		}
		// Only valid for raw IPv6 sockets.
		if skType != linux.SOCK_RAW {
			return nil, syserr.ErrProtocolNotAvailable
		}

		stk := inet.StackFromContext(t)
		if stk == nil {
			return nil, syserr.ErrNoDevice
		}
		entries, err := netfilter.GetEntries6(t, stk.(*Stack).Stack, outPtr, outLen)
		if err != nil {
			return nil, err
		}
		return &entries, nil

	case linux.IP6T_SO_GET_REVISION_MATCH:
		if outLen < linux.SizeOfXTGetRevision {
			return nil, syserr.ErrInvalidArgument
		}

		// Only valid for raw IPv6 sockets.
		if skType != linux.SOCK_RAW {
			return nil, syserr.ErrProtocolNotAvailable
		}

		stk := inet.StackFromContext(t)
		if stk == nil {
			return nil, syserr.ErrNoDevice
		}

		// Get the highest support matcher revision.
		ret, err := netfilter.MatchRevision(t, outPtr)
		if err != nil {
			return nil, err
		}

		return &ret, nil

	case linux.IP6T_SO_GET_REVISION_TARGET:
		if outLen < linux.SizeOfXTGetRevision {
			return nil, syserr.ErrInvalidArgument
		}

		// Only valid for raw IPv6 sockets.
		if skType != linux.SOCK_RAW {
			return nil, syserr.ErrProtocolNotAvailable
		}

		stk := inet.StackFromContext(t)
		if stk == nil {
			return nil, syserr.ErrNoDevice
		}
		ret, err := netfilter.TargetRevision(t, outPtr, header.IPv6ProtocolNumber)
		if err != nil {
			return nil, err
		}
		return &ret, nil
	}
	return nil, syserr.ErrProtocolNotAvailable
}

// getSockOptIP implements GetSockOpt when level is SOL_IP.
func getSockOptIP(t *kernel.Task, s socket.Socket, ep commonEndpoint, name int, outPtr hostarch.Addr, outLen int, _ int) (marshal.Marshallable, *syserr.Error) {
	if _, ok := ep.(tcpip.Endpoint); !ok {
		log.Warningf("SOL_IP options not supported on endpoints other than tcpip.Endpoint: option = %d, endpoint = %T", name, ep)
		return nil, syserr.ErrUnknownProtocolOption
	}

	switch name {
	case linux.IP_TTL:
		if outLen < sizeOfInt32 {
			return nil, syserr.ErrInvalidArgument
		}

		v, err := ep.GetSockOptInt(tcpip.IPv4TTLOption)
		if err != nil {
			return nil, syserr.TranslateNetstackError(err)
		}

		// Fill in the default value, if needed.
		vP := primitive.Int32(v)
		if vP == 0 {
			vP, err = defaultTTL(t, header.IPv4ProtocolNumber)
			if err != nil {
				return nil, syserr.TranslateNetstackError(err)
			}
		}

		return &vP, nil

	case linux.IP_RECVTTL:
		if outLen < sizeOfInt32 {
			return nil, syserr.ErrInvalidArgument
		}

		v := primitive.Int32(boolToInt32(ep.SocketOptions().GetReceiveTTL()))
		return &v, nil

	case linux.IP_MULTICAST_TTL:
		if outLen < sizeOfInt32 {
			return nil, syserr.ErrInvalidArgument
		}

		v, err := ep.GetSockOptInt(tcpip.MulticastTTLOption)
		if err != nil {
			return nil, syserr.TranslateNetstackError(err)
		}

		vP := primitive.Int32(v)
		return &vP, nil

	case linux.IP_MULTICAST_IF:
		if outLen < len(linux.InetAddr{}) {
			return nil, syserr.ErrInvalidArgument
		}

		var v tcpip.MulticastInterfaceOption
		if err := ep.GetSockOpt(&v); err != nil {
			return nil, syserr.TranslateNetstackError(err)
		}

		a, _ := socket.ConvertAddress(linux.AF_INET, tcpip.FullAddress{Addr: v.InterfaceAddr})

		return &a.(*linux.SockAddrInet).Addr, nil

	case linux.IP_MULTICAST_LOOP:
		if outLen < sizeOfInt32 {
			return nil, syserr.ErrInvalidArgument
		}

		v := primitive.Int32(boolToInt32(ep.SocketOptions().GetMulticastLoop()))
		return &v, nil

	case linux.IP_TOS:
		// Length handling for parity with Linux.
		if outLen == 0 {
			var b primitive.ByteSlice
			return &b, nil
		}
		v, err := ep.GetSockOptInt(tcpip.IPv4TOSOption)
		if err != nil {
			return nil, syserr.TranslateNetstackError(err)
		}
		if outLen < sizeOfInt32 {
			vP := primitive.Uint8(v)
			return &vP, nil
		}
		vP := primitive.Int32(v)
		return &vP, nil

	case linux.IP_RECVTOS:
		if outLen < sizeOfInt32 {
			return nil, syserr.ErrInvalidArgument
		}

		v := primitive.Int32(boolToInt32(ep.SocketOptions().GetReceiveTOS()))
		return &v, nil

	case linux.IP_RECVERR:
		if outLen < sizeOfInt32 {
			return nil, syserr.ErrInvalidArgument
		}

		v := primitive.Int32(boolToInt32(ep.SocketOptions().GetIPv4RecvError()))
		return &v, nil

	case linux.IP_PKTINFO:
		if outLen < sizeOfInt32 {
			return nil, syserr.ErrInvalidArgument
		}

		v := primitive.Int32(boolToInt32(ep.SocketOptions().GetReceivePacketInfo()))
		return &v, nil

	case linux.IP_HDRINCL:
		if outLen < sizeOfInt32 {
			return nil, syserr.ErrInvalidArgument
		}

		v := primitive.Int32(boolToInt32(ep.SocketOptions().GetHeaderIncluded()))
		return &v, nil

	case linux.IP_RECVORIGDSTADDR:
		if outLen < sizeOfInt32 {
			return nil, syserr.ErrInvalidArgument
		}

		v := primitive.Int32(boolToInt32(ep.SocketOptions().GetReceiveOriginalDstAddress()))
		return &v, nil

	case linux.SO_ORIGINAL_DST:
		if outLen < linux.SockAddrInetSize {
			return nil, syserr.ErrInvalidArgument
		}

		var v tcpip.OriginalDestinationOption
		if err := ep.GetSockOpt(&v); err != nil {
			return nil, syserr.TranslateNetstackError(err)
		}

		a, _ := socket.ConvertAddress(linux.AF_INET, tcpip.FullAddress(v))
		return a.(*linux.SockAddrInet), nil

	case linux.IPT_SO_GET_INFO:
		if outLen < linux.SizeOfIPTGetinfo {
			return nil, syserr.ErrInvalidArgument
		}

		// Only valid for raw IPv4 sockets.
		if family, skType, _ := s.Type(); family != linux.AF_INET || skType != linux.SOCK_RAW {
			return nil, syserr.ErrProtocolNotAvailable
		}

		stk := inet.StackFromContext(t)
		if stk == nil {
			return nil, syserr.ErrNoDevice
		}
		info, err := netfilter.GetInfo(t, stk.(*Stack).Stack, outPtr, false)
		if err != nil {
			return nil, err
		}
		return &info, nil

	case linux.IPT_SO_GET_ENTRIES:
		if outLen < linux.SizeOfIPTGetEntries {
			return nil, syserr.ErrInvalidArgument
		}

		// Only valid for raw IPv4 sockets.
		if family, skType, _ := s.Type(); family != linux.AF_INET || skType != linux.SOCK_RAW {
			return nil, syserr.ErrProtocolNotAvailable
		}

		stk := inet.StackFromContext(t)
		if stk == nil {
			return nil, syserr.ErrNoDevice
		}
		entries, err := netfilter.GetEntries4(t, stk.(*Stack).Stack, outPtr, outLen)
		if err != nil {
			return nil, err
		}
		return &entries, nil

	case linux.IPT_SO_GET_REVISION_MATCH:
		if outLen < linux.SizeOfXTGetRevision {
			return nil, syserr.ErrInvalidArgument
		}

		// Only valid for raw IPv4 sockets.
		family, skType, _ := s.Type()
		if family != linux.AF_INET || skType != linux.SOCK_RAW {
			return nil, syserr.ErrProtocolNotAvailable
		}

		stk := inet.StackFromContext(t)
		if stk == nil {
			return nil, syserr.ErrNoDevice
		}

		// Get the highest support matcher revision.
		ret, err := netfilter.MatchRevision(t, outPtr)
		if err != nil {
			return nil, err
		}

		return &ret, nil

	case linux.IPT_SO_GET_REVISION_TARGET:
		if outLen < linux.SizeOfXTGetRevision {
			return nil, syserr.ErrInvalidArgument
		}

		// Only valid for raw IPv4 sockets.
		if family, skType, _ := s.Type(); family != linux.AF_INET || skType != linux.SOCK_RAW {
			return nil, syserr.ErrProtocolNotAvailable
		}

		stk := inet.StackFromContext(t)
		if stk == nil {
			return nil, syserr.ErrNoDevice
		}
		ret, err := netfilter.TargetRevision(t, outPtr, header.IPv4ProtocolNumber)
		if err != nil {
			return nil, err
		}
		return &ret, nil

	case linux.IP_MTU_DISCOVER:
		if outLen < sizeOfInt32 {
			return nil, syserr.ErrInvalidArgument
		}

		v, err := ep.GetSockOptInt(tcpip.MTUDiscoverOption)
		if err != nil {
			return nil, syserr.TranslateNetstackError(err)
		}
		switch tcpip.PMTUDStrategy(v) {
		case tcpip.PMTUDiscoveryWant:
			v = linux.IP_PMTUDISC_WANT
		case tcpip.PMTUDiscoveryDont:
			v = linux.IP_PMTUDISC_DONT
		case tcpip.PMTUDiscoveryDo:
			v = linux.IP_PMTUDISC_DO
		case tcpip.PMTUDiscoveryProbe:
			v = linux.IP_PMTUDISC_PROBE
		default:
			panic(fmt.Errorf("unknown PMTUD option: %d", v))
		}
		vP := primitive.Int32(v)
		return &vP, nil
	}
	return nil, syserr.ErrProtocolNotAvailable
}

func getSockOptPacket(t *kernel.Task, s socket.Socket, ep commonEndpoint, name int, outPtr hostarch.Addr, outLen int) (marshal.Marshallable, *syserr.Error) {
	if _, ok := ep.(tcpip.Endpoint); !ok {
		log.Warningf("SOL_PACKET options not supported on endpoints other than tcpip.Endpoint: option = %d, endpoint = %T", name, ep)
		return nil, syserr.ErrUnknownProtocolOption
	}
	switch name {
	case linux.PACKET_HDRLEN:
		var version primitive.Int32
		version.CopyIn(t, outPtr)
		switch version {
		case linux.TPACKET_V1:
			v := primitive.Int32(uint32((*linux.TpacketHdr)(nil).SizeBytes()))
			return &v, nil
		case linux.TPACKET_V2:
			v := primitive.Int32(uint32((*linux.Tpacket2Hdr)(nil).SizeBytes()))
			return &v, nil
		default:
			return nil, syserr.ErrInvalidArgument
		}
	case linux.PACKET_STATISTICS:
		var tps tcpip.TpacketStats
		if err := ep.GetSockOpt(&tps); err != nil {
			return nil, syserr.TranslateNetstackError(err)
		}
		v := linux.TpacketStats{
			Packets: tps.Packets,
			Dropped: tps.Dropped,
		}
		return &v, nil
	}
	return nil, syserr.ErrProtocolNotAvailable
}

// SetSockOpt can be used to implement the linux syscall setsockopt(2) for
// sockets backed by a commonEndpoint.
func SetSockOpt(t *kernel.Task, s socket.Socket, ep commonEndpoint, level int, name int, optVal []byte) *syserr.Error {
	switch level {
	case linux.SOL_SOCKET:
		return setSockOptSocket(t, s, ep, name, optVal)

	case linux.SOL_TCP:
		return setSockOptTCP(t, s, ep, name, optVal)

	case linux.SOL_ICMPV6:
		return setSockOptICMPv6(t, s, ep, name, optVal)

	case linux.SOL_IPV6:
		return setSockOptIPv6(t, s, ep, name, optVal)

	case linux.SOL_IP:
		return setSockOptIP(t, s, ep, name, optVal)

	case linux.SOL_PACKET:
		return setSockOptPacket(t, s, ep, name, optVal)

	case linux.SOL_UDP,
		linux.SOL_RAW:
		// Not supported.
	}

	return nil
}

func clampBufSize(newSz, min, max int64, ignoreMax bool) int64 {
	// packetOverheadFactor is used to multiply the value provided by the user on
	// a setsockopt(2) for setting the send/receive buffer sizes sockets.
	const packetOverheadFactor = 2

	if !ignoreMax && newSz > max {
		newSz = max
	}

	if newSz < math.MaxInt32/packetOverheadFactor {
		newSz *= packetOverheadFactor
		if newSz < min {
			newSz = min
		}
	} else {
		newSz = math.MaxInt32
	}
	return newSz
}

func incrementBadSetSocketOptionMetric(t *kernel.Task, fieldValue *metric.FieldValue, name int) {
	if name >= 0 && name <= maxSocketOptionNameValue {
		unimplementedSetSocketOptionMetric.Increment(fieldValue, allowedSocketOptionNameValues[name])
	} else {
		unknownSetSocketOptionMetric.Increment(&socketLevelSocketFieldValue)
	}
	t.Kernel().EmitUnimplementedEvent(t, unix.SYS_SETSOCKOPT)
}

// setSockOptSocket implements SetSockOpt when level is SOL_SOCKET.
func setSockOptSocket(t *kernel.Task, s socket.Socket, ep commonEndpoint, name int, optVal []byte) *syserr.Error {
	switch name {
	case linux.SO_SNDBUF:
		if len(optVal) < sizeOfInt32 {
			return syserr.ErrInvalidArgument
		}

		v := hostarch.ByteOrder.Uint32(optVal)
		min, max := ep.SocketOptions().SendBufferLimits()
		clamped := clampBufSize(int64(v), min, max, false /* ignoreMax */)
		ep.SocketOptions().SetSendBufferSize(clamped, true /* notify */)
		return nil

	case linux.SO_RCVBUF:
		if len(optVal) < sizeOfInt32 {
			return syserr.ErrInvalidArgument
		}

		v := hostarch.ByteOrder.Uint32(optVal)
		min, max := ep.SocketOptions().ReceiveBufferLimits()
		clamped := clampBufSize(int64(v), min, max, false /* ignoreMax */)
		ep.SocketOptions().SetReceiveBufferSize(clamped, true /* notify */)
		return nil

	case linux.SO_RCVBUFFORCE:
		if len(optVal) < sizeOfInt32 {
			return syserr.ErrInvalidArgument
		}

		if creds := auth.CredentialsFromContext(t); !creds.HasCapability(linux.CAP_NET_ADMIN) {
			return syserr.ErrNotPermitted
		}

		v := hostarch.ByteOrder.Uint32(optVal)
		min, max := ep.SocketOptions().ReceiveBufferLimits()
		clamped := clampBufSize(int64(v), min, max, true /* ignoreMax */)
		ep.SocketOptions().SetReceiveBufferSize(clamped, true /* notify */)
		return nil

	case linux.SO_REUSEADDR:
		if len(optVal) < sizeOfInt32 {
			return syserr.ErrInvalidArgument
		}

		v := hostarch.ByteOrder.Uint32(optVal)
		ep.SocketOptions().SetReuseAddress(v != 0)
		return nil

	case linux.SO_REUSEPORT:
		if len(optVal) < sizeOfInt32 {
			return syserr.ErrInvalidArgument
		}

		v := hostarch.ByteOrder.Uint32(optVal)
		ep.SocketOptions().SetReusePort(v != 0)
		return nil

	case linux.SO_BINDTODEVICE:
		n := bytes.IndexByte(optVal, 0)
		if n == -1 {
			n = len(optVal)
		}
		name := string(optVal[:n])
		if name == "" {
			return syserr.TranslateNetstackError(ep.SocketOptions().SetBindToDevice(0))
		}
		s := t.NetworkContext()
		if s == nil {
			return syserr.ErrNoDevice
		}
		for nicID, nic := range s.Interfaces() {
			if nic.Name == name {
				return syserr.TranslateNetstackError(ep.SocketOptions().SetBindToDevice(nicID))
			}
		}
		return syserr.ErrUnknownDevice

	case linux.SO_BROADCAST:
		if len(optVal) < sizeOfInt32 {
			return syserr.ErrInvalidArgument
		}

		v := hostarch.ByteOrder.Uint32(optVal)
		ep.SocketOptions().SetBroadcast(v != 0)
		return nil

	case linux.SO_PASSCRED:
		if len(optVal) < sizeOfInt32 {
			return syserr.ErrInvalidArgument
		}

		v := hostarch.ByteOrder.Uint32(optVal)
		ep.SocketOptions().SetPassCred(v != 0)
		return nil

	case linux.SO_KEEPALIVE:
		if len(optVal) < sizeOfInt32 {
			return syserr.ErrInvalidArgument
		}

		v := hostarch.ByteOrder.Uint32(optVal)
		ep.SocketOptions().SetKeepAlive(v != 0)
		return nil

	case linux.SO_SNDTIMEO:
		if len(optVal) < linux.SizeOfTimeval {
			return syserr.ErrInvalidArgument
		}

		var v linux.Timeval
		v.UnmarshalBytes(optVal)
		if v.Usec < 0 || v.Usec >= int64(time.Second/time.Microsecond) {
			return syserr.ErrDomain
		}
		s.SetSendTimeout(v.ToNsecCapped())
		return nil

	case linux.SO_RCVTIMEO:
		if len(optVal) < linux.SizeOfTimeval {
			return syserr.ErrInvalidArgument
		}

		var v linux.Timeval
		v.UnmarshalBytes(optVal)
		if v.Usec < 0 || v.Usec >= int64(time.Second/time.Microsecond) {
			return syserr.ErrDomain
		}
		s.SetRecvTimeout(v.ToNsecCapped())
		return nil

	case linux.SO_OOBINLINE:
		if len(optVal) < sizeOfInt32 {
			return syserr.ErrInvalidArgument
		}

		v := hostarch.ByteOrder.Uint32(optVal)
		ep.SocketOptions().SetOutOfBandInline(v != 0)
		return nil

	case linux.SO_NO_CHECK:
		if len(optVal) < sizeOfInt32 {
			return syserr.ErrInvalidArgument
		}

		v := hostarch.ByteOrder.Uint32(optVal)
		ep.SocketOptions().SetNoChecksum(v != 0)
		return nil

	case linux.SO_LINGER:
		if len(optVal) < linux.SizeOfLinger {
			return syserr.ErrInvalidArgument
		}

		var v linux.Linger
		v.UnmarshalBytes(optVal)

		ep.SocketOptions().SetLinger(tcpip.LingerOption{
			Enabled: v.OnOff != 0,
			Timeout: time.Second * time.Duration(v.Linger),
		})
		return nil

	case linux.SO_DETACH_FILTER:
		// optval is ignored.
		var v tcpip.SocketDetachFilterOption
		return syserr.TranslateNetstackError(ep.SetSockOpt(&v))

	// TODO(b/226603727): Add support for SO_RCVLOWAT option. For now, only
	// the unsupported syscall message is removed.
	case linux.SO_RCVLOWAT:
		if len(optVal) < sizeOfInt32 {
			return syserr.ErrInvalidArgument
		}

		v := hostarch.ByteOrder.Uint32(optVal)
		ep.SocketOptions().SetRcvlowat(int32(v))
		return nil
	case linux.SO_DEBUG,
		linux.SO_TYPE,
		linux.SO_ERROR,
		linux.SO_DONTROUTE,
		linux.SO_PRIORITY,
		linux.SO_BSDCOMPAT,
		linux.SO_PEERCRED,
		linux.SO_SNDLOWAT,
		linux.SO_ATTACH_FILTER,
		linux.SO_PEERNAME,
		linux.SO_TIMESTAMP,
		linux.SO_ACCEPTCONN,
		linux.SO_PEERSEC,
		linux.SO_SNDBUFFORCE,
		linux.SO_PASSSEC,
		linux.SO_TIMESTAMPNS,
		linux.SO_MARK,
		linux.SO_TIMESTAMPING,
		linux.SO_PROTOCOL,
		linux.SO_DOMAIN,
		linux.SO_RXQ_OVFL,
		linux.SO_WIFI_STATUS,
		linux.SO_PEEK_OFF,
		linux.SO_NOFCS,
		linux.SO_LOCK_FILTER,
		linux.SO_SELECT_ERR_QUEUE,
		linux.SO_BUSY_POLL,
		linux.SO_MAX_PACING_RATE,
		linux.SO_BPF_EXTENSIONS,
		linux.SO_INCOMING_CPU,
		linux.SO_ATTACH_BPF,
		linux.SO_ATTACH_REUSEPORT_CBPF,
		linux.SO_ATTACH_REUSEPORT_EBPF,
		linux.SO_CNX_ADVICE,
		linux.SO_MEMINFO,
		linux.SO_INCOMING_NAPI_ID,
		linux.SO_COOKIE,
		linux.SO_PEERGROUPS,
		linux.SO_ZEROCOPY,
		linux.SO_TXTIME,
		linux.SO_BINDTOIFINDEX,
		linux.SO_TIMESTAMP_NEW,
		linux.SO_TIMESTAMPNS_NEW,
		linux.SO_TIMESTAMPING_NEW,
		linux.SO_RCVTIMEO_NEW,
		linux.SO_SNDTIMEO_NEW,
		linux.SO_DETACH_REUSEPORT_BPF,
		linux.SO_PREFER_BUSY_POLL,
		linux.SO_BUSY_POLL_BUDGET,
		linux.SO_NETNS_COOKIE,
		linux.SO_BUF_LOCK,
		linux.SO_RESERVE_MEM,
		linux.SO_TXREHASH,
		linux.SO_RCVMARK,
		linux.SO_PASSPIDFD,
		linux.SO_PEERPIDFD,
		linux.SO_DEVMEM_LINEAR,
		linux.SO_DEVMEM_DMABUF,
		linux.SO_DEVMEM_DONTNEED,
		linux.SO_RCVPRIORITY:
		incrementBadSetSocketOptionMetric(t, &socketLevelSocketFieldValue, name)
		return nil
	default:
		if err, handled := setSockOptSocketCustom(t, s, ep, name, optVal); handled {
			return err
		}
	}
	incrementBadSetSocketOptionMetric(t, &socketLevelSocketFieldValue, name)
	return nil
}

// setSockOptTCP implements SetSockOpt when level is SOL_TCP.
func setSockOptTCP(t *kernel.Task, s socket.Socket, ep commonEndpoint, name int, optVal []byte) *syserr.Error {
	if !socket.IsTCP(s) {
		return syserr.ErrUnknownProtocolOption
	}

	switch name {
	case linux.TCP_NODELAY:
		if len(optVal) < sizeOfInt32 {
			return syserr.ErrInvalidArgument
		}

		v := hostarch.ByteOrder.Uint32(optVal)
		ep.SocketOptions().SetDelayOption(v == 0)
		return nil

	case linux.TCP_CORK:
		if len(optVal) < sizeOfInt32 {
			return syserr.ErrInvalidArgument
		}

		v := hostarch.ByteOrder.Uint32(optVal)
		ep.SocketOptions().SetCorkOption(v != 0)
		return nil

	case linux.TCP_QUICKACK:
		if len(optVal) < sizeOfInt32 {
			return syserr.ErrInvalidArgument
		}

		v := hostarch.ByteOrder.Uint32(optVal)
		ep.SocketOptions().SetQuickAck(v != 0)
		return nil

	case linux.TCP_MAXSEG:
		if len(optVal) < sizeOfInt32 {
			return syserr.ErrInvalidArgument
		}

		v := hostarch.ByteOrder.Uint32(optVal)
		return syserr.TranslateNetstackError(ep.SetSockOptInt(tcpip.MaxSegOption, int(v)))

	case linux.TCP_KEEPIDLE:
		if len(optVal) < sizeOfInt32 {
			return syserr.ErrInvalidArgument
		}

		v := hostarch.ByteOrder.Uint32(optVal)
		if v < 1 || v > linux.MAX_TCP_KEEPIDLE {
			return syserr.ErrInvalidArgument
		}
		opt := tcpip.KeepaliveIdleOption(time.Second * time.Duration(v))
		return syserr.TranslateNetstackError(ep.SetSockOpt(&opt))

	case linux.TCP_KEEPINTVL:
		if len(optVal) < sizeOfInt32 {
			return syserr.ErrInvalidArgument
		}

		v := hostarch.ByteOrder.Uint32(optVal)
		if v < 1 || v > linux.MAX_TCP_KEEPINTVL {
			return syserr.ErrInvalidArgument
		}
		opt := tcpip.KeepaliveIntervalOption(time.Second * time.Duration(v))
		return syserr.TranslateNetstackError(ep.SetSockOpt(&opt))

	case linux.TCP_KEEPCNT:
		if len(optVal) < sizeOfInt32 {
			return syserr.ErrInvalidArgument
		}

		v := hostarch.ByteOrder.Uint32(optVal)
		if v < 1 || v > linux.MAX_TCP_KEEPCNT {
			return syserr.ErrInvalidArgument
		}
		return syserr.TranslateNetstackError(ep.SetSockOptInt(tcpip.KeepaliveCountOption, int(v)))

	case linux.TCP_USER_TIMEOUT:
		if len(optVal) < sizeOfInt32 {
			return syserr.ErrInvalidArgument
		}

		v := int32(hostarch.ByteOrder.Uint32(optVal))
		if v < 0 {
			return syserr.ErrInvalidArgument
		}
		opt := tcpip.TCPUserTimeoutOption(time.Millisecond * time.Duration(v))
		return syserr.TranslateNetstackError(ep.SetSockOpt(&opt))

	case linux.TCP_CONGESTION:
		v := tcpip.CongestionControlOption(optVal)
		if err := ep.SetSockOpt(&v); err != nil {
			return syserr.TranslateNetstackError(err)
		}
		return nil

	case linux.TCP_LINGER2:
		if len(optVal) < sizeOfInt32 {
			return syserr.ErrInvalidArgument
		}

		v := int32(hostarch.ByteOrder.Uint32(optVal))
		opt := tcpip.TCPLingerTimeoutOption(time.Second * time.Duration(v))
		return syserr.TranslateNetstackError(ep.SetSockOpt(&opt))

	case linux.TCP_DEFER_ACCEPT:
		if len(optVal) < sizeOfInt32 {
			return syserr.ErrInvalidArgument
		}
		v := int32(hostarch.ByteOrder.Uint32(optVal))
		if v < 0 {
			v = 0
		}
		opt := tcpip.TCPDeferAcceptOption(time.Second * time.Duration(v))
		return syserr.TranslateNetstackError(ep.SetSockOpt(&opt))

	case linux.TCP_SYNCNT:
		if len(optVal) < sizeOfInt32 {
			return syserr.ErrInvalidArgument
		}
		v := hostarch.ByteOrder.Uint32(optVal)

		return syserr.TranslateNetstackError(ep.SetSockOptInt(tcpip.TCPSynCountOption, int(v)))

	case linux.TCP_WINDOW_CLAMP:
		if len(optVal) < sizeOfInt32 {
			return syserr.ErrInvalidArgument
		}
		v := hostarch.ByteOrder.Uint32(optVal)

		return syserr.TranslateNetstackError(ep.SetSockOptInt(tcpip.TCPWindowClampOption, int(v)))

	case linux.TCP_INFO,
		linux.TCP_MD5SIG,
		linux.TCP_THIN_LINEAR_TIMEOUTS,
		linux.TCP_THIN_DUPACK,
		linux.TCP_REPAIR,
		linux.TCP_REPAIR_QUEUE,
		linux.TCP_QUEUE_SEQ,
		linux.TCP_REPAIR_OPTIONS,
		linux.TCP_FASTOPEN,
		linux.TCP_TIMESTAMP,
		linux.TCP_NOTSENT_LOWAT,
		linux.TCP_CC_INFO,
		linux.TCP_SAVE_SYN,
		linux.TCP_SAVED_SYN,
		linux.TCP_REPAIR_WINDOW,
		linux.TCP_FASTOPEN_CONNECT,
		linux.TCP_ULP,
		linux.TCP_MD5SIG_EXT,
		linux.TCP_FASTOPEN_KEY,
		linux.TCP_FASTOPEN_NO_COOKIE,
		linux.TCP_ZEROCOPY_RECEIVE,
		linux.TCP_INQ,
		linux.TCP_TX_DELAY:
		// Not supported.
		incrementBadSetSocketOptionMetric(t, &socketLevelTCPFieldValue, name)
		return nil
	default:
		if err, handled := setSockOptTCPCustom(t, s, ep, name, optVal); handled {
			return err
		}
	}
	incrementBadSetSocketOptionMetric(t, &socketLevelTCPFieldValue, name)
	return nil
}

func setSockOptICMPv6(t *kernel.Task, s socket.Socket, ep commonEndpoint, name int, optVal []byte) *syserr.Error {
	if _, ok := ep.(tcpip.Endpoint); !ok {
		log.Warningf("SOL_ICMPV6 options not supported on endpoints other than tcpip.Endpoint: option = %d", name)
		return syserr.ErrUnknownProtocolOption
	}

	if family, _, _ := s.Type(); family != linux.AF_INET6 {
		return syserr.ErrUnknownProtocolOption
	}

	switch name {
	case linux.ICMPV6_FILTER:
		var req linux.ICMP6Filter
		if len(optVal) < req.SizeBytes() {
			return syserr.ErrInvalidArgument
		}

		req.UnmarshalUnsafe(optVal)
		return syserr.TranslateNetstackError(ep.SetSockOpt(&tcpip.ICMPv6Filter{DenyType: req.Filter}))
	default:
		if err, handled := setSockOptICMPv6Custom(t, s, ep, name, optVal); handled {
			return err
		}
	}
	incrementBadSetSocketOptionMetric(t, &socketLevelICMPV6FieldValue, name)
	return nil
}

// setSockOptIPv6 implements SetSockOpt when level is SOL_IPV6.
func setSockOptIPv6(t *kernel.Task, s socket.Socket, ep commonEndpoint, name int, optVal []byte) *syserr.Error {
	if _, ok := ep.(tcpip.Endpoint); !ok {
		log.Warningf("SOL_IPV6 options not supported on endpoints other than tcpip.Endpoint: option = %d", name)
		return syserr.ErrUnknownProtocolOption
	}

	family, _, _ := s.Type()
	if family != linux.AF_INET6 {
		return syserr.ErrUnknownProtocolOption
	}

	switch name {
	case linux.IPV6_CHECKSUM:
		if len(optVal) < sizeOfInt32 {
			return syserr.ErrInvalidArgument
		}

		// int may not be 32-bits so we cast the uint32 to an int32 before casting
		// to an int.
		return syserr.TranslateNetstackError(ep.SetSockOptInt(tcpip.IPv6Checksum, int(int32(hostarch.ByteOrder.Uint32(optVal)))))

	case linux.IPV6_V6ONLY:
		if len(optVal) < sizeOfInt32 {
			return syserr.ErrInvalidArgument
		}

		if socket.IsTCP(s) && tcp.EndpointState(ep.State()) != tcp.StateInitial {
			return syserr.ErrInvalidEndpointState
		} else if socket.IsUDP(s) && transport.DatagramEndpointState(ep.State()) != transport.DatagramEndpointStateInitial {
			return syserr.ErrInvalidEndpointState
		}

		v := hostarch.ByteOrder.Uint32(optVal)
		ep.SocketOptions().SetV6Only(v != 0)
		return nil

	case linux.IPV6_ADD_MEMBERSHIP:
		req, err := copyInMulticastV6Request(optVal)
		if err != nil {
			return err
		}

		return syserr.TranslateNetstackError(ep.SetSockOpt(&tcpip.AddMembershipOption{
			NIC:           tcpip.NICID(req.InterfaceIndex),
			MulticastAddr: tcpip.AddrFrom16(req.MulticastAddr),
		}))

	case linux.IPV6_DROP_MEMBERSHIP:
		req, err := copyInMulticastV6Request(optVal)
		if err != nil {
			return err
		}

		return syserr.TranslateNetstackError(ep.SetSockOpt(&tcpip.RemoveMembershipOption{
			NIC:           tcpip.NICID(req.InterfaceIndex),
			MulticastAddr: tcpip.AddrFrom16(req.MulticastAddr),
		}))

	case linux.IPV6_IPSEC_POLICY,
		linux.IPV6_JOIN_ANYCAST,
		linux.IPV6_LEAVE_ANYCAST,
		// TODO(b/148887420): Add support for IPV6_PKTINFO.
		linux.IPV6_PKTINFO,
		linux.IPV6_ROUTER_ALERT,
		linux.IPV6_XFRM_POLICY,
		linux.MCAST_BLOCK_SOURCE,
		linux.MCAST_JOIN_GROUP,
		linux.MCAST_JOIN_SOURCE_GROUP,
		linux.MCAST_LEAVE_GROUP,
		linux.MCAST_LEAVE_SOURCE_GROUP,
		linux.MCAST_UNBLOCK_SOURCE:
		// Not supported.

	case linux.IPV6_RECVORIGDSTADDR:
		if len(optVal) < sizeOfInt32 {
			return syserr.ErrInvalidArgument
		}
		v := int32(hostarch.ByteOrder.Uint32(optVal))

		ep.SocketOptions().SetReceiveOriginalDstAddress(v != 0)
		return nil

	case linux.IPV6_RECVPKTINFO:
		if len(optVal) < sizeOfInt32 {
			return syserr.ErrInvalidArgument
		}
		v := int32(hostarch.ByteOrder.Uint32(optVal))

		ep.SocketOptions().SetIPv6ReceivePacketInfo(v != 0)
		return nil

	case linux.IPV6_UNICAST_HOPS:
		if len(optVal) < sizeOfInt32 {
			return syserr.ErrInvalidArgument
		}
		v := int32(hostarch.ByteOrder.Uint32(optVal))
		if v < -1 || v > 255 {
			return syserr.ErrInvalidArgument
		}
		return syserr.TranslateNetstackError(ep.SetSockOptInt(tcpip.IPv6HopLimitOption, int(v)))

	case linux.IPV6_RECVHOPLIMIT:
		v, err := parseIntOrChar(optVal)
		if err != nil {
			return err
		}

		ep.SocketOptions().SetReceiveHopLimit(v != 0)
		return nil

	case linux.IPV6_TCLASS:
		if len(optVal) < sizeOfInt32 {
			return syserr.ErrInvalidArgument
		}
		v := int32(hostarch.ByteOrder.Uint32(optVal))
		if v < -1 || v > 255 {
			return syserr.ErrInvalidArgument
		}
		if v == -1 {
			v = 0
		}
		return syserr.TranslateNetstackError(ep.SetSockOptInt(tcpip.IPv6TrafficClassOption, int(v)))

	case linux.IPV6_RECVTCLASS:
		v, err := parseIntOrChar(optVal)
		if err != nil {
			return err
		}

		ep.SocketOptions().SetReceiveTClass(v != 0)
		return nil
	case linux.IPV6_RECVERR:
		if len(optVal) == 0 {
			return nil
		}
		v, err := parseIntOrChar(optVal)
		if err != nil {
			return err
		}
		ep.SocketOptions().SetIPv6RecvError(v != 0)
		return nil

	case linux.IP6T_SO_SET_REPLACE:
		if len(optVal) < linux.SizeOfIP6TReplace {
			return syserr.ErrInvalidArgument
		}

		// Only valid for raw IPv6 sockets.
		if !socket.IsRaw(s) {
			return syserr.ErrProtocolNotAvailable
		}

		stk := inet.StackFromContext(t)
		if stk == nil {
			return syserr.ErrNoDevice
		}
		// Stack must be a netstack stack.
		return netfilter.SetEntries(t.Credentials().UserNamespace, stk.(*Stack).Stack, optVal, true)

	case linux.IP6T_SO_SET_ADD_COUNTERS:
		log.Infof("IP6T_SO_SET_ADD_COUNTERS is not supported")
		return nil
	case linux.IPV6_MULTICAST_LOOP,
		linux.IPV6_MULTICAST_HOPS,
		linux.IPV6_MTU,
		linux.IPV6_MINHOPCOUNT,
		linux.IPV6_RECVERR_RFC4884,
		linux.IPV6_MULTICAST_ALL,
		linux.IPV6_AUTOFLOWLABEL,
		linux.IPV6_DONTFRAG,
		linux.IPV6_ROUTER_ALERT_ISOLATE,
		linux.IPV6_MTU_DISCOVER,
		linux.IPV6_FLOWINFO_SEND,
		linux.IPV6_ADDR_PREFERENCES,
		linux.IPV6_MULTICAST_IF,
		linux.IPV6_UNICAST_IF,
		linux.IPV6_ADDRFORM,
		linux.IPV6_2292PKTINFO,
		linux.IPV6_2292HOPLIMIT,
		linux.IPV6_RECVRTHDR,
		linux.IPV6_2292RTHDR,
		linux.IPV6_RECVHOPOPTS,
		linux.IPV6_2292HOPOPTS,
		linux.IPV6_RECVDSTOPTS,
		linux.IPV6_2292DSTOPTS,
		linux.IPV6_FLOWINFO,
		linux.IPV6_RECVPATHMTU,
		linux.IPV6_TRANSPARENT,
		linux.IPV6_FREEBIND,
		linux.IPV6_HOPOPTS,
		linux.IPV6_RTHDRDSTOPTS,
		linux.IPV6_RTHDR,
		linux.IPV6_DSTOPTS,
		linux.IPV6_2292PKTOPTIONS,
		linux.MCAST_MSFILTER,
		linux.IPV6_FLOWLABEL_MGR,
		linux.IPV6_RECVFRAGSIZE:
		// Not supported, but we choose to silently ignore these for compatibility
		// with old gVisor behavior.
		//
		// FIXME(lucasmanning): Remove the silent failure once we're confident
		// that no users are relying on it.
		incrementBadSetSocketOptionMetric(t, &socketLevelIPV6FieldValue, name)
		return nil
	default:
		if err, handled := setSockOptIPv6Custom(t, s, ep, name, optVal); handled {
			return err
		}
	}
	incrementBadSetSocketOptionMetric(t, &socketLevelIPV6FieldValue, name)
	return nil
}

var (
	inetMulticastRequestSize        = (*linux.InetMulticastRequest)(nil).SizeBytes()
	inetMulticastRequestWithNICSize = (*linux.InetMulticastRequestWithNIC)(nil).SizeBytes()
	inet6MulticastRequestSize       = (*linux.Inet6MulticastRequest)(nil).SizeBytes()
)

// copyInMulticastRequest copies in a variable-size multicast request. The
// kernel determines which structure was passed by its length. IP_MULTICAST_IF
// supports ip_mreqn, ip_mreq and in_addr, while IP_ADD_MEMBERSHIP and
// IP_DROP_MEMBERSHIP only support ip_mreqn and ip_mreq. To handle this,
// allowAddr controls whether in_addr is accepted or rejected.
func copyInMulticastRequest(optVal []byte, allowAddr bool) (linux.InetMulticastRequestWithNIC, *syserr.Error) {
	if len(optVal) < len(linux.InetAddr{}) {
		return linux.InetMulticastRequestWithNIC{}, syserr.ErrInvalidArgument
	}

	if len(optVal) < inetMulticastRequestSize {
		if !allowAddr {
			return linux.InetMulticastRequestWithNIC{}, syserr.ErrInvalidArgument
		}

		var req linux.InetMulticastRequestWithNIC
		copy(req.InterfaceAddr[:], optVal)
		return req, nil
	}

	if len(optVal) >= inetMulticastRequestWithNICSize {
		var req linux.InetMulticastRequestWithNIC
		req.UnmarshalUnsafe(optVal)
		return req, nil
	}

	var req linux.InetMulticastRequestWithNIC
	req.InetMulticastRequest.UnmarshalUnsafe(optVal)
	return req, nil
}

func copyInMulticastV6Request(optVal []byte) (linux.Inet6MulticastRequest, *syserr.Error) {
	if len(optVal) < inet6MulticastRequestSize {
		return linux.Inet6MulticastRequest{}, syserr.ErrInvalidArgument
	}

	var req linux.Inet6MulticastRequest
	req.UnmarshalUnsafe(optVal)
	return req, nil
}

// parseIntOrChar copies either a 32-bit int or an 8-bit uint out of buf.
//
// net/ipv4/ip_sockglue.c:do_ip_setsockopt does this for its socket options.
func parseIntOrChar(buf []byte) (int32, *syserr.Error) {
	if len(buf) == 0 {
		return 0, syserr.ErrInvalidArgument
	}

	if len(buf) >= sizeOfInt32 {
		return int32(hostarch.ByteOrder.Uint32(buf)), nil
	}

	return int32(buf[0]), nil
}

// setSockOptIP implements SetSockOpt when level is SOL_IP.
func setSockOptIP(t *kernel.Task, s socket.Socket, ep commonEndpoint, name int, optVal []byte) *syserr.Error {
	if _, ok := ep.(tcpip.Endpoint); !ok {
		log.Warningf("SOL_IP options not supported on endpoints other than tcpip.Endpoint: option = %d, endpoint = %T", name, ep)
		return syserr.ErrUnknownProtocolOption
	}

	switch name {
	case linux.IP_MULTICAST_TTL:
		v, err := parseIntOrChar(optVal)
		if err != nil {
			return err
		}

		if v == -1 {
			// Linux translates -1 to 1.
			v = 1
		}
		if v < 0 || v > 255 {
			return syserr.ErrInvalidArgument
		}
		return syserr.TranslateNetstackError(ep.SetSockOptInt(tcpip.MulticastTTLOption, int(v)))

	case linux.IP_ADD_MEMBERSHIP:
		req, err := copyInMulticastRequest(optVal, false /* allowAddr */)
		if err != nil {
			return err
		}

		return syserr.TranslateNetstackError(ep.SetSockOpt(&tcpip.AddMembershipOption{
			NIC: tcpip.NICID(req.InterfaceIndex),
			// TODO(igudger): Change AddMembership to use the standard
			// any address representation.
			InterfaceAddr: tcpip.AddrFrom4(req.InterfaceAddr),
			MulticastAddr: tcpip.AddrFrom4(req.MulticastAddr),
		}))

	case linux.IP_DROP_MEMBERSHIP:
		req, err := copyInMulticastRequest(optVal, false /* allowAddr */)
		if err != nil {
			return err
		}

		return syserr.TranslateNetstackError(ep.SetSockOpt(&tcpip.RemoveMembershipOption{
			NIC: tcpip.NICID(req.InterfaceIndex),
			// TODO(igudger): Change DropMembership to use the standard
			// any address representation.
			InterfaceAddr: tcpip.AddrFrom4(req.InterfaceAddr),
			MulticastAddr: tcpip.AddrFrom4(req.MulticastAddr),
		}))

	case linux.IP_MULTICAST_IF:
		req, err := copyInMulticastRequest(optVal, true /* allowAddr */)
		if err != nil {
			return err
		}

		return syserr.TranslateNetstackError(ep.SetSockOpt(&tcpip.MulticastInterfaceOption{
			NIC:           tcpip.NICID(req.InterfaceIndex),
			InterfaceAddr: socket.BytesToIPAddress(req.InterfaceAddr[:]),
		}))

	case linux.IP_MULTICAST_LOOP:
		v, err := parseIntOrChar(optVal)
		if err != nil {
			return err
		}

		ep.SocketOptions().SetMulticastLoop(v != 0)
		return nil

	case linux.MCAST_JOIN_GROUP:
		// FIXME(b/124219304): Implement MCAST_JOIN_GROUP.
		return syserr.ErrInvalidArgument

	case linux.IP_TTL:
		v, err := parseIntOrChar(optVal)
		if err != nil {
			return err
		}

		// -1 means default TTL.
		if v == -1 {
			v = 0
		} else if v < 1 || v > 255 {
			return syserr.ErrInvalidArgument
		}
		return syserr.TranslateNetstackError(ep.SetSockOptInt(tcpip.IPv4TTLOption, int(v)))

	case linux.IP_RECVTTL:
		v, err := parseIntOrChar(optVal)
		if err != nil {
			return err
		}
		ep.SocketOptions().SetReceiveTTL(v != 0)
		return nil

	case linux.IP_TOS:
		if len(optVal) == 0 {
			return nil
		}
		v, err := parseIntOrChar(optVal)
		if err != nil {
			return err
		}
		return syserr.TranslateNetstackError(ep.SetSockOptInt(tcpip.IPv4TOSOption, int(v)))

	case linux.IP_RECVTOS:
		v, err := parseIntOrChar(optVal)
		if err != nil {
			return err
		}
		ep.SocketOptions().SetReceiveTOS(v != 0)
		return nil

	case linux.IP_RECVERR:
		if len(optVal) == 0 {
			return nil
		}
		v, err := parseIntOrChar(optVal)
		if err != nil {
			return err
		}
		ep.SocketOptions().SetIPv4RecvError(v != 0)
		return nil

	case linux.IP_PKTINFO:
		if len(optVal) == 0 {
			return nil
		}
		v, err := parseIntOrChar(optVal)
		if err != nil {
			return err
		}
		ep.SocketOptions().SetReceivePacketInfo(v != 0)
		return nil

	case linux.IP_HDRINCL:
		if len(optVal) == 0 {
			return nil
		}
		v, err := parseIntOrChar(optVal)
		if err != nil {
			return err
		}
		ep.SocketOptions().SetHeaderIncluded(v != 0)
		return nil

	case linux.IP_RECVORIGDSTADDR:
		if len(optVal) == 0 {
			return nil
		}
		v, err := parseIntOrChar(optVal)
		if err != nil {
			return err
		}

		ep.SocketOptions().SetReceiveOriginalDstAddress(v != 0)
		return nil

	case linux.IPT_SO_SET_REPLACE:
		if len(optVal) < linux.SizeOfIPTReplace {
			return syserr.ErrInvalidArgument
		}

		// Only valid for raw IPv4 sockets.
		if family, skType, _ := s.Type(); family != linux.AF_INET || skType != linux.SOCK_RAW {
			return syserr.ErrProtocolNotAvailable
		}

		stk := inet.StackFromContext(t)
		if stk == nil {
			return syserr.ErrNoDevice
		}
		// Stack must be a netstack stack.
		return netfilter.SetEntries(t.Credentials().UserNamespace, stk.(*Stack).Stack, optVal, false)

	case linux.IP_MTU_DISCOVER:
		if len(optVal) == 0 {
			return nil
		}
		v, err := parseIntOrChar(optVal)
		if err != nil {
			return err
		}
		switch v {
		case linux.IP_PMTUDISC_DONT:
			v = int32(tcpip.PMTUDiscoveryDont)
		case linux.IP_PMTUDISC_WANT:
			v = int32(tcpip.PMTUDiscoveryWant)
		case linux.IP_PMTUDISC_DO:
			v = int32(tcpip.PMTUDiscoveryDo)
		case linux.IP_PMTUDISC_PROBE:
			v = int32(tcpip.PMTUDiscoveryProbe)
		case linux.IP_PMTUDISC_INTERFACE, linux.IP_PMTUDISC_OMIT:
			return nil // Noop.
		default:
			return syserr.ErrNotSupported
		}
		return syserr.TranslateNetstackError(ep.SetSockOptInt(tcpip.MTUDiscoverOption, int(v)))
	case linux.IP_RECVOPTS,
		linux.IP_RETOPTS,
		linux.IP_ROUTER_ALERT,
		linux.IP_FREEBIND,
		linux.IP_PASSSEC,
		linux.IP_TRANSPARENT,
		linux.IP_MINTTL,
		linux.IP_NODEFRAG,
		linux.IP_BIND_ADDRESS_NO_PORT,
		linux.IP_UNICAST_IF,
		linux.IP_MULTICAST_ALL,
		linux.IP_CHECKSUM,
		linux.IP_RECVFRAGSIZE,
		linux.IP_RECVERR_RFC4884,
		linux.IP_LOCAL_PORT_RANGE,
		linux.IP_OPTIONS,
		linux.IP_MSFILTER,
		linux.IP_BLOCK_SOURCE,
		linux.IP_UNBLOCK_SOURCE,
		linux.IP_ADD_SOURCE_MEMBERSHIP,
		linux.IP_DROP_SOURCE_MEMBERSHIP,
		linux.MCAST_LEAVE_GROUP,
		linux.MCAST_JOIN_SOURCE_GROUP,
		linux.MCAST_LEAVE_SOURCE_GROUP,
		linux.MCAST_BLOCK_SOURCE,
		linux.MCAST_UNBLOCK_SOURCE,
		linux.MCAST_MSFILTER,
		linux.IP_IPSEC_POLICY,
		linux.IP_XFRM_POLICY,
		linux.IPT_SO_SET_ADD_COUNTERS:
		// Not supported, but we choose to silently ignore these for compatibility
		// with old gVisor behavior.
		//
		// FIXME(lucasmanning): Remove the silent failure once we're confident
		// that no users are relying on it.
		incrementBadSetSocketOptionMetric(t, &socketLevelIPFieldValue, name)
		return nil
	default:
		if err, handled := setSockOptIPCustom(t, s, ep, name, optVal); handled {
			return err
		}
	}
	incrementBadSetSocketOptionMetric(t, &socketLevelIPFieldValue, name)
	return nil
}

func setSockOptPacket(t *kernel.Task, s socket.Socket, ep commonEndpoint, name int, optVal []byte) *syserr.Error {
	switch name {
	case linux.PACKET_RX_RING:
		var tpacketReq linux.TpacketReq
		if len(optVal) < tpacketReq.SizeBytes() {
			return syserr.ErrInvalidArgument
		}
		tpacketReq.UnmarshalBytes(optVal)
		req := tcpip.TpacketReq{
			TpBlockSize: tpacketReq.TpBlockSize,
			TpBlockNr:   tpacketReq.TpBlockNr,
			TpFrameSize: tpacketReq.TpFrameSize,
			TpFrameNr:   tpacketReq.TpFrameNr,
		}
		if err := ep.SetSockOpt(&req); err != nil {
			return syserr.TranslateNetstackError(err)
		}
		if ep, ok := ep.(stack.MappablePacketEndpoint); ok {
			var pme *packetmmap.Endpoint
			if ep.GetPacketMMapEndpoint() != nil {
				pme = ep.GetPacketMMapEndpoint().(*packetmmap.Endpoint)
				if pme.Mapped() {
					return syserr.ErrBusy
				}
			} else {
				pme = &packetmmap.Endpoint{}
			}
			opts := ep.GetPacketMMapOpts(&req, true /* isRx */)
			if opts.Req.TpFrameNr != 0 || opts.Req.TpBlockNr != 0 {
				if err := pme.Init(t, opts); err != nil {
					return syserr.FromError(err)
				}
				ep.SetPacketMMapEndpoint(pme)
			}
		} else {
			return syserr.ErrNotSupported
		}
		return nil
	case linux.PACKET_VERSION:
		if len(optVal) < sizeOfInt32 {
			return syserr.ErrInvalidArgument
		}
		v := hostarch.ByteOrder.Uint32(optVal)
		return syserr.TranslateNetstackError(ep.SetSockOptInt(tcpip.PacketMMapVersionOption, int(v)))
	case linux.PACKET_RESERVE:
		if len(optVal) < sizeOfInt32 {
			return syserr.ErrInvalidArgument
		}
		v := hostarch.ByteOrder.Uint32(optVal)
		return syserr.TranslateNetstackError(ep.SetSockOptInt(tcpip.PacketMMapReserveOption, int(v)))
	case linux.PACKET_ADD_MEMBERSHIP, linux.PACKET_AUXDATA:
		// Silently ignore these options.
		incrementBadSetSocketOptionMetric(t, &socketLevelPacketFieldValue, name)
		return nil
	default:
		incrementBadSetSocketOptionMetric(t, &socketLevelIPFieldValue, name)
		return syserr.ErrNotSupported
	}
}

// GetSockName implements the linux syscall getsockname(2) for sockets backed by
// tcpip.Endpoint.
func (s *sock) GetSockName(*kernel.Task) (linux.SockAddr, uint32, *syserr.Error) {
	addr, err := s.Endpoint.GetLocalAddress()
	if err != nil {
		return nil, 0, syserr.TranslateNetstackError(err)
	}

	a, l := socket.ConvertAddress(s.family, addr)
	return a, l, nil
}

// GetPeerName implements the linux syscall getpeername(2) for sockets backed by
// tcpip.Endpoint.
func (s *sock) GetPeerName(*kernel.Task) (linux.SockAddr, uint32, *syserr.Error) {
	addr, err := s.Endpoint.GetRemoteAddress()
	if err != nil {
		return nil, 0, syserr.TranslateNetstackError(err)
	}

	a, l := socket.ConvertAddress(s.family, addr)
	return a, l, nil
}

func (s *sock) fillCmsgInq(cmsg *socket.ControlMessages) {
	if !s.sockOptInq {
		return
	}
	rcvBufUsed, err := s.Endpoint.GetSockOptInt(tcpip.ReceiveQueueSizeOption)
	if err != nil {
		return
	}
	cmsg.IP.HasInq = true
	cmsg.IP.Inq = int32(rcvBufUsed)
}

func toLinuxPacketType(pktType tcpip.PacketType) uint8 {
	switch pktType {
	case tcpip.PacketHost:
		return linux.PACKET_HOST
	case tcpip.PacketOtherHost:
		return linux.PACKET_OTHERHOST
	case tcpip.PacketOutgoing:
		return linux.PACKET_OUTGOING
	case tcpip.PacketBroadcast:
		return linux.PACKET_BROADCAST
	case tcpip.PacketMulticast:
		return linux.PACKET_MULTICAST
	default:
		panic(fmt.Sprintf("unknown packet type: %d", pktType))
	}
}

// nonBlockingRead issues a non-blocking read.
//
// TODO(b/78348848): Support timestamps for stream sockets.
func (s *sock) nonBlockingRead(ctx context.Context, dst usermem.IOSequence, peek, trunc, senderRequested bool) (int, int, linux.SockAddr, uint32, socket.ControlMessages, *syserr.Error) {
	isPacket := s.isPacketBased()

	readOptions := tcpip.ReadOptions{
		Peek:               peek,
		NeedRemoteAddr:     senderRequested,
		NeedLinkPacketInfo: isPacket,
	}

	// TCP sockets discard the data if MSG_TRUNC is set.
	//
	// This behavior is documented in man 7 tcp:
	// Since version 2.4, Linux supports the use of MSG_TRUNC in the flags
	// argument of recv(2) (and recvmsg(2)). This flag causes the received
	// bytes of data to be discarded, rather than passed back in a
	// caller-supplied  buffer.
	var w io.Writer
	var res tcpip.ReadResult
	var err tcpip.Error

	s.readMu.Lock()
	defer s.readMu.Unlock()

	if !isPacket && trunc {
		w = &tcpip.LimitedWriter{
			W: io.Discard,
			N: dst.NumBytes(),
		}
		res, err = s.Endpoint.Read(w, readOptions)
	} else {
		switch s.Endpoint.(type) {
		case *tcp.Endpoint:
			s.mu.Lock()
			s.readWriter.Init(ctx, dst)
			res, err = s.Endpoint.Read(&s.readWriter, readOptions)
			s.mu.Unlock()
		default:
			res, err = s.Endpoint.Read(dst.Writer(ctx), readOptions)
		}
	}

	if _, ok := err.(*tcpip.ErrBadBuffer); ok && dst.NumBytes() == 0 {
		err = nil
	}
	if err != nil {
		return 0, 0, nil, 0, socket.ControlMessages{}, syserr.TranslateNetstackError(err)
	}
	// Set the control message, even if 0 bytes were read.
	s.updateTimestamp(res.ControlMessages)

	if isPacket {
		var addr linux.SockAddr
		var addrLen uint32
		if senderRequested {
			addr, addrLen = socket.ConvertAddress(s.family, res.RemoteAddr)
			switch v := addr.(type) {
			case *linux.SockAddrLink:
				v.Protocol = socket.Htons(uint16(res.LinkPacketInfo.Protocol))
				v.PacketType = toLinuxPacketType(res.LinkPacketInfo.PktType)
			}
		}

		msgLen := res.Count
		if trunc {
			msgLen = res.Total
		}

		var flags int
		if res.Total > res.Count {
			flags |= linux.MSG_TRUNC
		}

		return msgLen, flags, addr, addrLen, s.netstackToLinuxControlMessages(res.ControlMessages), nil
	}

	if peek {
		// MSG_TRUNC with MSG_PEEK on a TCP socket returns the
		// amount that could be read, and does not write to buffer.
		if trunc {
			// TCP endpoint does not return the total bytes in buffer as numTotal.
			// We need to query it from socket option.
			rql, err := s.Endpoint.GetSockOptInt(tcpip.ReceiveQueueSizeOption)
			if err != nil {
				return 0, 0, nil, 0, socket.ControlMessages{}, syserr.TranslateNetstackError(err)
			}
			msgLen := int(dst.NumBytes())
			if msgLen > rql {
				msgLen = rql
			}
			return msgLen, 0, nil, 0, socket.ControlMessages{}, nil
		}
	} else if n := res.Count; n != 0 {
		s.Endpoint.ModerateRecvBuf(n)
	}

	cmsg := s.netstackToLinuxControlMessages(res.ControlMessages)
	s.fillCmsgInq(&cmsg)
	return res.Count, 0, nil, 0, cmsg, syserr.TranslateNetstackError(err)
}

func (s *sock) netstackToLinuxControlMessages(cm tcpip.ReceivableControlMessages) socket.ControlMessages {
	readCM := socket.NewIPControlMessages(s.family, cm)
	return socket.ControlMessages{
		IP: socket.IPControlMessages{
			HasTimestamp:       readCM.HasTimestamp && s.sockOptTimestamp,
			Timestamp:          readCM.Timestamp,
			HasInq:             readCM.HasInq,
			Inq:                readCM.Inq,
			HasTOS:             readCM.HasTOS,
			TOS:                readCM.TOS,
			HasTClass:          readCM.HasTClass,
			TClass:             readCM.TClass,
			HasTTL:             readCM.HasTTL,
			TTL:                readCM.TTL,
			HasHopLimit:        readCM.HasHopLimit,
			HopLimit:           readCM.HopLimit,
			HasIPPacketInfo:    readCM.HasIPPacketInfo,
			PacketInfo:         readCM.PacketInfo,
			HasIPv6PacketInfo:  readCM.HasIPv6PacketInfo,
			IPv6PacketInfo:     readCM.IPv6PacketInfo,
			OriginalDstAddress: readCM.OriginalDstAddress,
			SockErr:            readCM.SockErr,
		},
	}
}

func (s *sock) linuxToNetstackControlMessages(cm socket.ControlMessages) tcpip.SendableControlMessages {
	return tcpip.SendableControlMessages{
		HasTTL:      cm.IP.HasTTL,
		TTL:         uint8(cm.IP.TTL),
		HasHopLimit: cm.IP.HasHopLimit,
		HopLimit:    uint8(cm.IP.HopLimit),
	}
}

// updateTimestamp sets the timestamp for SIOCGSTAMP. It should be called after
// successfully writing packet data out to userspace.
//
// Precondition: s.readMu must be locked.
func (s *sock) updateTimestamp(cm tcpip.ReceivableControlMessages) {
	// Save the SIOCGSTAMP timestamp only if SO_TIMESTAMP is disabled.
	if !s.sockOptTimestamp {
		s.timestampValid = true
		s.timestamp = cm.Timestamp
	}
}

// dequeueErr is analogous to net/core/skbuff.c:sock_dequeue_err_skb().
func (s *sock) dequeueErr() *tcpip.SockError {
	so := s.Endpoint.SocketOptions()
	err := so.DequeueErr()
	if err == nil {
		return nil
	}

	// Update socket error to reflect ICMP errors in queue.
	if nextErr := so.PeekErr(); nextErr != nil && nextErr.Cause.Origin().IsICMPErr() {
		so.SetLastError(nextErr.Err)
	} else if err.Cause.Origin().IsICMPErr() {
		so.SetLastError(nil)
	}
	return err
}

// addrFamilyFromNetProto returns the address family identifier for the given
// network protocol.
func addrFamilyFromNetProto(net tcpip.NetworkProtocolNumber) int {
	switch net {
	case header.IPv4ProtocolNumber:
		return linux.AF_INET
	case header.IPv6ProtocolNumber:
		return linux.AF_INET6
	default:
		panic(fmt.Sprintf("invalid net proto for addr family inference: %d", net))
	}
}

// recvErr handles MSG_ERRQUEUE for recvmsg(2).
// This is analogous to net/ipv4/ip_sockglue.c:ip_recv_error().
func (s *sock) recvErr(t *kernel.Task, dst usermem.IOSequence) (int, int, linux.SockAddr, uint32, socket.ControlMessages, *syserr.Error) {
	sockErr := s.dequeueErr()
	if sockErr == nil {
		return 0, 0, nil, 0, socket.ControlMessages{}, syserr.ErrTryAgain
	}
	if sockErr.Payload != nil {
		defer sockErr.Payload.Release()
	}

	// The payload of the original packet that caused the error is passed as
	// normal data via msg_iovec.  -- recvmsg(2)
	msgFlags := linux.MSG_ERRQUEUE
	if int(dst.NumBytes()) < sockErr.Payload.Size() {
		msgFlags |= linux.MSG_TRUNC
	}
	n, err := dst.CopyOut(t, sockErr.Payload.AsSlice())

	// The original destination address of the datagram that caused the error is
	// supplied via msg_name.  -- recvmsg(2)
	dstAddr, dstAddrLen := socket.ConvertAddress(addrFamilyFromNetProto(sockErr.NetProto), sockErr.Dst)
	cmgs := socket.ControlMessages{IP: socket.NewIPControlMessages(s.family, tcpip.ReceivableControlMessages{SockErr: sockErr})}
	return n, msgFlags, dstAddr, dstAddrLen, cmgs, syserr.FromError(err)
}

// RecvMsg implements the linux syscall recvmsg(2) for sockets backed by
// tcpip.Endpoint.
func (s *sock) RecvMsg(t *kernel.Task, dst usermem.IOSequence, flags int, haveDeadline bool, deadline ktime.Time, senderRequested bool, _ uint64) (n int, msgFlags int, senderAddr linux.SockAddr, senderAddrLen uint32, controlMessages socket.ControlMessages, err *syserr.Error) {
	if flags&linux.MSG_ERRQUEUE != 0 {
		return s.recvErr(t, dst)
	}

	trunc := flags&linux.MSG_TRUNC != 0
	peek := flags&linux.MSG_PEEK != 0
	dontWait := flags&linux.MSG_DONTWAIT != 0
	waitAll := flags&linux.MSG_WAITALL != 0
	if senderRequested && !s.isPacketBased() {
		// Stream sockets ignore the sender address.
		senderRequested = false
	}
	n, msgFlags, senderAddr, senderAddrLen, controlMessages, err = s.nonBlockingRead(t, dst, peek, trunc, senderRequested)

	if s.isPacketBased() && err == syserr.ErrClosedForReceive && flags&linux.MSG_DONTWAIT != 0 {
		// In this situation we should return EAGAIN.
		return 0, 0, nil, 0, socket.ControlMessages{}, syserr.ErrTryAgain
	}

	if err != nil && (err != syserr.ErrWouldBlock || dontWait) {
		// Read failed and we should not retry.
		return 0, 0, nil, 0, socket.ControlMessages{}, err
	}

	if err == nil && (dontWait || !waitAll || s.isPacketBased() || int64(n) >= dst.NumBytes()) {
		// We got all the data we need.
		return
	}

	// Don't overwrite any data we received.
	dst = dst.DropFirst(n)

	// We'll have to block. Register for notifications and keep trying to
	// send all the data.
	e, ch := waiter.NewChannelEntry(waiter.ReadableEvents)
	s.EventRegister(&e)
	defer s.EventUnregister(&e)

	for {
		var rn int
		rn, msgFlags, senderAddr, senderAddrLen, controlMessages, err = s.nonBlockingRead(t, dst, peek, trunc, senderRequested)
		n += rn
		if err != nil && err != syserr.ErrWouldBlock {
			// Always stop on errors other than would block as we generally
			// won't be able to get any more data. Eat the error if we got
			// any data.
			if n > 0 {
				err = nil
			}
			return
		}
		if err == nil && (s.isPacketBased() || !waitAll || int64(rn) >= dst.NumBytes()) {
			// We got all the data we need.
			return
		}
		dst = dst.DropFirst(rn)

		if err := t.BlockWithDeadline(ch, haveDeadline, deadline); err != nil {
			if n > 0 {
				return n, msgFlags, senderAddr, senderAddrLen, controlMessages, nil
			}
			if linuxerr.Equals(linuxerr.ETIMEDOUT, err) {
				return 0, 0, nil, 0, socket.ControlMessages{}, syserr.ErrTryAgain
			}
			return 0, 0, nil, 0, socket.ControlMessages{}, syserr.FromError(err)
		}
	}
}

// SendMsg implements the linux syscall sendmsg(2) for sockets backed by
// tcpip.Endpoint.
func (s *sock) SendMsg(t *kernel.Task, src usermem.IOSequence, to []byte, flags int, haveDeadline bool, deadline ktime.Time, controlMessages socket.ControlMessages) (int, *syserr.Error) {
	// Reject Unix control messages.
	if !controlMessages.Unix.Empty() {
		return 0, syserr.ErrInvalidArgument
	}

	var addr *tcpip.FullAddress
	if len(to) > 0 {
		addrBuf, family, err := socket.AddressAndFamily(to)
		if err != nil {
			return 0, err
		}
		if !s.checkFamily(family, false /* exact */) {
			return 0, syserr.ErrInvalidArgument
		}
		addrBuf = s.mapFamily(addrBuf, family)

		addr = &addrBuf
	}

	opts := tcpip.WriteOptions{
		To:              addr,
		More:            flags&linux.MSG_MORE != 0,
		EndOfRecord:     flags&linux.MSG_EOR != 0,
		ControlMessages: s.linuxToNetstackControlMessages(controlMessages),
	}

	r := src.Reader(t)
	var (
		total int64
		entry waiter.Entry
		ch    <-chan struct{}
	)
	for {
		n, err := s.Endpoint.Write(r, opts)
		total += n
		if flags&linux.MSG_DONTWAIT != 0 {
			return int(total), syserr.TranslateNetstackError(err)
		}
		block := true
		switch err.(type) {
		case nil:
			block = total != src.NumBytes()
		case *tcpip.ErrWouldBlock:
		default:
			block = false
		}
		if block {
			if ch == nil {
				// We'll have to block. Register for notification and keep trying to
				// send all the data.
				entry, ch = waiter.NewChannelEntry(waiter.WritableEvents)
				s.EventRegister(&entry)
				defer s.EventUnregister(&entry)
			} else {
				// Don't wait immediately after registration in case more data
				// became available between when we last checked and when we setup
				// the notification.
				if err := t.BlockWithDeadline(ch, haveDeadline, deadline); err != nil {
					if linuxerr.Equals(linuxerr.ETIMEDOUT, err) {
						return int(total), syserr.ErrTryAgain
					}
					// handleIOError will consume errors from t.Block if needed.
					return int(total), syserr.FromError(err)
				}
			}
			continue
		}
		return int(total), syserr.TranslateNetstackError(err)
	}
}

// Ioctl implements vfs.FileDescriptionImpl.
func (s *sock) Ioctl(ctx context.Context, uio usermem.IO, sysno uintptr, args arch.SyscallArguments) (uintptr, error) {
	t := kernel.TaskFromContext(ctx)
	if t == nil {
		panic("ioctl(2) may only be called from a task goroutine")
	}

	// SIOCGSTAMP is implemented by netstack rather than all commonEndpoint
	// sockets.
	// TODO(b/78348848): Add a commonEndpoint method to support SIOCGSTAMP.
	switch args[1].Int() {
	case linux.SIOCGSTAMP:
		s.readMu.Lock()
		defer s.readMu.Unlock()
		if !s.timestampValid {
			return 0, linuxerr.ENOENT
		}

		tv := linux.NsecToTimeval(s.timestamp.UnixNano())
		_, err := tv.CopyOut(t, args[2].Pointer())
		return 0, err

	case linux.TIOCINQ:
		v, terr := s.Endpoint.GetSockOptInt(tcpip.ReceiveQueueSizeOption)
		if terr != nil {
			return 0, syserr.TranslateNetstackError(terr).ToError()
		}

		if v > math.MaxInt32 {
			v = math.MaxInt32
		}

		// Copy result to userspace.
		vP := primitive.Int32(v)
		_, err := vP.CopyOut(t, args[2].Pointer())
		return 0, err
	}

	return Ioctl(ctx, s.Endpoint, uio, sysno, args)
}

// Ioctl performs a socket ioctl.
func Ioctl(ctx context.Context, ep commonEndpoint, io usermem.IO, sysno uintptr, args arch.SyscallArguments) (uintptr, error) {
	t := kernel.TaskFromContext(ctx)
	if t == nil {
		panic("ioctl(2) may only be called from a task goroutine")
	}

	switch arg := int(args[1].Int()); arg {
	case linux.SIOCGIFFLAGS,
		linux.SIOCGIFADDR,
		linux.SIOCGIFBRDADDR,
		linux.SIOCGIFDSTADDR,
		linux.SIOCGIFHWADDR,
		linux.SIOCGIFINDEX,
		linux.SIOCGIFMAP,
		linux.SIOCGIFMETRIC,
		linux.SIOCGIFMTU,
		linux.SIOCGIFNAME,
		linux.SIOCGIFNETMASK,
		linux.SIOCGIFTXQLEN,
		linux.SIOCETHTOOL:

		var ifr linux.IFReq
		if _, err := ifr.CopyIn(t, args[2].Pointer()); err != nil {
			return 0, err
		}
		if err := interfaceIoctl(ctx, io, arg, &ifr); err != nil {
			return 0, err.ToError()
		}
		_, err := ifr.CopyOut(t, args[2].Pointer())
		return 0, err

	case linux.SIOCGIFCONF:
		// Return a list of interface addresses or the buffer size
		// necessary to hold the list.
		var ifc linux.IFConf
		if _, err := ifc.CopyIn(t, args[2].Pointer()); err != nil {
			return 0, err
		}

		if err := ifconfIoctl(ctx, t, io, &ifc); err != nil {
			return 0, err
		}

		_, err := ifc.CopyOut(t, args[2].Pointer())
		return 0, err

	case linux.TIOCINQ:
		v, terr := ep.GetSockOptInt(tcpip.ReceiveQueueSizeOption)
		if terr != nil {
			return 0, syserr.TranslateNetstackError(terr).ToError()
		}

		if v > math.MaxInt32 {
			v = math.MaxInt32
		}
		// Copy result to userspace.
		vP := primitive.Int32(v)
		_, err := vP.CopyOut(t, args[2].Pointer())
		return 0, err

	case linux.TIOCOUTQ:
		v, terr := ep.GetSockOptInt(tcpip.SendQueueSizeOption)
		if terr != nil {
			return 0, syserr.TranslateNetstackError(terr).ToError()
		}

		if v > math.MaxInt32 {
			v = math.MaxInt32
		}

		// Copy result to userspace.
		vP := primitive.Int32(v)
		_, err := vP.CopyOut(t, args[2].Pointer())
		return 0, err

	case linux.SIOCGIFMEM, linux.SIOCGIFPFLAGS, linux.SIOCGMIIPHY, linux.SIOCGMIIREG:
		// Not supported.
	}

	return 0, linuxerr.ENOTTY
}

// interfaceIoctl implements interface requests.
func interfaceIoctl(ctx context.Context, _ usermem.IO, arg int, ifr *linux.IFReq) *syserr.Error {
	var (
		iface inet.Interface
		index int32
		found bool
	)

	// Find the relevant device.
	stk := inet.StackFromContext(ctx)
	if stk == nil {
		return syserr.ErrNoDevice
	}

	// SIOCGIFNAME uses ifr.ifr_ifindex rather than ifr.ifr_name to
	// identify a device.
	if arg == linux.SIOCGIFNAME {
		// Gets the name of the interface given the interface index
		// stored in ifr_ifindex.
		index = int32(hostarch.ByteOrder.Uint32(ifr.Data[:4]))
		if iface, ok := stk.Interfaces()[index]; ok {
			ifr.SetName(iface.Name)
			return nil
		}
		return syserr.ErrNoDevice
	}

	// Find the relevant device.
	for index, iface = range stk.Interfaces() {
		if iface.Name == ifr.Name() {
			found = true
			break
		}
	}
	if !found {
		return syserr.ErrNoDevice
	}

	switch arg {
	case linux.SIOCGIFINDEX:
		// Copy out the index to the data.
		hostarch.ByteOrder.PutUint32(ifr.Data[:], uint32(index))

	case linux.SIOCGIFHWADDR:
		// Copy the hardware address out.
		//
		// Refer: https://linux.die.net/man/7/netdevice
		// SIOCGIFHWADDR, SIOCSIFHWADDR
		//
		// Get or set the hardware address of a device using
		// ifr_hwaddr. The hardware address is specified in a struct
		// sockaddr. sa_family contains the ARPHRD_* device type,
		// sa_data the L2 hardware address starting from byte 0. Setting
		// the hardware address is a privileged operation.
		hostarch.ByteOrder.PutUint16(ifr.Data[:], iface.DeviceType)
		n := copy(ifr.Data[2:], iface.Addr)
		for i := 2 + n; i < len(ifr.Data); i++ {
			ifr.Data[i] = 0 // Clear padding.
		}

	case linux.SIOCGIFFLAGS:
		f, err := interfaceStatusFlags(stk, iface.Name)
		if err != nil {
			return err
		}
		// Drop the flags that don't fit in the size that we need to return. This
		// matches Linux behavior.
		hostarch.ByteOrder.PutUint16(ifr.Data[:2], uint16(f))

	case linux.SIOCGIFADDR:
		// Copy the IPv4 address out.
		for _, addr := range stk.InterfaceAddrs()[index] {
			// This ioctl is only compatible with AF_INET addresses.
			if addr.Family != linux.AF_INET {
				continue
			}
			copy(ifr.Data[4:8], addr.Addr)
			break
		}

	case linux.SIOCGIFMETRIC:
		// Gets the metric of the device. As per netdevice(7), this
		// always just sets ifr_metric to 0.
		hostarch.ByteOrder.PutUint32(ifr.Data[:4], 0)

	case linux.SIOCGIFMTU:
		// Gets the MTU of the device.
		hostarch.ByteOrder.PutUint32(ifr.Data[:4], iface.MTU)

	case linux.SIOCGIFMAP:
		// Gets the hardware parameters of the device.
		// TODO(gvisor.dev/issue/505): Implement.

	case linux.SIOCGIFTXQLEN:
		// Gets the transmit queue length of the device.
		// TODO(gvisor.dev/issue/505): Implement.

	case linux.SIOCGIFDSTADDR:
		// Gets the destination address of a point-to-point device.
		// TODO(gvisor.dev/issue/505): Implement.

	case linux.SIOCGIFBRDADDR:
		// Gets the broadcast address of a device.
		// TODO(gvisor.dev/issue/505): Implement.

	case linux.SIOCGIFNETMASK:
		// Gets the network mask of a device.
		for _, addr := range stk.InterfaceAddrs()[index] {
			// This ioctl is only compatible with AF_INET addresses.
			if addr.Family != linux.AF_INET {
				continue
			}
			// Populate ifr.ifr_netmask (type sockaddr).
			hostarch.ByteOrder.PutUint16(ifr.Data[0:], uint16(linux.AF_INET))
			hostarch.ByteOrder.PutUint16(ifr.Data[2:], 0)
			var mask uint32 = 0xffffffff << (32 - addr.PrefixLen)
			// Netmask is expected to be returned as a big endian
			// value.
			binary.BigEndian.PutUint32(ifr.Data[4:8], mask)
			break
		}

	case linux.SIOCETHTOOL:
		// Stubbed out for now, Ideally we should implement the required
		// sub-commands for ETHTOOL
		//
		// See:
		// https://github.com/torvalds/linux/blob/aa0c9086b40c17a7ad94425b3b70dd1fdd7497bf/net/core/dev_ioctl.c
		return syserr.ErrEndpointOperation

	default:
		// Not a valid call.
		return syserr.ErrInvalidArgument
	}

	return nil
}

// ifconfIoctl populates a struct ifconf for the SIOCGIFCONF ioctl.
func ifconfIoctl(ctx context.Context, t *kernel.Task, _ usermem.IO, ifc *linux.IFConf) error {
	// If Ptr is NULL, return the necessary buffer size via Len.
	// Otherwise, write up to Len bytes starting at Ptr containing ifreq
	// structs.
	stk := inet.StackFromContext(ctx)
	if stk == nil {
		return syserr.ErrNoDevice.ToError()
	}

	if ifc.Ptr == 0 {
		ifc.Len = int32(len(stk.Interfaces())) * int32(linux.SizeOfIFReq)
		return nil
	}

	max := ifc.Len
	ifc.Len = 0
	for key, ifaceAddrs := range stk.InterfaceAddrs() {
		iface := stk.Interfaces()[key]
		for _, ifaceAddr := range ifaceAddrs {
			// Don't write past the end of the buffer.
			if ifc.Len+int32(linux.SizeOfIFReq) > max {
				break
			}
			if ifaceAddr.Family != linux.AF_INET {
				continue
			}

			// Populate ifr.ifr_addr.
			ifr := linux.IFReq{}
			ifr.SetName(iface.Name)
			hostarch.ByteOrder.PutUint16(ifr.Data[0:2], uint16(ifaceAddr.Family))
			hostarch.ByteOrder.PutUint16(ifr.Data[2:4], 0)
			copy(ifr.Data[4:8], ifaceAddr.Addr[:4])

			// Copy the ifr to userspace.
			dst := uintptr(ifc.Ptr) + uintptr(ifc.Len)
			ifc.Len += int32(linux.SizeOfIFReq)
			if _, err := ifr.CopyOut(t, hostarch.Addr(dst)); err != nil {
				return err
			}
		}
	}
	return nil
}

// interfaceStatusFlags returns status flags for an interface in the stack.
// Flag values and meanings are described in greater detail in netdevice(7) in
// the SIOCGIFFLAGS section.
func interfaceStatusFlags(stack inet.Stack, name string) (uint32, *syserr.Error) {
	// We should only ever be passed a netstack.Stack.
	epstack, ok := stack.(*Stack)
	if !ok {
		return 0, errStackType
	}

	// Find the NIC corresponding to this interface.
	for _, info := range epstack.Stack.NICInfo() {
		if info.Name == name {
			return nicStateFlagsToLinux(info.Flags), nil
		}
	}
	return 0, syserr.ErrNoDevice
}

func nicStateFlagsToLinux(f stack.NICStateFlags) uint32 {
	var rv uint32
	if f.Up {
		rv |= linux.IFF_UP | linux.IFF_LOWER_UP
	}
	if f.Running {
		rv |= linux.IFF_RUNNING
	}
	if f.Promiscuous {
		rv |= linux.IFF_PROMISC
	}
	if f.Loopback {
		rv |= linux.IFF_LOOPBACK
	}
	return rv
}

// State implements socket.Socket.State. State translates the internal state
// returned by netstack to values defined by Linux.
func (s *sock) State() uint32 {
	if s.family != linux.AF_INET && s.family != linux.AF_INET6 {
		// States not implemented for this socket's family.
		return 0
	}

	switch {
	case socket.IsTCP(s):
		// TCP socket.
		switch tcp.EndpointState(s.Endpoint.State()) {
		case tcp.StateEstablished:
			return linux.TCP_ESTABLISHED
		case tcp.StateSynSent:
			return linux.TCP_SYN_SENT
		case tcp.StateSynRecv:
			return linux.TCP_SYN_RECV
		case tcp.StateFinWait1:
			return linux.TCP_FIN_WAIT1
		case tcp.StateFinWait2:
			return linux.TCP_FIN_WAIT2
		case tcp.StateTimeWait:
			return linux.TCP_TIME_WAIT
		case tcp.StateClose, tcp.StateInitial, tcp.StateBound, tcp.StateConnecting, tcp.StateError:
			return linux.TCP_CLOSE
		case tcp.StateCloseWait:
			return linux.TCP_CLOSE_WAIT
		case tcp.StateLastAck:
			return linux.TCP_LAST_ACK
		case tcp.StateListen:
			return linux.TCP_LISTEN
		case tcp.StateClosing:
			return linux.TCP_CLOSING
		default:
			// Internal or unknown state.
			return 0
		}
	case socket.IsUDP(s):
		// UDP socket.
		switch transport.DatagramEndpointState(s.Endpoint.State()) {
		case transport.DatagramEndpointStateInitial, transport.DatagramEndpointStateBound, transport.DatagramEndpointStateClosed:
			return linux.TCP_CLOSE
		case transport.DatagramEndpointStateConnected:
			return linux.TCP_ESTABLISHED
		default:
			return 0
		}
	case socket.IsICMP(s):
		// We don't support this yet.
	case socket.IsRaw(s):
		// We don't support this yet.
	default:
		// Unknown transport protocol, how did we make this socket?
		log.Warningf("Unknown transport protocol for an existing socket: family=%v, type=%v, protocol=%v, internal type %v", s.family, s.skType, s.protocol, reflect.TypeOf(s.Endpoint).Elem())
		return 0
	}

	return 0
}

// Type implements socket.Socket.Type.
func (s *sock) Type() (family int, skType linux.SockType, protocol int) {
	return s.family, s.skType, s.protocol
}

// EventRegister implements waiter.Waitable.
func (s *sock) EventRegister(e *waiter.Entry) error {
	s.Queue.EventRegister(e)
	return nil
}

// EventUnregister implements waiter.Waitable.EventUnregister.
func (s *sock) EventUnregister(e *waiter.Entry) {
	s.Queue.EventUnregister(e)
}

// ConfigureMMap implements vfs.FileDescriptionImpl.ConfigureMMap.
func (s *sock) ConfigureMMap(ctx context.Context, opts *memmap.MMapOpts) error {
	if mappablePacketEP, ok := s.Endpoint.(stack.MappablePacketEndpoint); ok {
		packetMMapEP := mappablePacketEP.GetPacketMMapEndpoint()
		if packetMMapEP == nil {
			return linuxerr.ENODEV
		}
		ep := packetMMapEP.(*packetmmap.Endpoint)
		if err := vfs.GenericConfigureMMap(&s.vfsfd, ep, opts); err != nil {
			return err
		}
		return ep.ConfigureMMap(ctx, opts)
	}
	return linuxerr.ENODEV
}
