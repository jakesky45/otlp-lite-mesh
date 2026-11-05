// Package udp is Profile Alpha of SPEC.md §5.1: frames as UDP datagrams,
// one frame per datagram, sent to a multicast group (or a plain address)
// with no handshake and no acknowledgement.
//
// The package moves bytes and nothing else. It does not decode frames, open
// seals or know about OTLP. A Receiver hands every datagram it gets to a
// handler function; what the handler does is the gateway's business. The one
// rule it enforces is size: a datagram larger than the configured maximum is
// dropped before anyone looks at it (errata E2, one layer down).
//
// LEARN: this is the first package with concurrency. A Receiver's Run loop
// blocks reading from the socket; to stop it we need a second goroutine that
// waits for the context to be cancelled and then closes the socket, which
// makes the blocked read return. That "close to unblock" pattern is how Go
// network code is usually stopped.
package udp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"sync/atomic"

	"github.com/jakesky45/otlp-lite-mesh/pkg/frame"
)

// Defaults from the spec and errata.
const (
	// DefaultGroup is the multicast group and port. 239.0.0.0/8 is the
	// "administratively scoped" range: routers do not forward it beyond the
	// local network, which is what a mesh wants. 4319 continues OTLP's 4317
	// and 4318, but is not IANA-registered (errata E4).
	DefaultGroup = "239.42.0.1:4319"

	// DefaultMaxFrame is the largest datagram a Receiver accepts: a header
	// plus the default maximum payload, 1280 bytes, the IPv6 minimum MTU.
	DefaultMaxFrame = frame.HeaderSize + frame.DefaultMaxPayload
)

// ErrTooLarge is returned by Send for a frame over the maximum.
var ErrTooLarge = errors.New("udp: frame larger than the maximum datagram")

// Option configures a Sender or Receiver.
//
// LEARN: the "functional options" idiom. Instead of a config struct with many
// fields or a constructor with many parameters, the constructor takes a
// variadic list of functions that each tweak the settings. Callers write
// udp.NewReceiver(addr, udp.WithMaxFrame(300)) and omit what they don't care
// about. It reads well and new options never break existing callers.
type Option func(*settings)

type settings struct {
	maxFrame int
	ttl      int
	iface    *net.Interface
}

func defaults() settings {
	return settings{maxFrame: DefaultMaxFrame, ttl: 1}
}

// WithMaxFrame sets the largest datagram sent or accepted, in bytes.
func WithMaxFrame(n int) Option { return func(s *settings) { s.maxFrame = n } }

// WithTTL sets the multicast time-to-live: how many routers a datagram may
// cross. 1 (the default) keeps it on the local link.
func WithTTL(ttl int) Option { return func(s *settings) { s.ttl = ttl } }

// WithInterface pins multicast to one network interface, for machines with
// several. nil lets the OS choose.
func WithInterface(ifi *net.Interface) Option { return func(s *settings) { s.iface = ifi } }

// Sender sends frames to one address.
type Sender struct {
	conn     *net.UDPConn
	maxFrame int
	sent     atomic.Uint64
}

// NewSender opens a UDP socket aimed at addr ("239.42.0.1:4319" or any
// "host:port"). Nothing is transmitted until Send; UDP has no connection.
func NewSender(addr string, opts ...Option) (*Sender, error) {
	st := defaults()
	for _, o := range opts {
		o(&st)
	}
	dst, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("udp: resolve %q: %w", addr, err)
	}
	// LEARN: "Dial" on UDP does not connect to anything; it just fixes the
	// destination so later Writes don't have to repeat it, and lets the OS
	// pick the local port.
	conn, err := net.DialUDP("udp", nil, dst)
	if err != nil {
		return nil, fmt.Errorf("udp: dial %q: %w", addr, err)
	}
	if dst.IP.IsMulticast() {
		if err := setMulticastTTL(conn, st.ttl); err != nil {
			conn.Close()
			return nil, err
		}
	}
	return &Sender{conn: conn, maxFrame: st.maxFrame}, nil
}

// Send transmits one frame as one datagram. It does not wait for anything:
// UDP gives no acknowledgement, so a nil error means "handed to the OS", not
// "arrived".
func (s *Sender) Send(raw []byte) error {
	if len(raw) > s.maxFrame {
		return fmt.Errorf("%w: %d > %d bytes", ErrTooLarge, len(raw), s.maxFrame)
	}
	if _, err := s.conn.Write(raw); err != nil {
		return fmt.Errorf("udp: send: %w", err)
	}
	s.sent.Add(1)
	return nil
}

// Sent reports how many frames have been handed to the OS.
func (s *Sender) Sent() uint64 { return s.sent.Load() }

// LocalAddr is the socket's own address, useful in tests.
func (s *Sender) LocalAddr() net.Addr { return s.conn.LocalAddr() }

// Close releases the socket.
func (s *Sender) Close() error { return s.conn.Close() }

// Handler receives one datagram. raw is a fresh copy the handler may keep.
// from is the sender's address, for logging; it is not authenticated.
type Handler func(raw []byte, from *net.UDPAddr)

// Stats counts what a Receiver has seen. Oversized datagrams are dropped
// before the handler sees them.
type Stats struct {
	Received  uint64 // datagrams handed to the handler
	Oversized uint64 // datagrams dropped for exceeding the maximum
}

// Receiver listens on one address and delivers datagrams to a Handler.
type Receiver struct {
	conn      *net.UDPConn
	maxFrame  int
	received  atomic.Uint64
	oversized atomic.Uint64
}

// NewReceiver listens on addr. A multicast address joins the group; any
// other address, such as "127.0.0.1:0", is a plain UDP listener (port 0 asks
// the OS for a free one; read it back with LocalAddr).
func NewReceiver(addr string, opts ...Option) (*Receiver, error) {
	st := defaults()
	for _, o := range opts {
		o(&st)
	}
	laddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("udp: resolve %q: %w", addr, err)
	}
	var conn *net.UDPConn
	if laddr.IP.IsMulticast() {
		// Joins the group on the interface (nil = OS default) and sets
		// SO_REUSEADDR so several receivers on one machine can share the port.
		conn, err = net.ListenMulticastUDP("udp", st.iface, laddr)
	} else {
		conn, err = net.ListenUDP("udp", laddr)
	}
	if err != nil {
		return nil, fmt.Errorf("udp: listen %q: %w", addr, err)
	}
	// A bigger kernel buffer absorbs bursts while the handler is busy.
	_ = conn.SetReadBuffer(1 << 20)
	return &Receiver{conn: conn, maxFrame: st.maxFrame}, nil
}

// LocalAddr is the address actually bound, including an OS-chosen port.
func (r *Receiver) LocalAddr() net.Addr { return r.conn.LocalAddr() }

// Stats returns the counters so far.
func (r *Receiver) Stats() Stats {
	return Stats{Received: r.received.Load(), Oversized: r.oversized.Load()}
}

// Run reads datagrams and calls handle for each until ctx is cancelled, then
// returns nil. The handler runs on Run's goroutine, so a slow handler slows
// reception; the gateway keeps it short and does the real work elsewhere.
func (r *Receiver) Run(ctx context.Context, handle Handler) error {
	// LEARN: a goroutine is a lightweight thread started with the "go"
	// keyword. This one does one thing: waits for the context to be cancelled
	// (<-ctx.Done() blocks until then) and closes the socket. Closing makes
	// the ReadFromUDP below return an error, which is how the loop ends.
	go func() {
		<-ctx.Done()
		r.conn.Close()
	}()

	// One byte larger than the maximum, so an oversized datagram can be told
	// apart from one that is exactly the maximum. UDP truncates silently.
	buf := make([]byte, r.maxFrame+1)
	for {
		n, from, err := r.conn.ReadFromUDP(buf)
		if err != nil {
			// LEARN: after Close, reads fail with net.ErrClosed. If the context
			// was cancelled that is the expected way out, not a failure.
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("udp: read: %w", err)
		}
		if n > r.maxFrame {
			r.oversized.Add(1)
			continue
		}
		r.received.Add(1)
		// LEARN: buf is reused on the next read, so the handler gets a copy.
		// Without this, a handler that kept raw would see it overwritten.
		handle(bytes.Clone(buf[:n]), from)
	}
}

// Close stops the receiver. Run returns shortly after.
func (r *Receiver) Close() error { return r.conn.Close() }
