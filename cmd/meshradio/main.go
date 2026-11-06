// Command meshradio is the protocol with nothing else attached: one program
// that transmits sealed frames over UDP and one that receives them, printing
// every step. No OpenTelemetry SDK, no gateway, no backend. It exists to test
// the wire protocol between two real machines before anything cleverer is
// built on top.
//
//	meshradio keygen                              # print a new master key (hex)
//	meshradio nodekey -master KEY [-node ID]      # derive a node's key at "enrolment"
//	meshradio tx -key NODEKEY -node ID -to ADDR   # transmit a sealed span every -every
//	meshradio rx -master KEY -listen ADDR         # receive, open, and print
//
// Keying follows SPEC.md §8.1: the receiver holds the master key, the
// transmitter holds only its own derived key.
//
// LEARN: subcommands. One binary, several modes, each with its own flags.
// The standard library's flag package supports this through flag.NewFlagSet:
// one FlagSet per subcommand, parsed on the arguments after the mode word.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"sync/atomic"
	"time"

	"github.com/jakesky45/otlp-lite-mesh/pkg/frame"
	"github.com/jakesky45/otlp-lite-mesh/pkg/payload"
	"github.com/jakesky45/otlp-lite-mesh/pkg/prune"
	"github.com/jakesky45/otlp-lite-mesh/pkg/seal"
	"github.com/jakesky45/otlp-lite-mesh/pkg/transport/udp"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	// LEARN: os.Args[0] is the program name, os.Args[1] the first argument,
	// and os.Args[2:] everything after it. Each subcommand parses its own
	// flags from that tail.
	switch os.Args[1] {
	case "keygen":
		err = keygen()
	case "nodekey":
		err = nodekey(os.Args[2:])
	case "tx":
		err = tx(os.Args[2:])
	case "rx":
		err = rx(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "meshradio: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "meshradio:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  meshradio keygen
  meshradio nodekey -master HEX [-node HEX32]
  meshradio tx -key HEX -node HEX32 [-to ADDR] [-every DUR] [-count N]
  meshradio rx -master HEX [-listen ADDR] [-insecure]

Typical two-machine test (receiver R, transmitter T):
  R$ meshradio keygen                       -> MASTER
  R$ meshradio nodekey -master MASTER       -> NODE and NODEKEY (copy both to T)
  R$ meshradio rx -master MASTER -listen 0.0.0.0:4319
  T$ meshradio tx -key NODEKEY -node NODE -to <R's IP>:4319
`)
}

// keygen prints a fresh master key.
func keygen() error {
	k, err := seal.NewMasterKey()
	if err != nil {
		return err
	}
	fmt.Println(k)
	return nil
}

// nodekey is "enrolment": derive one node's key from the master. The node ID
// is random unless given, and both are printed so they can be copied to the
// device. The device never sees the master.
func nodekey(args []string) error {
	fs := flag.NewFlagSet("nodekey", flag.ExitOnError)
	masterHex := fs.String("master", "", "master key, 64 hex chars (required)")
	nodeHex := fs.String("node", "", "node ID, 32 hex chars (default: random)")
	fs.Parse(args)

	master, err := seal.ParseMasterKey(*masterHex)
	if err != nil {
		return err
	}
	node, err := parseOrRandomNode(*nodeHex)
	if err != nil {
		return err
	}
	key, err := seal.NodeKey(master, node)
	if err != nil {
		return err
	}
	fmt.Printf("node    %x\n", node[:])
	fmt.Printf("nodekey %x\n", key)
	fmt.Printf("\n# on the transmitting machine:\n# meshradio tx -key %x -node %x -to <receiver-ip>:4319\n", key, node[:])
	return nil
}

// tx seals and sends one frame per interval. The payload is the pruned
// sample span with sys.latency replaced by the frame number, so every frame
// is different and the receiver can show the sequence.
func tx(args []string) error {
	fs := flag.NewFlagSet("tx", flag.ExitOnError)
	keyHex := fs.String("key", "", "this node's key, 64 hex chars (required)")
	nodeHex := fs.String("node", "", "this node's ID, 32 hex chars (required)")
	to := fs.String("to", udp.DefaultGroup, "destination: multicast group or host:port")
	every := fs.Duration("every", 2*time.Second, "interval between frames")
	count := fs.Int("count", 0, "stop after this many frames (0 = until Ctrl-C)")
	fs.Parse(args)

	key, err := hex.DecodeString(*keyHex)
	if err != nil {
		return fmt.Errorf("-key: %w", err)
	}
	node, err := parseNode(*nodeHex)
	if err != nil {
		return err
	}
	sealer, err := seal.NewSealerFromKey(key, node)
	if err != nil {
		return err
	}
	sender, err := udp.NewSender(*to)
	if err != nil {
		return err
	}
	defer sender.Close()

	fmt.Fprintf(os.Stderr, "tx: node %v -> %s every %s (Ctrl-C to stop)\n", node, *to, *every)

	ctx := interruptContext()
	// LEARN: time.Ticker delivers the current time on a channel at a fixed
	// interval. Combined with select and ctx.Done(), it is the standard
	// "do something periodically until told to stop" loop.
	ticker := time.NewTicker(*every)
	defer ticker.Stop()

	for n := 1; *count == 0 || n <= *count; n++ {
		span := prune.Span(payload.SampleSpan(), prune.Default)
		span.Attributes[2] = payload.IntAttr("sys.latency", int64(n)) // sys.latency slot
		dt, plain, err := payload.Marshal(span)
		if err != nil {
			return err
		}
		raw, err := sealer.Seal(frame.Header{DataType: dt}, plain)
		if err != nil {
			return err
		}
		if err := sender.Send(raw); err != nil {
			return err
		}
		fmt.Printf("tx #%-4d %3d B payload -> %3d B sealed frame -> %s\n", n, len(plain), len(raw), *to)

		select {
		case <-ticker.C:
		case <-ctx.Done():
			fmt.Fprintf(os.Stderr, "tx: stopped after %d frames\n", sender.Sent())
			return nil
		}
	}
	return nil
}

// rx listens, opens every frame it can, and prints a line per frame plus a
// running tally of what was rejected and why.
func rx(args []string) error {
	fs := flag.NewFlagSet("rx", flag.ExitOnError)
	masterHex := fs.String("master", "", "master key, 64 hex chars (required unless -insecure)")
	listen := fs.String("listen", udp.DefaultGroup, "address to listen on: multicast group or ip:port")
	insecure := fs.Bool("insecure", false, "accept unsealed frames too (testing only)")
	fs.Parse(args)

	var opener *seal.Opener
	if *masterHex == "" && !*insecure {
		return fmt.Errorf("-master is required (or -insecure to accept unsealed frames)")
	}
	master, err := seal.ParseMasterKey(*masterHex)
	if err != nil && !*insecure {
		return err
	}
	opener = seal.NewOpener(master)
	opener.RequireSealed = !*insecure

	rxr, err := udp.NewReceiver(*listen)
	if err != nil {
		return err
	}
	defer rxr.Close()
	fmt.Fprintf(os.Stderr, "rx: listening on %s (Ctrl-C to stop)\n", rxr.LocalAddr())

	// LEARN: atomic counters again, because the summary at the end reads them
	// after Run has returned; cheap and safe without a mutex.
	var ok, bad, replays atomic.Uint64

	ctx := interruptContext()
	err = rxr.Run(ctx, func(raw []byte, from *net.UDPAddr) {
		fr, err := opener.Open(raw)
		if err != nil {
			bad.Add(1)
			if isReplay(err) {
				replays.Add(1)
			}
			fmt.Printf("rx  REJECT %3d B from %-21s %v\n", len(raw), from, err)
			return
		}
		ok.Add(1)
		describe := fmt.Sprintf("%v payload %d B", fr.DataType, len(fr.Payload))
		if fr.DataType == frame.DataTypeSpan {
			if m, err := payload.Unmarshal(fr.DataType, fr.Payload); err == nil {
				if s, isSpan := m.(interface{ GetName() string }); isSpan {
					describe = fmt.Sprintf("span %q", s.GetName())
				}
			}
		}
		fmt.Printf("rx  OK     %3d B from %-21s node %v  %s\n", len(raw), from, fr.NodeID, describe)
	})

	st := rxr.Stats()
	fmt.Fprintf(os.Stderr, "rx: stopped. received %d, opened %d, rejected %d (replays %d), oversized %d\n",
		st.Received, ok.Load(), bad.Load(), replays.Load(), st.Oversized)
	return err
}

func isReplay(err error) bool {
	// LEARN: errors.Is would be the usual tool; written out here to show
	// that wrapped errors can also be matched by unwrapping manually.
	for e := err; e != nil; e = unwrap(e) {
		if e == seal.ErrReplay {
			return true
		}
	}
	return false
}

func unwrap(err error) error {
	u, ok := err.(interface{ Unwrap() error })
	if !ok {
		return nil
	}
	return u.Unwrap()
}

// interruptContext returns a context that is cancelled on Ctrl-C.
//
// LEARN: signal.NotifyContext ties an OS signal to a context, so the same
// ctx.Done() that stops a receiver's Run loop also fires when the user hits
// Ctrl-C. One mechanism for "stop", whatever the reason.
func interruptContext() context.Context {
	ctx, _ := signal.NotifyContext(context.Background(), os.Interrupt)
	return ctx
}

func parseNode(s string) (frame.NodeID, error) {
	var n frame.NodeID
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != len(n) {
		return n, fmt.Errorf("-node must be %d hex chars", 2*len(n))
	}
	copy(n[:], b)
	return n, nil
}

func parseOrRandomNode(s string) (frame.NodeID, error) {
	if s != "" {
		return parseNode(s)
	}
	var n frame.NodeID
	if _, err := rand.Read(n[:]); err != nil {
		return n, err
	}
	return n, nil
}
