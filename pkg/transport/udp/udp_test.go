package udp_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/jakesky45/otlp-lite-mesh/pkg/frame"
	"github.com/jakesky45/otlp-lite-mesh/pkg/payload"
	"github.com/jakesky45/otlp-lite-mesh/pkg/prune"
	"github.com/jakesky45/otlp-lite-mesh/pkg/seal"
	"github.com/jakesky45/otlp-lite-mesh/pkg/transport/udp"
)

var testNode = frame.NodeID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}

// startReceiver listens on an OS-chosen loopback port and runs it in the
// background, returning the receiver and a channel the handler feeds.
//
// LEARN: a channel is a typed pipe between goroutines. The handler (on the
// receiver's goroutine) sends each datagram into it; the test (on its own
// goroutine) receives from it. The channel does the synchronisation, no
// locks needed. Buffered (capacity 256) so the handler never blocks.
func startReceiver(t *testing.T, opts ...udp.Option) (*udp.Receiver, <-chan []byte) {
	t.Helper()
	rx, err := udp.NewReceiver("127.0.0.1:0", opts...)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan []byte, 256)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- rx.Run(ctx, func(raw []byte, _ *net.UDPAddr) { got <- raw })
	}()

	// LEARN: t.Cleanup registers a function to run when the test ends, even
	// if it fails. Here it stops the receiver and checks Run exited cleanly.
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run returned %v after cancel", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("Run did not return within 2s of cancel")
		}
	})
	return rx, got
}

// recv waits for one datagram or fails the test after a timeout.
//
// LEARN: select waits on several channel operations at once and takes
// whichever is ready first. Pairing a receive with time.After is the
// standard "wait, but not forever" shape.
func recv(t *testing.T, ch <-chan []byte) []byte {
	t.Helper()
	select {
	case b := <-ch:
		return b
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a datagram")
		return nil
	}
}

func TestSendReceiveLoopback(t *testing.T) {
	rx, got := startReceiver(t)
	tx, err := udp.NewSender(rx.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Close()

	want, _ := frame.Encode(frame.Header{DataType: frame.DataTypeSpan, NodeID: testNode}, []byte("over the wire"))
	if err := tx.Send(want); err != nil {
		t.Fatal(err)
	}
	if got := recv(t, got); !bytes.Equal(got, want) {
		t.Errorf("received %x, want %x", got, want)
	}
	if tx.Sent() != 1 || rx.Stats().Received != 1 {
		t.Errorf("counters: sent %d, received %d", tx.Sent(), rx.Stats().Received)
	}
}

// TestHundredFrames is the Stage 6 acceptance test from the plan: 100 in,
// 100 out, on loopback. UDP may reorder, so we check the set, not the order.
func TestHundredFrames(t *testing.T) {
	rx, got := startReceiver(t)
	tx, err := udp.NewSender(rx.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Close()

	const n = 100
	seen := make(map[byte]bool)
	for i := 0; i < n; i++ {
		raw, _ := frame.Encode(frame.Header{DataType: frame.DataTypeMetric, NodeID: testNode}, []byte{byte(i)})
		if err := tx.Send(raw); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < n; i++ {
		raw := recv(t, got)
		fr, err := frame.Decode(raw)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		seen[fr.Payload[0]] = true
	}
	if len(seen) != n {
		t.Errorf("received %d distinct frames, want %d", len(seen), n)
	}
}

func TestOversizedDatagramDropped(t *testing.T) {
	// Receiver accepts at most 100 bytes; sender is allowed to send bigger
	// so we can see the receiver's own guard fire.
	rx, got := startReceiver(t, udp.WithMaxFrame(100))
	tx, err := udp.NewSender(rx.LocalAddr().String(), udp.WithMaxFrame(2000))
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Close()

	big := make([]byte, 101)
	ok := make([]byte, 100)
	if err := tx.Send(big); err != nil {
		t.Fatal(err)
	}
	if err := tx.Send(ok); err != nil {
		t.Fatal(err)
	}
	// Only the 100-byte one should arrive.
	if raw := recv(t, got); len(raw) != 100 {
		t.Errorf("got %d-byte datagram, want the 100-byte one", len(raw))
	}
	// Give the dropped one a moment to be counted, then check nothing else came.
	select {
	case raw := <-got:
		t.Errorf("unexpected extra datagram of %d bytes", len(raw))
	case <-time.After(200 * time.Millisecond):
	}
	st := rx.Stats()
	if st.Oversized != 1 || st.Received != 1 {
		t.Errorf("stats = %+v, want 1 oversized, 1 received", st)
	}
}

func TestSenderRefusesOversized(t *testing.T) {
	tx, err := udp.NewSender("127.0.0.1:1") // never written to
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Close()
	err = tx.Send(make([]byte, udp.DefaultMaxFrame+1))
	if !errors.Is(err, udp.ErrTooLarge) {
		t.Errorf("error = %v, want ErrTooLarge", err)
	}
	if tx.Sent() != 0 {
		t.Error("refused frame was counted as sent")
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	rx, err := udp.NewReceiver("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- rx.Run(ctx, func([]byte, *net.UDPAddr) {}) }()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v, want nil after cancel", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after cancel")
	}
}

// TestMulticastJoin tries the real multicast path. Some machines and CI
// runners have no multicast-capable interface, so a join failure skips
// rather than fails.
func TestMulticastJoin(t *testing.T) {
	const group = "239.42.0.1:43190" // a test port, not the real 4319
	rx, err := udp.NewReceiver(group)
	if err != nil {
		t.Skipf("multicast not available here: %v", err)
	}
	got := make(chan []byte, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rx.Run(ctx, func(raw []byte, _ *net.UDPAddr) { got <- raw })

	tx, err := udp.NewSender(group)
	if err != nil {
		t.Skipf("multicast send not available here: %v", err)
	}
	defer tx.Close()
	want, _ := frame.Encode(frame.Header{DataType: frame.DataTypeLog, NodeID: testNode}, []byte("mcast"))
	if err := tx.Send(want); err != nil {
		t.Skipf("multicast send failed here: %v", err)
	}
	select {
	case raw := <-got:
		if !bytes.Equal(raw, want) {
			t.Errorf("received %x, want %x", raw, want)
		}
	case <-time.After(2 * time.Second):
		t.Skip("multicast datagram did not loop back on this machine (common on macOS)")
	}
}

// TestFullPipeline composes every stage so far: a span is pruned, marshalled,
// sealed, sent over UDP, received, opened, unmarshalled and compared. This
// is the edge-to-gateway path minus the gateway's OTLP export.
func TestFullPipeline(t *testing.T) {
	master, _ := seal.NewMasterKey()
	sealer, _ := seal.NewSealer(master, testNode)
	opener := seal.NewOpener(master)

	rx, got := startReceiver(t)
	tx, err := udp.NewSender(rx.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Close()

	// Edge side.
	pruned := prune.Span(payload.SampleSpan(), prune.Default)
	dt, plain, _ := payload.Marshal(pruned)
	raw, err := sealer.Seal(frame.Header{DataType: dt}, plain)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Send(raw); err != nil {
		t.Fatal(err)
	}
	t.Logf("on the wire: %d bytes", len(raw))

	// Gateway side.
	fr, err := opener.Open(recv(t, got))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	msg, err := payload.Unmarshal(fr.DataType, fr.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(msg, pruned) {
		t.Error("span that arrived differs from the span that was sent")
	}
	if fr.NodeID != testNode {
		t.Errorf("NodeID = %v, want %v", fr.NodeID, testNode)
	}
}

func ExampleSender() {
	rx, _ := udp.NewReceiver("127.0.0.1:0")
	defer rx.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	got := make(chan []byte, 1)
	go rx.Run(ctx, func(raw []byte, _ *net.UDPAddr) { got <- raw })

	tx, _ := udp.NewSender(rx.LocalAddr().String())
	defer tx.Close()
	raw, _ := frame.Encode(frame.Header{DataType: frame.DataTypeSpan, NodeID: testNode}, []byte("hi"))
	_ = tx.Send(raw)

	fr, _ := frame.Decode(<-got)
	fmt.Printf("%d-byte datagram, %v from %v: %q\n", len(raw), fr.DataType, fr.NodeID, fr.Payload)
	// Output: 26-byte datagram, span from 01020304-0506-0708-090a-0b0c0d0e0f10: "hi"
}
