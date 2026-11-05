// Package compress shrinks a frame payload, or chooses not to, and records the
// choice in the frame's Flags (SPEC.md §3.2, bits 0 and 1).
//
// Two algorithms are supported, gzip and zstd. Both add a fixed header and
// trailer of their own, so on a payload of a few hundred bytes they can make
// it bigger. Auto tries them and keeps the smallest result, including "leave
// it alone", which is what usually wins for a single pruned span.
//
// LEARN: this package is mostly about io.Reader and io.Writer, Go's two
// universal interfaces for "a stream of bytes". Compression libraries are
// written against them, so the same code can compress to a file, a network
// socket or (here) an in-memory bytes.Buffer.
package compress

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"

	"github.com/jakesky45/otlp-lite-mesh/pkg/frame"
)

// Algo selects a compression algorithm.
type Algo uint8

// LEARN: iota is a counter that starts at 0 inside a const block and goes up
// by one per line. It's how Go writes enums: None=0, Gzip=1, Zstd=2.
const (
	None Algo = iota
	Gzip
	Zstd
)

func (a Algo) String() string {
	switch a {
	case None:
		return "none"
	case Gzip:
		return "gzip"
	case Zstd:
		return "zstd"
	default:
		return fmt.Sprintf("algo(%d)", uint8(a))
	}
}

// Flags returns the frame flag bits that announce this algorithm.
func (a Algo) Flags() frame.Flags {
	switch a {
	case Gzip:
		return frame.FlagCompressed
	case Zstd:
		return frame.FlagCompressed | frame.FlagZstd
	default:
		return 0
	}
}

// FromFlags reads the algorithm back out of a frame's flags.
func FromFlags(f frame.Flags) Algo {
	switch {
	case f.Zstd():
		return Zstd
	case f.Compressed():
		return Gzip
	default:
		return None
	}
}

// Errors returned by Decode.
var (
	ErrTooLarge = errors.New("compress: decompressed payload exceeds limit")
	ErrCorrupt  = errors.New("compress: payload is not valid compressed data")
)

// LEARN: a zstd encoder/decoder is expensive to build and safe to share, so
// the package makes one of each at start-up and reuses them. Variables at
// package level are initialised before main runs. zstd.NewWriter(nil) means
// "no destination yet"; we only use its EncodeAll method, which takes bytes.
var (
	zstdEnc, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBestCompression))
	zstdDec, _ = zstd.NewReader(nil)
)

// Encode compresses payload with algo and returns the result together with
// the flag bits to set on the frame. With None it returns payload unchanged.
func Encode(payload []byte, algo Algo) ([]byte, frame.Flags, error) {
	switch algo {
	case None:
		return payload, 0, nil

	case Gzip:
		// LEARN: bytes.Buffer is an in-memory io.Writer (and io.Reader). Its
		// zero value is ready to use: no constructor call needed.
		var buf bytes.Buffer
		w, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		if err != nil {
			return nil, 0, err
		}
		if _, err := w.Write(payload); err != nil {
			return nil, 0, err
		}
		// LEARN: Close is not optional for compressors. It flushes the last
		// block and writes the trailer; without it the output is truncated.
		if err := w.Close(); err != nil {
			return nil, 0, err
		}
		return buf.Bytes(), algo.Flags(), nil

	case Zstd:
		// EncodeAll appends to the second argument; nil means "start a new slice".
		return zstdEnc.EncodeAll(payload, nil), algo.Flags(), nil

	default:
		return nil, 0, fmt.Errorf("compress: unknown algorithm %v", algo)
	}
}

// Auto returns the smallest of: payload as-is, gzip, zstd. Ties go to the
// simpler option, so an uncompressed result is preferred when nothing helps.
func Auto(payload []byte) ([]byte, frame.Flags, error) {
	best, bestFlags := payload, frame.Flags(0)
	for _, algo := range []Algo{Zstd, Gzip} {
		out, flags, err := Encode(payload, algo)
		if err != nil {
			return nil, 0, err
		}
		if len(out) < len(best) {
			best, bestFlags = out, flags
		}
	}
	return best, bestFlags, nil
}

// Decode reverses Encode according to the frame's flags. maxSize caps the
// decompressed size: a 200-byte payload that claims to inflate to 2 GB is an
// attack, not telemetry, and is rejected with ErrTooLarge before the memory
// is allocated. Uncompressed payloads are returned as-is, unchecked, since
// the frame layer already bounded their size.
func Decode(payload []byte, flags frame.Flags, maxSize int) ([]byte, error) {
	switch FromFlags(flags) {
	case None:
		return payload, nil

	case Gzip:
		r, err := gzip.NewReader(bytes.NewReader(payload))
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
		}
		// LEARN: defer runs the call when the surrounding function returns,
		// whichever return it is. It's how Go guarantees clean-up without
		// try/finally. Deferred calls run last-in, first-out.
		defer r.Close()

		// LEARN: io.LimitReader wraps a reader and stops after N bytes. We ask
		// for one more than the limit: if we get it, the real size was over.
		out, err := io.ReadAll(io.LimitReader(r, int64(maxSize)+1))
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
		}
		if len(out) > maxSize {
			return nil, fmt.Errorf("%w: more than %d bytes", ErrTooLarge, maxSize)
		}
		return out, nil

	case Zstd:
		// zstd frames carry the decompressed size up front when the encoder
		// knows it (EncodeAll always does), so we can refuse before inflating.
		var hdr zstd.Header
		if err := hdr.Decode(payload); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
		}
		if hdr.HasFCS && hdr.FrameContentSize > uint64(maxSize) {
			return nil, fmt.Errorf("%w: header says %d bytes", ErrTooLarge, hdr.FrameContentSize)
		}
		out, err := zstdDec.DecodeAll(payload, nil)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
		}
		if len(out) > maxSize {
			return nil, fmt.Errorf("%w: more than %d bytes", ErrTooLarge, maxSize)
		}
		return out, nil

	default:
		return nil, fmt.Errorf("compress: unknown flags %v", flags)
	}
}
