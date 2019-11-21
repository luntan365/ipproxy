// Package ipproxy provides a facility for proxying IP traffic. Currently it
// only supports TCP and UDP on top of IPv4.
package ipproxy

import (
	"context"
	"io"
	"net"
	"sync"
	"time"

	"github.com/google/netstack/tcpip"
	"github.com/google/netstack/tcpip/buffer"
	"github.com/google/netstack/tcpip/link/channel"
	"github.com/google/netstack/tcpip/network/ipv4"
	"github.com/google/netstack/tcpip/stack"
	"github.com/google/netstack/tcpip/transport/tcp"
	"github.com/google/netstack/tcpip/transport/udp"

	"github.com/getlantern/errors"
	"github.com/getlantern/golog"
)

var (
	log = golog.LoggerFor("ipproxy")
)

const (
	DefaultMTU                 = 1500
	DefaultOutboundBufferDepth = 10000
	DefaultIdleTimeout         = 65 * time.Second
	DefaultTCPConnectBacklog   = 10
	DefaultStatsInterval       = 15 * time.Second

	IPProtocolICMP = 1
	IPProtocolTCP  = 6
	IPProtocolUDP  = 17
)

type Opts struct {
	// MTU in bytes. Default of 1500 is usually fine.
	MTU int

	// OutboundBufferDepth specifies the number of outbound packets to buffer.
	// The default is 1.
	OutboundBufferDepth int

	// IdleTimeout specifies the amount of time before idle connections are
	// automatically closed. The default is 65 seconds.
	IdleTimeout time.Duration

	// TCPConnectBacklog is the allows backlog of TCP connections to a given
	// upstream port. Defaults to 10.
	TCPConnectBacklog int

	// StatsInterval controls how frequently to display stats. Defaults to 15
	// seconds.
	StatsInterval time.Duration

	// DialTCP specifies a function for dialing upstream TCP connections. Defaults
	// to net.Dialer.DialContext().
	DialTCP func(ctx context.Context, network, addr string) (net.Conn, error)

	// DialUDP specifies a function for dialing upstream UDP connections. Defaults
	// to net.Dialer.DialContext().
	DialUDP func(ctx context.Context, network, addr string) (*net.UDPConn, error)
}

// ApplyDefaults applies the default values to the given Opts, including making
// a new Opts if opts is nil.
func (opts *Opts) ApplyDefaults() *Opts {
	if opts == nil {
		opts = &Opts{}
	}
	if opts.MTU <= 0 {
		opts.MTU = DefaultMTU
	}
	if opts.OutboundBufferDepth <= 0 {
		opts.OutboundBufferDepth = DefaultOutboundBufferDepth
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = DefaultIdleTimeout
	}
	if opts.TCPConnectBacklog <= 0 {
		opts.TCPConnectBacklog = DefaultTCPConnectBacklog
	}
	if opts.StatsInterval <= 0 {
		opts.StatsInterval = DefaultStatsInterval
	}
	if opts.DialTCP == nil {
		d := &net.Dialer{}
		opts.DialTCP = d.DialContext
	}
	if opts.DialUDP == nil {
		d := &net.Dialer{}
		opts.DialUDP = func(ctx context.Context, network, addr string) (*net.UDPConn, error) {
			conn, err := d.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return conn.(*net.UDPConn), nil
		}
	}
	return opts
}

type Proxy interface {
	// Serve starts proxying and blocks until finished
	Serve() error

	// Count of accepted packets
	AcceptedPackets() int

	// Count of rejected packets
	RejectedPackets() int

	// Number of TCP origins being tracked
	NumTCPOrigins() int

	// Number of TCP connections being tracked
	NumTCPConns() int

	// Number of UDP "connections" being tracked
	NumUDPConns() int

	// Close shuts down the proxy in an orderly fashion and blocks until shutdown
	// is complete.
	Close() error
}

type proxy struct {
	acceptedPackets int64
	rejectedPackets int64
	numTcpOrigins   int64
	numTcpConns     int64
	numUdpConns     int64

	opts       *Opts
	proto      tcpip.NetworkProtocolNumber
	downstream io.ReadWriter

	pktIn      chan ipPacket
	tcpOrigins map[addr]*tcpOrigin
	udpConns   map[fourtuple]*udpConn

	toDownstream chan channel.PacketInfo

	closeable
}

func (p *proxy) Serve() error {
	icmpStack, icmpEndpoint, err := p.stackForICMP()
	if err != nil {
		return err
	}

	go p.trackStats()
	go p.copyFromUpstream()
	var wg sync.WaitGroup
	wg.Add(1)
	go p.copyToUpstream(icmpStack, icmpEndpoint, &wg)
	return p.readDownstreamPackets(&wg)
}

func New(downstream io.ReadWriter, opts *Opts) (Proxy, error) {
	// Default options
	opts = opts.ApplyDefaults()

	p := &proxy{
		opts:         opts,
		proto:        ipv4.ProtocolNumber,
		downstream:   downstream,
		pktIn:        make(chan ipPacket, 1000),
		tcpOrigins:   make(map[addr]*tcpOrigin, 0),
		udpConns:     make(map[fourtuple]*udpConn, 0),
		toDownstream: make(chan channel.PacketInfo),
		closeable: closeable{
			closeCh:           make(chan struct{}),
			readyToFinalizeCh: make(chan struct{}),
			closedCh:          make(chan struct{}),
		},
	}

	p.finalizer = func() error {
		return nil
	}

	return p, nil
}

func (p *proxy) readDownstreamPackets(wg *sync.WaitGroup) (finalErr error) {
	defer p.closeNow()
	defer wg.Wait() // wait for copyToUpstream to finish with all of its cleanup

	for {
		// we can't reuse this byte slice across reads because each one is held in
		// memory by the tcpip stack.
		b := make([]byte, p.opts.MTU)
		n, err := p.downstream.Read(b)
		if err != nil {
			if err == io.EOF {
				return err
			}
			return errors.New("Unexpected error reading from downstream: %v", err)
		}
		raw := b[:n]
		pkt, err := parseIPPacket(raw)
		if err != nil {
			log.Debugf("Error on inbound packet, ignoring: %v", err)
			p.rejectedPacket()
			continue
		}

		p.pktIn <- pkt
	}
}

func (p *proxy) copyToUpstream(icmpStack *stack.Stack, icmpEndpoint *channel.Endpoint, wg *sync.WaitGroup) {
	defer wg.Done()
	defer p.closeNow()
	defer icmpStack.Close()
	defer p.closeTCP()
	defer p.closeUDP()

	reapTicker := time.NewTicker(1 * time.Second)
	defer reapTicker.Stop()

	for {
		select {
		case pkt := <-p.pktIn:
			switch pkt.ipProto {
			case IPProtocolTCP:
				p.acceptedPacket()
				p.onTCP(pkt)
			case IPProtocolUDP:
				p.acceptedPacket()
				p.onUDP(pkt)
			case IPProtocolICMP:
				p.acceptedPacket()
				icmpEndpoint.InjectInbound(p.proto, tcpip.PacketBuffer{Data: buffer.View(pkt.raw).ToVectorisedView()})
			default:
				p.rejectedPacket()
				log.Debugf("Unknown IP protocol, ignoring: %v", pkt.ipProto)
				continue
			}
		case <-reapTicker.C:
			p.reapTCP()
			p.reapUDP()
		case <-p.closeCh:
			return
		}
	}
}

func (p *proxy) copyFromUpstream() {
	defer p.Close()

	for {
		select {
		case <-p.closedCh:
			return
		case pktInfo := <-p.toDownstream:
			pkt := make([]byte, 0, p.opts.MTU)
			pkt = append(pkt, pktInfo.Pkt.Header.View()...)
			pkt = append(pkt, pktInfo.Pkt.Data.ToView()...)
			_, err := p.downstream.Write(pkt)
			if err != nil {
				log.Errorf("Unexpected error writing to downstream: %v", err)
				return
			}
		}
	}
}

func (p *proxy) stackForICMP() (*stack.Stack, *channel.Endpoint, error) {
	channelEndpoint := channel.New(p.opts.OutboundBufferDepth, uint32(p.opts.MTU), "")
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocol{ipv4.NewProtocol()},
		TransportProtocols: []stack.TransportProtocol{tcp.NewProtocol(), udp.NewProtocol()},
	})
	if err := s.CreateNIC(nicID, channelEndpoint); err != nil {
		s.Close()
		return nil, nil, errors.New("Unable to create ICMP NIC: %v", err)
	}
	if err := s.SetPromiscuousMode(nicID, true); err != nil {
		s.Close()
		return nil, nil, errors.New("Unable to set ICMP NIC to promiscious mode: %v", err)
	}
	go func() {
		for {
			select {
			case <-p.closedCh:
				return
			case pktInfo := <-channelEndpoint.C:
				select {
				case <-p.closedCh:
					return
				case p.toDownstream <- pktInfo:
				}
			}
		}
	}()
	return s, channelEndpoint, nil
}
