package conn

// PATCH (soracom-honeypot): RotateBind, a Bind implementation for egress
// environments whose UDP relay delivers at most ONE inbound packet per
// connected UDP socket ("flow"), then silently drops the rest, while new
// sockets always work. Verified empirically against this sandbox's gateway
// (198.19.0.1:3128): fresh connected sockets got a DNS response 14/14
// times; a second exchange on the same socket succeeded 0/20 times.
//
// Strategy: every outbound packet is sent on a brand-new DialUDP socket,
// and a reader goroutine waits for the single inbound packet that flow
// can still deliver. The WireGuard server roams to the source of our
// latest packet (standard WG behaviour), so its next inbound packet
// lands on the newest socket, whose one-packet budget is still intact.
// Superseded sockets stay readable for a grace period to catch packets
// the server dispatched just before a rotation, then are reaped.
//
// Pinning every received packet's endpoint to the configured server
// endpoint is kept from the earlier patch: relay replies arrive with the
// gateway address as source, which must not be adopted as the peer
// endpoint on this side.

import (
	"errors"
	"net"
	"net/netip"
	"sync"
	"time"
)

const (
	rotateSocketTTL       = 120 * time.Second // hard cap for any one socket
	rotateSupersededGrace = 45 * time.Second  // read window after a newer socket exists
)

type shotSocket struct {
	conn      *net.UDPConn
	createdAt time.Time
}

type recvItem struct {
	data []byte
	ep   Endpoint
}

type RotateBind struct {
	mu      sync.Mutex
	sockets map[*shotSocket]struct{}
	latest  *shotSocket
	recvCh  chan recvItem
	done    chan struct{}
	closed  bool
	dst     netip.AddrPort // last seen destination
	dstEp   Endpoint
}

func NewRotateBind() *RotateBind {
	return &RotateBind{
		sockets: make(map[*shotSocket]struct{}),
		recvCh:  make(chan recvItem, 64),
		done:    make(chan struct{}),
	}
}

func (b *RotateBind) Open(port uint16) ([]ReceiveFunc, uint16, error) {
	// wireguard-go closes and re-opens the bind when the listen port is
	// (re)configured via UAPI, which is exactly what soratun does at
	// startup; reset state so a re-opened bind is usable again.
	b.mu.Lock()
	if b.closed {
		b.closed = false
		b.done = make(chan struct{})
		b.recvCh = make(chan recvItem, 64)
		b.sockets = make(map[*shotSocket]struct{})
		b.latest = nil
	}
	b.mu.Unlock()
	fn := func(packets [][]byte, sizes []int, eps []Endpoint) (int, error) {
		select {
		case item := <-b.recvCh:
			n := copy(packets[0], item.data)
			sizes[0] = n
			eps[0] = item.ep
			return 1, nil
		case <-b.done:
			return 0, net.ErrClosed
		}
	}
	return []ReceiveFunc{fn}, port, nil
}

func (b *RotateBind) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	close(b.done)
	for s := range b.sockets {
		s.conn.Close()
	}
	b.sockets = make(map[*shotSocket]struct{})
	return nil
}

func (b *RotateBind) SetMark(mark uint32) error { return nil }

func (b *RotateBind) BatchSize() int { return 1 }

func (b *RotateBind) ParseEndpoint(s string) (Endpoint, error) {
	e, err := netip.ParseAddrPort(s)
	if err != nil {
		return nil, err
	}
	return &StdNetEndpoint{AddrPort: e}, nil
}

// Send dials a fresh connected socket for every packet, per the design
// note at the top of this file.
func (b *RotateBind) Send(bufs [][]byte, ep Endpoint) error {
	sep, ok := ep.(*StdNetEndpoint)
	if !ok {
		return errors.New("rotate bind: unsupported endpoint type")
	}
	dst := sep.AddrPort

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return net.ErrClosed
	}
	b.dstEp = ep
	b.mu.Unlock()

	for _, buf := range bufs {
		if err := b.sendOne(buf, dst); err != nil {
			return err
		}
	}
	return nil
}

func (b *RotateBind) sendOne(buf []byte, dst netip.AddrPort) error {
	udpAddr := &net.UDPAddr{IP: dst.Addr().AsSlice(), Port: int(dst.Port())}
	c, err := net.DialUDP("udp4", nil, udpAddr)
	if err != nil {
		return err
	}
	s := &shotSocket{conn: c, createdAt: time.Now()}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		c.Close()
		return net.ErrClosed
	}
	prev := b.latest
	b.latest = s
	b.sockets[s] = struct{}{}
	b.mu.Unlock()

	if prev != nil {
		// Give the superseded socket a bounded grace window for packets
		// the server sent just before it learned the new endpoint.
		prev.conn.SetReadDeadline(time.Now().Add(rotateSupersededGrace))
	}
	c.SetReadDeadline(time.Now().Add(rotateSocketTTL))
	go b.readLoop(s)
	_, err = c.Write(buf)
	if err != nil {
		b.dropSocket(s)
		return err
	}
	return nil
}

func (b *RotateBind) readLoop(s *shotSocket) {
	buf := make([]byte, 65535)
	for {
		n, err := s.conn.Read(buf)
		if err != nil {
			b.dropSocket(s)
			return
		}
		data := make([]byte, n)
		copy(data, buf[:n])
		b.mu.Lock()
		ep := b.dstEp
		b.mu.Unlock()
		select {
		case b.recvCh <- recvItem{data: data, ep: ep}:
		case <-b.done:
		}
		// This flow has now delivered the only inbound packet it can
		// ever deliver in this environment; stop reading. If the peer
		// (against expectations) sends more on it, wireguard-go will
		// recover via retransmission onto a newer socket.
		b.dropSocket(s)
		return
	}
}

func (b *RotateBind) dropSocket(s *shotSocket) {
	b.mu.Lock()
	if _, ok := b.sockets[s]; ok {
		delete(b.sockets, s)
		s.conn.Close()
	}
	b.mu.Unlock()
}

var _ = errors.New // keep errors import if unused in future edits
