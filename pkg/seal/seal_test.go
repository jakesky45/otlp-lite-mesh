package seal_test

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/jakesky45/otlp-lite-mesh/pkg/frame"
	"github.com/jakesky45/otlp-lite-mesh/pkg/payload"
	"github.com/jakesky45/otlp-lite-mesh/pkg/prune"
	"github.com/jakesky45/otlp-lite-mesh/pkg/seal"
)

var (
	nodeA = frame.NodeID{0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 1, 2, 3, 4, 5, 6, 7, 8}
	nodeB = frame.NodeID{0xbb, 0xbb, 0xbb, 0xbb, 0xbb, 0xbb, 0xbb, 0xbb, 1, 2, 3, 4, 5, 6, 7, 8}
)

// fixedKey is deterministic so failures are reproducible. Never do this
// outside a test.
func fixedKey(t testing.TB, fill byte) seal.MasterKey {
	t.Helper()
	var k seal.MasterKey
	for i := range k {
		k[i] = fill
	}
	return k
}

func spanHeader() frame.Header {
	return frame.Header{DataType: frame.DataTypeSpan}
}

func TestRoundTrip(t *testing.T) {
	master := fixedKey(t, 0x11)
	sealer, err := seal.NewSealer(master, nodeA)
	if err != nil {
		t.Fatal(err)
	}
	opener := seal.NewOpener(master)

	plain := []byte("hello mesh")
	raw, err := sealer.Seal(spanHeader(), plain)
	if err != nil {
		t.Fatal(err)
	}
	if want := frame.HeaderSize + len(plain) + seal.Overhead; len(raw) != want {
		t.Errorf("sealed frame is %d bytes, want %d", len(raw), want)
	}

	// The header is in clear: a relay can still decode it.
	hdr, err := frame.Decode(raw)
	if err != nil {
		t.Fatalf("relay decode: %v", err)
	}
	if !hdr.Flags.Sealed() || hdr.NodeID != nodeA || hdr.DataType != frame.DataTypeSpan {
		t.Errorf("header not as expected: %+v", hdr.Header)
	}
	if bytes.Contains(hdr.Payload, plain) {
		t.Error("plaintext is visible in the sealed payload")
	}

	fr, err := opener.Open(raw)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(fr.Payload, plain) {
		t.Errorf("payload = %q, want %q", fr.Payload, plain)
	}
	if fr.Flags.Sealed() {
		t.Error("FlagSealed should be cleared after opening")
	}
	if fr.PayloadLen != uint32(len(plain)) {
		t.Errorf("PayloadLen = %d, want %d", fr.PayloadLen, len(plain))
	}
}

// TestOverheadIsTwenty pins the number quoted in the blog and the spec errata.
func TestOverheadIsTwenty(t *testing.T) {
	if seal.Overhead != 20 {
		t.Fatalf("Overhead = %d, want 20", seal.Overhead)
	}
	_, pruned, _ := payload.Marshal(prune.Span(payload.SampleSpan(), prune.Default))
	sealer, _ := seal.NewSealer(fixedKey(t, 1), nodeA)
	raw, err := sealer.Seal(spanHeader(), pruned)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("pruned span %d B -> sealed frame %d B on the wire (header %d + overhead %d)",
		len(pruned), len(raw), frame.HeaderSize, seal.Overhead)
	if len(raw) != 279 {
		t.Errorf("sealed pruned span frame is %d bytes, expected 279", len(raw))
	}
}

// TestTamperAnyByteFails flips every single byte of a sealed frame in turn,
// header included, and checks each one is rejected. This is the property
// that makes "header as associated data" worth it.
func TestTamperAnyByteFails(t *testing.T) {
	master := fixedKey(t, 0x22)
	sealer, _ := seal.NewSealer(master, nodeA)
	raw, _ := sealer.Seal(spanHeader(), []byte("tamper me"))

	for i := range raw {
		mutated := bytes.Clone(raw)
		mutated[i] ^= 0x01               // flip the lowest bit
		opener := seal.NewOpener(master) // fresh window each time, so replay never masks the result
		_, err := opener.Open(mutated)
		if err == nil {
			t.Errorf("byte %d flipped: Open succeeded, should have failed", i)
			continue
		}
		// Header bytes 0..3 and 20..23 may trip frame.Decode first (bad magic,
		// version, flags, length). Everything else must be an auth failure.
		if i >= 4 && i < 20 || i >= frame.HeaderSize {
			if !errors.Is(err, seal.ErrAuth) {
				t.Errorf("byte %d flipped: error = %v, want ErrAuth", i, err)
			}
		}
	}
}

func TestWrongKeyFails(t *testing.T) {
	sealer, _ := seal.NewSealer(fixedKey(t, 0x33), nodeA)
	raw, _ := sealer.Seal(spanHeader(), []byte("secret"))
	opener := seal.NewOpener(fixedKey(t, 0x34))
	if _, err := opener.Open(raw); !errors.Is(err, seal.ErrAuth) {
		t.Errorf("error = %v, want ErrAuth", err)
	}
}

func TestNodesHaveDifferentKeys(t *testing.T) {
	master := fixedKey(t, 0x44)
	ka, _ := seal.NodeKey(master, nodeA)
	kb, _ := seal.NodeKey(master, nodeB)
	if bytes.Equal(ka, kb) {
		t.Error("two nodes derived the same key")
	}
	if bytes.Equal(ka, master[:]) {
		t.Error("node key equals master key")
	}
	// Derivation must be deterministic: the gateway recomputes it.
	ka2, _ := seal.NodeKey(master, nodeA)
	if !bytes.Equal(ka, ka2) {
		t.Error("NodeKey is not deterministic")
	}
}

func TestReplayRejected(t *testing.T) {
	master := fixedKey(t, 0x55)
	sealer, _ := seal.NewSealer(master, nodeA)
	opener := seal.NewOpener(master)

	raw, _ := sealer.Seal(spanHeader(), []byte("once"))
	if _, err := opener.Open(raw); err != nil {
		t.Fatalf("first open: %v", err)
	}
	if _, err := opener.Open(raw); !errors.Is(err, seal.ErrReplay) {
		t.Errorf("second open: error = %v, want ErrReplay", err)
	}
}

func TestReorderWithinWindowAccepted(t *testing.T) {
	master := fixedKey(t, 0x66)
	sealer, _ := seal.NewSealer(master, nodeA)
	opener := seal.NewOpener(master)

	var frames [][]byte
	for i := 0; i < 5; i++ {
		raw, _ := sealer.Seal(spanHeader(), []byte{byte(i)})
		frames = append(frames, raw)
	}
	// Deliver 4, 0, 2, 1, 3: all distinct, all within 64 of the newest.
	for _, i := range []int{4, 0, 2, 1, 3} {
		if _, err := opener.Open(frames[i]); err != nil {
			t.Errorf("frame %d out of order: %v", i, err)
		}
	}
	// And each one again is a replay.
	for i := range frames {
		if _, err := opener.Open(frames[i]); !errors.Is(err, seal.ErrReplay) {
			t.Errorf("frame %d repeated: error = %v, want ErrReplay", i, err)
		}
	}
}

func TestTooOldRejected(t *testing.T) {
	master := fixedKey(t, 0x77)
	sealer, _ := seal.NewSealer(master, nodeA)
	opener := seal.NewOpener(master)

	first, _ := sealer.Seal(spanHeader(), []byte("0"))
	var last []byte
	for i := 1; i <= 70; i++ { // counters 1..70, well past the 64 window
		last, _ = sealer.Seal(spanHeader(), []byte("x"))
	}
	if _, err := opener.Open(last); err != nil {
		t.Fatal(err)
	}
	if _, err := opener.Open(first); !errors.Is(err, seal.ErrReplay) {
		t.Errorf("counter 0 after counter 70: error = %v, want ErrReplay", err)
	}
}

func TestUnsealedFrames(t *testing.T) {
	master := fixedKey(t, 0x88)
	plain, _ := frame.Encode(frame.Header{DataType: frame.DataTypeSpan, NodeID: nodeA}, []byte("clear"))

	strict := seal.NewOpener(master)
	if _, err := strict.Open(plain); !errors.Is(err, seal.ErrUnsealed) {
		t.Errorf("strict: error = %v, want ErrUnsealed", err)
	}

	lax := seal.NewOpener(master)
	lax.RequireSealed = false
	fr, err := lax.Open(plain)
	if err != nil {
		t.Fatalf("lax: %v", err)
	}
	if string(fr.Payload) != "clear" {
		t.Errorf("lax: payload = %q", fr.Payload)
	}
}

func TestSealedButTooShort(t *testing.T) {
	master := fixedKey(t, 0x99)
	// A frame that claims to be sealed but carries only 5 bytes of payload.
	raw, _ := frame.Encode(frame.Header{DataType: frame.DataTypeSpan, Flags: frame.FlagSealed, NodeID: nodeA}, []byte("short"))
	if _, err := seal.NewOpener(master).Open(raw); !errors.Is(err, seal.ErrTooShort) {
		t.Errorf("error = %v, want ErrTooShort", err)
	}
}

func TestMasterKeyHex(t *testing.T) {
	k, err := seal.NewMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	k2, err := seal.ParseMasterKey(k.String())
	if err != nil {
		t.Fatal(err)
	}
	if k != k2 {
		t.Error("hex round trip changed the key")
	}
	for _, bad := range []string{"", "abc", "zz", k.String()[:62]} {
		if _, err := seal.ParseMasterKey(bad); !errors.Is(err, seal.ErrBadKey) {
			t.Errorf("ParseMasterKey(%q): error = %v, want ErrBadKey", bad, err)
		}
	}
}

// ExampleSealer prints the on-the-wire picture for Part 6.
func ExampleSealer() {
	master, _ := seal.ParseMasterKey("000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
	node := frame.NodeID{0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}

	sealer, _ := seal.NewSealer(master, node)
	raw, _ := sealer.Seal(frame.Header{DataType: frame.DataTypeSpan}, []byte("hello mesh"))

	fmt.Printf("plaintext      %d bytes\n", len("hello mesh"))
	fmt.Printf("sealed frame   %d bytes = header %d + counter 4 + ciphertext %d + tag 16\n",
		len(raw), frame.HeaderSize, len("hello mesh"))
	fmt.Printf("flags byte     0x%02x (%v)\n", raw[3], frame.Flags(raw[3]))
	fmt.Printf("counter        % x\n", raw[24:28])

	opened, _ := seal.NewOpener(master).Open(raw)
	fmt.Printf("opened         %q, flags %v\n", opened.Payload, opened.Flags)
	// Output:
	// plaintext      10 bytes
	// sealed frame   54 bytes = header 24 + counter 4 + ciphertext 10 + tag 16
	// flags byte     0x04 (sealed)
	// counter        00 00 00 00
	// opened         "hello mesh", flags none
}
