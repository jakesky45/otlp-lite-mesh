package compress_test

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/jakesky45/otlp-lite-mesh/pkg/compress"
	"github.com/jakesky45/otlp-lite-mesh/pkg/frame"
	"github.com/jakesky45/otlp-lite-mesh/pkg/payload"
	"github.com/jakesky45/otlp-lite-mesh/pkg/prune"
)

// prunedSpan is the 235-byte payload from Stage 3: the realistic input.
func prunedSpan(t testing.TB) []byte {
	t.Helper()
	_, b, err := payload.Marshal(prune.Span(payload.SampleSpan(), prune.Default))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// rawSpan is the 1090-byte unpruned span, for comparison.
func rawSpan(t testing.TB) []byte {
	t.Helper()
	_, b, err := payload.Marshal(payload.SampleSpan())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRoundTrip(t *testing.T) {
	inputs := map[string][]byte{
		"empty":       {},
		"one byte":    {0x42},
		"pruned span": prunedSpan(t),
		"raw span":    rawSpan(t),
		"repetitive":  bytes.Repeat([]byte("sys.battery=83;"), 100),
		"incompressible": func() []byte {
			b := make([]byte, 512)
			for i := range b {
				b[i] = byte(i*7919 + i>>3) // pseudo-random
			}
			return b
		}(),
	}
	for name, in := range inputs {
		for _, algo := range []compress.Algo{compress.None, compress.Gzip, compress.Zstd} {
			t.Run(name+"/"+algo.String(), func(t *testing.T) {
				enc, flags, err := compress.Encode(in, algo)
				if err != nil {
					t.Fatalf("Encode: %v", err)
				}
				if got := compress.FromFlags(flags); got != algo {
					t.Errorf("flags %v read back as %v, want %v", flags, got, algo)
				}
				dec, err := compress.Decode(enc, flags, frame.DefaultMaxPayload*10)
				if err != nil {
					t.Fatalf("Decode: %v", err)
				}
				if !bytes.Equal(dec, in) {
					t.Errorf("round trip changed the bytes (%d in, %d out)", len(in), len(dec))
				}
			})
		}
	}
}

func TestFlagsMapping(t *testing.T) {
	if compress.Gzip.Flags() != frame.FlagCompressed {
		t.Error("gzip should set only bit 0")
	}
	if compress.Zstd.Flags() != frame.FlagCompressed|frame.FlagZstd {
		t.Error("zstd should set bits 0 and 1")
	}
	if compress.None.Flags() != 0 {
		t.Error("none should set no bits")
	}
	// The sealed bit is someone else's; FromFlags must ignore it.
	if compress.FromFlags(frame.FlagSealed) != compress.None {
		t.Error("sealed-only flags should read as no compression")
	}
}

// TestAutoLeavesSmallPayloadsAlone is the headline of Part 5: on a single
// pruned span, neither algorithm beats "do nothing".
func TestAutoLeavesSmallPayloadsAlone(t *testing.T) {
	in := prunedSpan(t)
	out, flags, err := compress.Auto(in)
	if err != nil {
		t.Fatal(err)
	}
	if flags != 0 {
		t.Errorf("Auto chose %v for a %d-byte pruned span; expected no compression", compress.FromFlags(flags), len(in))
	}
	if len(out) != len(in) {
		t.Errorf("Auto returned %d bytes for %d-byte input with no compression", len(out), len(in))
	}
}

func TestAutoCompressesWhenItHelps(t *testing.T) {
	in := bytes.Repeat([]byte("sys.battery=83;"), 100) // 1500 B, highly repetitive
	out, flags, err := compress.Auto(in)
	if err != nil {
		t.Fatal(err)
	}
	if flags == 0 {
		t.Fatal("Auto should compress a 1500-byte repetitive payload")
	}
	if len(out) >= len(in)/4 {
		t.Errorf("Auto only got %d -> %d bytes on very repetitive input", len(in), len(out))
	}
	t.Logf("repetitive 1500 B -> %d B with %v", len(out), compress.FromFlags(flags))
}

func TestDecodeRejectsBombs(t *testing.T) {
	// 64 KB of zeros compresses to almost nothing; a receiver with a 1256-byte
	// limit must refuse to inflate it.
	bomb := make([]byte, 64*1024)
	for _, algo := range []compress.Algo{compress.Gzip, compress.Zstd} {
		t.Run(algo.String(), func(t *testing.T) {
			enc, flags, err := compress.Encode(bomb, algo)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%d bytes of zeros -> %d bytes of %v", len(bomb), len(enc), algo)
			_, err = compress.Decode(enc, flags, frame.DefaultMaxPayload)
			if !errors.Is(err, compress.ErrTooLarge) {
				t.Errorf("error = %v, want ErrTooLarge", err)
			}
		})
	}
}

func TestDecodeRejectsGarbage(t *testing.T) {
	garbage := []byte("this is not compressed data at all")
	for _, algo := range []compress.Algo{compress.Gzip, compress.Zstd} {
		t.Run(algo.String(), func(t *testing.T) {
			_, err := compress.Decode(garbage, algo.Flags(), frame.DefaultMaxPayload)
			if !errors.Is(err, compress.ErrCorrupt) {
				t.Errorf("error = %v, want ErrCorrupt", err)
			}
		})
	}
}

func TestDecodeUncompressedIsPassthrough(t *testing.T) {
	in := []byte("plain")
	out, err := compress.Decode(in, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, in) {
		t.Error("uncompressed payload should pass through untouched")
	}
}

// ExampleAuto prints the size table for Part 5.
func ExampleAuto() {
	_, pruned, _ := payload.Marshal(prune.Span(payload.SampleSpan(), prune.Default))
	_, raw, _ := payload.Marshal(payload.SampleSpan())

	rows := []struct {
		name string
		in   []byte
	}{
		{"pruned span x1", pruned},
		{"pruned span x10", bytes.Repeat(pruned, 10)},
		{"raw span x1", raw},
	}
	fmt.Printf("%-16s %5s %5s %5s  %s\n", "payload", "raw", "gzip", "zstd", "auto picks")
	for _, r := range rows {
		g, _, _ := compress.Encode(r.in, compress.Gzip)
		z, _, _ := compress.Encode(r.in, compress.Zstd)
		_, f, _ := compress.Auto(r.in)
		fmt.Printf("%-16s %5d %5d %5d  %s\n", r.name, len(r.in), len(g), len(z), compress.FromFlags(f))
	}
	// Output:
	// payload            raw  gzip  zstd  auto picks
	// pruned span x1     235   248   248  none
	// pruned span x10   2350   270   250  zstd
	// raw span x1       1090   743   738  zstd
}

// LEARN: a benchmark is a test function named BenchmarkXxx taking *testing.B.
// The framework calls the loop body b.N times, choosing N so it runs about a
// second, and reports ns/op. Run with: go test -bench=. -benchmem ./pkg/compress
func BenchmarkEncode(b *testing.B) {
	in := prunedSpan(b)
	for _, algo := range []compress.Algo{compress.Gzip, compress.Zstd} {
		b.Run(algo.String(), func(b *testing.B) {
			b.SetBytes(int64(len(in)))
			for b.Loop() {
				if _, _, err := compress.Encode(in, algo); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkDecode(b *testing.B) {
	in := prunedSpan(b)
	for _, algo := range []compress.Algo{compress.Gzip, compress.Zstd} {
		enc, flags, _ := compress.Encode(in, algo)
		b.Run(algo.String(), func(b *testing.B) {
			b.SetBytes(int64(len(in)))
			for b.Loop() {
				if _, err := compress.Decode(enc, flags, frame.DefaultMaxPayload); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
