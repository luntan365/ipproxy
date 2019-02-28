package ipproxy

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/google/netstack/tcpip"
	"github.com/google/netstack/tcpip/buffer"
	"github.com/google/netstack/tcpip/network/ipv4"
	"github.com/google/netstack/tcpip/transport/tcp"

	"github.com/getlantern/errors"
)

func (p *proxy) onTCP(pkt ipPacket) {
	dstAddr := pkt.ft().dst
	p.tcpConnTrackMx.Lock()
	dest := p.tcpConnTrack[dstAddr]
	p.tcpConnTrackMx.Unlock()
	if dest == nil {
		var err error
		dest, err = p.startTCPDest(dstAddr)
		if err != nil {
			log.Error(err)
			return
		}
		p.tcpConnTrackMx.Lock()
		p.tcpConnTrack[dstAddr] = dest
		p.tcpConnTrackMx.Unlock()
	}

	p.channelEndpoint.Inject(ipv4.ProtocolNumber, buffer.View(pkt.raw).ToVectorisedView())
}

func (p *proxy) startTCPDest(dstAddr addr) (*tcpDest, error) {
	nicID := p.nextNICID()
	if err := p.stack.CreateNIC(nicID, p.linkID); err != nil {
		return nil, errors.New("Unable to create TCP NIC: %v", err)
	}
	ipAddr := tcpip.Address(net.ParseIP(dstAddr.ip).To4())
	if err := p.stack.AddAddress(nicID, p.proto, ipAddr); err != nil {
		return nil, errors.New("Unable to add IP addr for TCP dest: %v", err)
	}

	dest := &tcpDest{
		baseConn: newBaseConn(p, nil, nil),
		addr:     dstAddr.String(),
		conns:    make(map[tcpip.FullAddress]*baseConn),
	}
	dest.markActive()

	if err := dest.init(tcp.ProtocolNumber, tcpip.FullAddress{nicID, ipAddr, dstAddr.port}); err != nil {
		return nil, errors.New("Unable to initialize TCP dest: %v", err)
	}

	if err := dest.ep.Listen(p.opts.TCPConnectBacklog); err != nil {
		dest.finalize()
		return nil, errors.New("Unable to listen for TCP connections: %v", err)
	}

	go dest.accept()
	return dest, nil
}

type tcpDest struct {
	baseConn
	addr    string
	conns   map[tcpip.FullAddress]*baseConn
	connsMx sync.Mutex
}

func (dest *tcpDest) accept() {
	for {
		acceptedEp, wq, err := dest.ep.Accept()
		if err != nil {
			if err == tcpip.ErrWouldBlock {
				<-dest.notifyCh
				continue
			}
			log.Errorf("Accept() failed: %v", err)
			return
		}

		upstream, dialErr := dest.p.opts.DialTCP(context.Background(), "tcp", dest.addr)
		if dialErr != nil {
			log.Errorf("Unexpected error dialing upstream to %v: %v", dest.addr, err)
			return
		}

		downstreamAddr, _ := acceptedEp.GetRemoteAddress()
		tcpConn := newBaseConnWithQueue(dest.p, upstream, wq, func() error {
			dest.removeConn(downstreamAddr)
			return nil
		})
		tcpConn.ep = acceptedEp
		go tcpConn.copyToUpstream(nil)
		go tcpConn.copyFromUpstream(tcpip.WriteOptions{})
		dest.connsMx.Lock()
		dest.conns[downstreamAddr] = &tcpConn
		dest.connsMx.Unlock()
	}
}

func (dest *tcpDest) removeConn(addr tcpip.FullAddress) {
	dest.connsMx.Lock()
	delete(dest.conns, addr)
	dest.connsMx.Unlock()
}

func (dest *tcpDest) numConns() int {
	dest.connsMx.Lock()
	numConns := len(dest.conns)
	dest.connsMx.Unlock()
	return numConns
}

// reapUDP reaps idled TCP connections and destinations. We do this on a single
// goroutine to avoid creating a bunch of timers for each connection
// (which is expensive).
func (p *proxy) reapTCP() {
	for {
		time.Sleep(1 * time.Second)
		p.tcpConnTrackMx.Lock()
		dests := make(map[addr]*tcpDest, len(p.tcpConnTrack))
		for a, dest := range p.tcpConnTrack {
			dests[a] = dest
		}
		p.tcpConnTrackMx.Unlock()
		for a, dest := range dests {
			dest.connsMx.Lock()
			conns := make([]*baseConn, 0, len(dest.conns))
			for _, conn := range dest.conns {
				conns = append(conns, conn)
			}
			dest.connsMx.Unlock()
			if len(conns) > 0 {
				for _, conn := range dest.conns {
					if conn.timeSinceLastActive() > p.opts.IdleTimeout {
						go conn.Close()
					}
				}
			} else if dest.timeSinceLastActive() > p.opts.IdleTimeout {
				p.tcpConnTrackMx.Lock()
				delete(p.tcpConnTrack, a)
				p.tcpConnTrackMx.Unlock()
				dest.Close()
			}
		}
	}
}
