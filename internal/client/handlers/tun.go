package handlers

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/NHAS/reverse_ssh/pkg/logger"
	"github.com/go-ping/ping"
	"gvisor.dev/gvisor/pkg/buffer"
	"inet.af/tcpproxy"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/header/parse"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/raw"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"

	"golang.org/x/crypto/ssh"
)

func Tun(newChannel ssh.NewChannel, l logger.Logger) {

	defer func() {
		if r := recover(); r != nil {
			l.Error("Recovered panic from tun driver %v", r)
		}
	}()

	var tunInfo struct {
		Mode uint32
		No   uint32
	}

	err := ssh.Unmarshal(newChannel.ExtraData(), &tunInfo)
	if err != nil {
		newChannel.Reject(ssh.ConnectionFailed, "connection closed")
		l.Warning("Unable to accept new channel %s", err)
		return
	}

	if tunInfo.Mode != 1 {
		newChannel.Reject(ssh.ConnectionFailed, "connection closed")
		return
	}

	tunnel, req, err := newChannel.Accept()
	if err != nil {
		newChannel.Reject(ssh.ConnectionFailed, "connection closed")
		l.Warning("Unable to accept new channel %s", err)
		return
	}
	defer tunnel.Close()

	// Create a new gvisor userland network stack.
	ns := stack.New(stack.Options{
		NetworkProtocols: []stack.NetworkProtocolFactory{
			ipv4.NewProtocol,
			ipv6.NewProtocol,
		},
		TransportProtocols: []stack.TransportProtocolFactory{
			tcp.NewProtocol,
			udp.NewProtocol,
			icmp.NewProtocol4,
		},
		HandleLocal: false,
	})
	defer ns.Close()

	linkEP := NewSSHEndpoint(tunnel)

	const NICID = 1
	// Create a new NIC
	if err := ns.CreateNIC(NICID, linkEP); err != nil {
		l.Error("CreateNIC: %v", err)
		return
	}

	err = icmpResponder(ns)
	if err != nil {
		l.Error("Unable to create icmp responder: %v", err)
		return
	}

	// Forward TCP connections
	tcpHandler := tcp.NewForwarder(ns, 30000, 4000, forwardTCP)

	// Forward UDP connections
	udpHandler := udp.NewForwarder(ns, forwardUDP)

	// Register forwarders
	ns.SetTransportProtocolHandler(tcp.ProtocolNumber, tcpHandler.HandlePacket)
	ns.SetTransportProtocolHandler(udp.ProtocolNumber, udpHandler.HandlePacket)

	// Allow all routes by default
	ns.SetRouteTable([]tcpip.Route{
		{
			Destination: header.IPv4EmptySubnet,
			NIC:         NICID,
		},
		{
			Destination: header.IPv6EmptySubnet,
			NIC:         NICID,
		},
	})

	// Disable forwarding
	ns.SetForwardingDefaultAndAllNICs(ipv4.ProtocolNumber, false)
	ns.SetForwardingDefaultAndAllNICs(ipv6.ProtocolNumber, false)

	// Enable TCP SACK
	nsacks := tcpip.TCPSACKEnabled(true)
	ns.SetTransportProtocolOption(tcp.ProtocolNumber, &nsacks)

	// Disable SYN-Cookies, as this can mess with nmap scans
	synCookies := tcpip.TCPAlwaysUseSynCookies(false)
	ns.SetTransportProtocolOption(tcp.ProtocolNumber, &synCookies)

	// Allow packets from all sources/destinations
	ns.SetPromiscuousMode(NICID, true)
	ns.SetSpoofing(NICID, true)

	ssh.DiscardRequests(req)

	l.Info("Tunnel ended")

}

func forwardUDP(request *udp.ForwarderRequest) {

	id := request.ID()

	var wq waiter.Queue
	ep, iperr := request.CreateEndpoint(&wq)
	if iperr != nil {
		log.Println("[+] failed to create endpoint for udp: ", iperr)

		return
	}

	log.Printf("tun [+] %s -> %s:%d/udp\n", id.RemoteAddress, id.LocalAddress, id.LocalPort)

	p, _ := NewUDPProxy(&autoStoppingListener{underlying: gonet.NewUDPConn(&wq, ep)}, func() (net.Conn, error) {
		return net.Dial("udp", fmt.Sprintf("%s:%d", id.LocalAddress, id.LocalPort))
	})
	go func() {
		p.Run()

		// note that at this point packets that are sent to the current forwarder session
		// will be dropped. We will start processing the packets again when we get a new
		// forwarder request.
		ep.Close()
	}()

}

func forwardTCP(request *tcp.ForwarderRequest) {

	id := request.ID()

	fwdDst := net.TCPAddr{
		IP:   net.ParseIP(id.LocalAddress.String()),
		Port: int(id.LocalPort),
	}

	//log.Printf("[+] %s -> %s:%d/tcp\n", id.RemoteAddress, id.LocalAddress, id.LocalPort)

	outbound, err := net.DialTimeout("tcp", fwdDst.String(), 5*time.Second)
	if err != nil {
		log.Printf("failed to dial: %s:%d/tcp", id.LocalAddress, id.LocalPort)
		request.Complete(true)
		return
	}

	var wq waiter.Queue
	ep, errTcp := request.CreateEndpoint(&wq)

	//request.Complete(false)

	if errTcp != nil {
		// ErrConnectionRefused is a transient error
		if _, ok := errTcp.(*tcpip.ErrConnectionRefused); !ok {
			log.Printf("could not create endpoint: %s", errTcp)
		}
		return
	}

	remote := tcpproxy.DialProxy{
		DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
			return outbound, nil
		},
	}
	remote.HandleConn(gonet.NewTCPConn(&wq, ep))
}

type SSHEndpoint struct {
	dispatcher stack.NetworkDispatcher
	tunnel     ssh.Channel

	lock sync.Mutex
}

func NewSSHEndpoint(dev ssh.Channel) *SSHEndpoint {
	return &SSHEndpoint{
		tunnel: dev,
	}
}

func (m *SSHEndpoint) Close() {
	log.Println("closed")
	debug.PrintStack()
	m.tunnel.Close()
}

func (m *SSHEndpoint) SetOnCloseAction(func()) {

}

func (m *SSHEndpoint) SetLinkAddress(addr tcpip.LinkAddress) {

}

func (m *SSHEndpoint) SetMTU(uint32) {

}

func (m *SSHEndpoint) ParseHeader(*stack.PacketBuffer) bool {
	return true
}

// MTU implements stack.LinkEndpoint.
func (m *SSHEndpoint) MTU() uint32 {
	return 1500
}

// Capabilities implements stack.LinkEndpoint.
func (m *SSHEndpoint) Capabilities() stack.LinkEndpointCapabilities {
	return stack.CapabilityNone
}

// MaxHeaderLength implements stack.LinkEndpoint.
func (m *SSHEndpoint) MaxHeaderLength() uint16 {
	return 0
}

// LinkAddress implements stack.LinkEndpoint.
func (m *SSHEndpoint) LinkAddress() tcpip.LinkAddress {
	return ""
}

// Attach implements stack.LinkEndpoint.
func (m *SSHEndpoint) Attach(dispatcher stack.NetworkDispatcher) {
	m.dispatcher = dispatcher
	go m.dispatchLoop()
}

func (m *SSHEndpoint) dispatchLoop() {
	for {
		// Copying ssh spec
		packet := make([]byte, 32*1024)

		n, err := m.tunnel.Read(packet)
		if err != nil {
			log.Println("failed to read from tunnel: ", err)
			m.tunnel.Close()
			return
		}

		if n < 4 {
			continue
		}

		if !m.IsAttached() {
			continue
		}

		var (
		// This seemingly is always set to 0 which is unhelpful
		//pktLength uint16 = binary.BigEndian.Uint16(packet[:2])
		//family    uint16 = binary.BigEndian.Uint16(packet[2:4])
		)

		//Remove the SSH added family address uint32 (for layer 3 tun)
		packet = packet[4:n]

	outer:
		for len(packet) > 0 {

			var totalLen uint16
			switch header.IPVersion(packet) {
			case header.IPv4Version:

				if len(packet) < header.IPv4MinimumSize {
					log.Println("ipv4 packet was smaller than the minimum size")

					break outer
				}

				totalLen = header.IPv4(packet).TotalLength()
				// extremely brittle, because we have to throw out an entire buffers worth of data if people give us fucky packets
				// unfortunately because ssh isnt giving us discrete packet sizes we have to deal with this
				if uint16(len(packet)) < totalLen {
					log.Println("invalid ipv4 packet, packet was smaller than reported length in packet header")

					break outer
				}

				pkb := stack.NewPacketBuffer(stack.PacketBufferOptions{
					Payload: buffer.MakeWithData(packet[:totalLen]),
				})

				m.dispatcher.DeliverNetworkPacket(header.IPv4ProtocolNumber, pkb)
			case header.IPv6Version:

				if len(packet) < header.IPv6MinimumSize {
					log.Println("ipv6 packet was smaller than the minimum size ditching")

					break outer
				}

				// not supporting multiple headers because fuck that noise
				totalLen = header.IPv6MinimumSize + header.IPv6(packet).PayloadLength()
				if uint16(len(packet)) < totalLen {
					log.Println("invalid ipv6 packet length, the total length was greater than the remaining packet length")

					break outer
				}

				pkb := stack.NewPacketBuffer(stack.PacketBufferOptions{
					Payload: buffer.MakeWithData(packet[:totalLen]),
				})

				m.dispatcher.DeliverNetworkPacket(header.IPv6ProtocolNumber, pkb)
			default:
				log.Println("recieved something that wasnt a ipv6 or ipv4 packet: family: ", header.IPVersion(packet), "len:", len(packet))
				break outer
			}

			packet = packet[min(totalLen+4, uint16(len(packet))):]
		}

	}
}

// IsAttached implements stack.LinkEndpoint.
func (m *SSHEndpoint) IsAttached() bool {
	return m.dispatcher != nil
}

// WritePackets writes outbound packets
func (m *SSHEndpoint) WritePackets(pkts stack.PacketBufferList) (int, tcpip.Error) {
	n := 0
	for _, pkt := range pkts.AsSlice() {
		if err := m.writePacket(pkt); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// WritePacket writes outbound packets
func (m *SSHEndpoint) writePacket(pkt *stack.PacketBuffer) tcpip.Error {

	pktBuf := pkt.ToView().AsSlice()

	//I have quite literally no idea why a lock here fixes ssh issues
	m.lock.Lock()
	defer m.lock.Unlock()

	// 3.2 Frame Format
	// https://git.kernel.org/pub/scm/linux/kernel/git/torvalds/linux.git/tree/Documentation/networking/tuntap.rst?id=HEAD
	packet := make([]byte, 4)
	binary.BigEndian.PutUint16(packet, 1)
	binary.BigEndian.PutUint16(packet[2:], uint16(pkt.NetworkProtocolNumber))

	packet = append(packet, pktBuf...)

	if _, err := m.tunnel.Write(packet); err != nil {
		log.Println("failed to write packet to tunnel: ", err)
		return &tcpip.ErrInvalidEndpointState{}
	}

	return nil
}

// Wait implements stack.LinkEndpoint.Wait.
func (m *SSHEndpoint) Wait() {}

// ARPHardwareType implements stack.LinkEndpoint.ARPHardwareType.
func (*SSHEndpoint) ARPHardwareType() header.ARPHardwareType {
	return header.ARPHardwareNone
}

// AddHeader implements stack.LinkEndpoint.AddHeader.
func (*SSHEndpoint) AddHeader(*stack.PacketBuffer) {
}

// WriteRawPacket implements stack.LinkEndpoint.
func (*SSHEndpoint) WriteRawPacket(*stack.PacketBuffer) tcpip.Error {
	return &tcpip.ErrNotSupported{}
}

func icmpResponder(s *stack.Stack) error {

	var wq waiter.Queue
	rawProto, rawerr := raw.NewEndpoint(s, ipv4.ProtocolNumber, icmp.ProtocolNumber4, &wq)
	if rawerr != nil {
		return errors.New("could not create raw endpoint")
	}
	if err := rawProto.Bind(tcpip.FullAddress{}); err != nil {
		return errors.New("could not bind raw endpoint")
	}
	go func() {
		we, ch := waiter.NewChannelEntry(waiter.ReadableEvents)
		wq.EventRegister(&we)
		for {
			var buff bytes.Buffer
			_, err := rawProto.Read(&buff, tcpip.ReadOptions{})
			if _, ok := err.(*tcpip.ErrWouldBlock); ok {
				// Wait for data to become available.

				for range ch {

					_, err := rawProto.Read(&buff, tcpip.ReadOptions{})

					if err != nil {

						continue
					}

					iph := header.IPv4(buff.Bytes())

					hlen := int(iph.HeaderLength())
					if buff.Len() < hlen {
						return
					}

					// Reconstruct a ICMP PacketBuffer from bytes.
					view := buffer.MakeWithData(buff.Bytes())
					packetbuff := stack.NewPacketBuffer(stack.PacketBufferOptions{
						Payload:            view,
						ReserveHeaderBytes: hlen,
					})

					packetbuff.NetworkProtocolNumber = ipv4.ProtocolNumber
					packetbuff.TransportProtocolNumber = icmp.ProtocolNumber4
					packetbuff.NetworkHeader().Consume(hlen)

					go func() {
						if TryResolve(iph.DestinationAddress().String()) {
							ProcessICMP(s, packetbuff)
						}
					}()
				}
			}

		}
	}()
	return nil
}

// ProcessICMP send back a ICMP echo reply from after receiving a echo request.
// This code come mostly from pkg/tcpip/network/ipv4/icmp.go
func ProcessICMP(nstack *stack.Stack, pkt *stack.PacketBuffer) {
	// (gvisor) pkg/tcpip/network/ipv4/icmp.go:174 - handleICMP
	defer pkt.DecRef()

	h := header.ICMPv4(pkt.TransportHeader().Slice())
	if len(h) < header.ICMPv4MinimumSize {
		return
	}

	// Only do in-stack processing if the checksum is correct.
	if checksum.Checksum(h, pkt.Data().Checksum()) != 0xffff {
		return
	}

	iph := header.IPv4(pkt.NetworkHeader().Slice())
	var newOptions header.IPv4Options

	// TODO(b/112892170): Meaningfully handle all ICMP types.
	switch h.Type() {
	case header.ICMPv4Echo:

		replyData := stack.PayloadSince(pkt.TransportHeader())
		defer replyData.Release()

		ipHdr := header.IPv4(pkt.NetworkHeader().Slice())
		localAddressBroadcast := pkt.NetworkPacketInfo.LocalAddressBroadcast

		// Take the base of the incoming request IP header but replace the options.

		pkt = nil

		// As per RFC 1122 section 3.2.1.3, when a host sends any datagram, the IP
		// source address MUST be one of its own IP addresses (but not a broadcast
		// or multicast address).
		localAddr := ipHdr.DestinationAddress()
		if localAddressBroadcast || header.IsV4MulticastAddress(localAddr) {
			localAddr = tcpip.Address{}
		}

		r, err := nstack.FindRoute(1, localAddr, ipHdr.SourceAddress(), ipv4.ProtocolNumber, false /* multicastLoop */)
		if err != nil {
			// If we cannot find a route to the destination, silently drop the packet.
			return
		}
		defer r.Release()

		replyHeaderLength := uint8(header.IPv4MinimumSize + len(newOptions))
		replyIPHdrView := buffer.NewView(int(replyHeaderLength))
		replyIPHdrView.Write(iph[:header.IPv4MinimumSize])
		replyIPHdrView.Write(newOptions)
		replyIPHdr := header.IPv4(replyIPHdrView.AsSlice())
		replyIPHdr.SetHeaderLength(replyHeaderLength)
		replyIPHdr.SetSourceAddress(r.LocalAddress())
		replyIPHdr.SetDestinationAddress(r.RemoteAddress())
		replyIPHdr.SetTTL(r.DefaultTTL())
		replyIPHdr.SetTotalLength(uint16(len(replyIPHdr) + len(replyData.AsSlice())))
		replyIPHdr.SetChecksum(0)
		replyIPHdr.SetChecksum(^replyIPHdr.CalculateChecksum())

		replyICMPHdr := header.ICMPv4(replyData.AsSlice())
		replyICMPHdr.SetType(header.ICMPv4EchoReply)
		replyICMPHdr.SetChecksum(0)
		replyICMPHdr.SetChecksum(^checksum.Checksum(replyData.AsSlice(), 0))

		replyBuf := buffer.MakeWithView(replyIPHdrView)
		replyBuf.Append(replyData.Clone())
		replyPkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
			ReserveHeaderBytes: int(r.MaxHeaderLength()),
			Payload:            replyBuf,
		})
		defer replyPkt.DecRef()

		// Populate the network/transport headers in the packet buffer so the
		// ICMP packet goes through IPTables.
		if ok := parse.IPv4(replyPkt); !ok {
			panic("expected to parse IPv4 header we just created")
		}
		if ok := parse.ICMPv4(replyPkt); !ok {
			panic("expected to parse ICMPv4 header we just created")
		}

		replyPkt.TransportProtocolNumber = header.ICMPv4ProtocolNumber
		if err := r.WriteHeaderIncludedPacket(replyPkt); err != nil {
			return
		}
	}
}

// TryResolve tries to discover if the remote host is up using ICMP
func TryResolve(address string) bool {
	methods := []func(string) (bool, error){
		RawPinger,
		CommandPinger,
	}
	for _, method := range methods {
		if result, err := method(address); err == nil {
			return result
		}
	}
	// Everything failed...
	return false
}

// RawPinger use ICMP sockets to discover if a host is up. This could require administrative permissions on some hosts
func RawPinger(target string) (bool, error) {
	pinger, err := ping.NewPinger(target)
	if err != nil {
		return false, err
	}
	pinger.Count = 1
	pinger.Timeout = 4 * time.Second // NMAP default timeout ?
	if runtime.GOOS == "windows" {
		pinger.SetPrivileged(true)
	}
	err = pinger.Run()
	if err != nil {
		return false, err
	}

	return pinger.PacketsRecv != 0, nil
}

// CommandPinger uses the internal ping command (dirty), but should not require privileges
func CommandPinger(target string) (bool, error) {
	countArg := "-c"
	waitArg := "-W"
	waitTime := "3"
	if runtime.GOOS == "windows" {
		countArg = "/n"
		waitArg = "/w"
		waitTime = "3000"
	}

	cmd := exec.Command("ping", countArg, "1", waitArg, waitTime, target)
	if err := cmd.Run(); err != nil {
		return false, err
	}
	return true, nil
}

// Modified version of https://github.com/moby/moby/blob/master/cmd/docker-proxy/udp_proxy.go and
// https://github.com/moby/vpnkit/blob/master/go/pkg/libproxy/udp_proxy.go

const (
	// UDPConnTrackTimeout is the timeout used for UDP connection tracking
	UDPConnTrackTimeout = 90 * time.Second
	// UDPBufSize is the buffer size for the UDP proxy
	UDPBufSize = 65507
)

// A net.Addr where the IP is split into two fields so you can use it as a key
// in a map:
type connTrackKey struct {
	IPHigh uint64
	IPLow  uint64
	Port   int
}

func newConnTrackKey(addr *net.UDPAddr) *connTrackKey {
	if len(addr.IP) == net.IPv4len {
		return &connTrackKey{
			IPHigh: 0,
			IPLow:  uint64(binary.BigEndian.Uint32(addr.IP)),
			Port:   addr.Port,
		}
	}
	return &connTrackKey{
		IPHigh: binary.BigEndian.Uint64(addr.IP[:8]),
		IPLow:  binary.BigEndian.Uint64(addr.IP[8:]),
		Port:   addr.Port,
	}
}

type connTrackMap map[connTrackKey]net.Conn

// UDPProxy is proxy for which handles UDP datagrams. It implements the Proxy
// interface to handle UDP traffic forwarding between the frontend and backend
// addresses.
type UDPProxy struct {
	listener       udpConn
	dialer         func() (net.Conn, error)
	connTrackTable connTrackMap
	connTrackLock  sync.Mutex
}

// NewUDPProxy creates a new UDPProxy.
func NewUDPProxy(listener udpConn, dialer func() (net.Conn, error)) (*UDPProxy, error) {
	return &UDPProxy{
		listener:       listener,
		connTrackTable: make(connTrackMap),
		dialer:         dialer,
	}, nil
}

func (proxy *UDPProxy) replyLoop(proxyConn net.Conn, clientAddr net.Addr, clientKey *connTrackKey) {
	defer func() {
		proxy.connTrackLock.Lock()
		delete(proxy.connTrackTable, *clientKey)
		proxy.connTrackLock.Unlock()
		proxyConn.Close()
	}()

	readBuf := make([]byte, UDPBufSize)
	for {
		_ = proxyConn.SetReadDeadline(time.Now().Add(UDPConnTrackTimeout))
	again:
		read, err := proxyConn.Read(readBuf)
		if err != nil {
			if err, ok := err.(*net.OpError); ok && err.Err == syscall.ECONNREFUSED {
				// This will happen if the last write failed
				// (e.g: nothing is actually listening on the
				// proxied port on the container), ignore it
				// and continue until UDPConnTrackTimeout
				// expires:
				goto again
			}
			return
		}
		for i := 0; i != read; {
			written, err := proxy.listener.WriteTo(readBuf[i:read], clientAddr)
			if err != nil {
				return
			}
			i += written
		}
	}
}

// Run starts forwarding the traffic using UDP.
func (proxy *UDPProxy) Run() {
	readBuf := make([]byte, UDPBufSize)
	for {
		read, from, err := proxy.listener.ReadFrom(readBuf)
		if err != nil {
			// NOTE: Apparently ReadFrom doesn't return
			// ECONNREFUSED like Read do (see comment in
			// UDPProxy.replyLoop)
			if !isClosedError(err) {
				log.Printf("Stopping udp proxy (%s)", err)
			}
			break
		}

		fromKey := newConnTrackKey(from.(*net.UDPAddr))
		proxy.connTrackLock.Lock()
		proxyConn, hit := proxy.connTrackTable[*fromKey]
		if !hit {
			proxyConn, err = proxy.dialer()
			if err != nil {
				log.Printf("Can't proxy a datagram to udp: %s\n", err)
				proxy.connTrackLock.Unlock()
				continue
			}
			proxy.connTrackTable[*fromKey] = proxyConn
			go proxy.replyLoop(proxyConn, from, fromKey)
		}
		proxy.connTrackLock.Unlock()
		for i := 0; i != read; {
			_ = proxyConn.SetReadDeadline(time.Now().Add(UDPConnTrackTimeout))
			written, err := proxyConn.Write(readBuf[i:read])
			if err != nil {
				log.Printf("Can't proxy a datagram to udp: %s\n", err)
				break
			}
			i += written
		}
	}
}

// Close stops forwarding the traffic.
func (proxy *UDPProxy) Close() error {
	proxy.listener.Close()
	proxy.connTrackLock.Lock()
	defer proxy.connTrackLock.Unlock()
	for _, conn := range proxy.connTrackTable {
		conn.Close()
	}
	return nil
}

func isClosedError(err error) bool {
	/* This comparison is ugly, but unfortunately, net.go doesn't export errClosing.
	 * See:
	 * http://golang.org/src/pkg/net/net.go
	 * https://code.google.com/p/go/issues/detail?id=4337
	 * https://groups.google.com/forum/#!msg/golang-nuts/0_aaCvBmOcM/SptmDyX1XJMJ
	 */
	return strings.HasSuffix(err.Error(), "use of closed network connection")
}

type udpConn interface {
	ReadFrom(b []byte) (int, net.Addr, error)
	WriteTo(b []byte, addr net.Addr) (int, error)
	SetReadDeadline(t time.Time) error
	io.Closer
}

type autoStoppingListener struct {
	underlying udpConn
}

func (l *autoStoppingListener) ReadFrom(b []byte) (int, net.Addr, error) {
	_ = l.underlying.SetReadDeadline(time.Now().Add(UDPConnTrackTimeout))
	return l.underlying.ReadFrom(b)
}

func (l *autoStoppingListener) WriteTo(b []byte, addr net.Addr) (int, error) {
	_ = l.underlying.SetReadDeadline(time.Now().Add(UDPConnTrackTimeout))
	return l.underlying.WriteTo(b, addr)
}

func (l *autoStoppingListener) SetReadDeadline(t time.Time) error {
	return l.underlying.SetReadDeadline(t)
}

func (l *autoStoppingListener) Close() error {
	return l.underlying.Close()
}
