// Package frame implements the OTLP-Lite-Mesh wire format described in
// SPEC.md §3: a fixed 24-byte header followed by a variable-length payload.
//
// The header is deliberately simple so that a relay node can forward a frame
// without understanding what is inside it:
//
//	offset  0      1        2         3       4..19    20..23      24..
//	       +------+--------+---------+-------+--------+-----------+---------+
//	       |Magic |Version |DataType |Flags  | NodeID |PayloadLen | Payload |
//	       +------+--------+---------+-------+--------+-----------+---------+
//	bytes    1      1        1         1       16       4          N
//
// All multi-byte integers are big-endian ("network byte order"). This package
// only knows how to lay bytes out and check them; it does not know what the
// payload means. That is the job of the payload, prune, compress and seal
// packages.
package frame

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
)

// Wire-format constants from SPEC.md §3.2.
const (
	// Magic is the first byte of every frame: ASCII 'O'. A receiver that sees
	// anything else drops the packet without reading further (SPEC.md §6.1).
	Magic byte = 0x4F

	// Version is the only header layout this package understands.
	Version byte = 0x01

	// HeaderSize is the fixed length of the header in bytes.
	HeaderSize = 24

	// DefaultMaxPayload is the largest PayloadLen a receiver accepts unless told
	// otherwise (errata E2). It keeps a whole frame inside the 1280-byte IPv6
	// minimum MTU, so the frame is never split into IP fragments.
	DefaultMaxPayload = 1280 - HeaderSize
)

// DataType says which single OTLP message the payload holds (SPEC.md §3.2 and
// errata E1).
type DataType uint8

// The three payload types defined by the spec.
const (
	DataTypeLog    DataType = 0x01 // one opentelemetry.proto.logs.v1.LogRecord
	DataTypeMetric DataType = 0x02 // one opentelemetry.proto.metrics.v1.Metric
	DataTypeSpan   DataType = 0x03 // one opentelemetry.proto.trace.v1.Span
)

// String returns a short human-readable name, used by meshdump and in errors.
func (d DataType) String() string {
	switch d {
	case DataTypeLog:
		return "log"
	case DataTypeMetric:
		return "metric"
	case DataTypeSpan:
		return "span"
	default:
		return fmt.Sprintf("unknown(0x%02x)", uint8(d))
	}
}

func (d DataType) valid() bool {
	return d >= DataTypeLog && d <= DataTypeSpan
}

// Flags is the header's bit field (SPEC.md §3.2, errata E3 and E7).
//
//	bit 0  FlagCompressed  payload is compressed
//	bit 1  FlagZstd        compression is zstd (gzip when clear); only valid with bit 0
//	bit 2  FlagSealed      payload is encrypted and authenticated (errata E7)
//	bits 3..7             reserved, must be zero
type Flags uint8

// Individual flag bits.
const (
	FlagCompressed Flags = 1 << 0
	FlagZstd       Flags = 1 << 1
	FlagSealed     Flags = 1 << 2

	reservedFlags Flags = 0xF8 // bits 3..7
)

// Compressed reports whether the payload is compressed.
func (f Flags) Compressed() bool { return f&FlagCompressed != 0 }

// Zstd reports whether the payload is zstd-compressed. It is false for gzip
// and false when the payload is not compressed at all.
func (f Flags) Zstd() bool { return f.Compressed() && f&FlagZstd != 0 }

// Sealed reports whether the payload is encrypted and authenticated.
func (f Flags) Sealed() bool { return f&FlagSealed != 0 }

// Validate enforces the rules in errata E3: no reserved bits, and no zstd bit
// without the compressed bit.
func (f Flags) Validate() error {
	if f&reservedFlags != 0 {
		return fmt.Errorf("%w: 0x%02x", ErrReservedFlags, uint8(f))
	}
	if f&FlagZstd != 0 && !f.Compressed() {
		return ErrZstdWithoutCompression
	}
	return nil
}

// String renders the set bits as "compressed|zstd|sealed", or "none".
func (f Flags) String() string {
	var parts []string
	if f.Compressed() {
		parts = append(parts, "compressed")
	}
	if f&FlagZstd != 0 {
		parts = append(parts, "zstd")
	}
	if f.Sealed() {
		parts = append(parts, "sealed")
	}
	if f&reservedFlags != 0 {
		parts = append(parts, fmt.Sprintf("reserved(0x%02x)", uint8(f&reservedFlags)))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, "|")
}

// NodeID identifies the device a frame came from (SPEC.md §3.2, errata E5).
// It is an array, not a slice, so a Header is a plain value that can be
// copied and compared with == like any other.
type NodeID [16]byte

// String renders the ID in canonical UUID form, 8-4-4-4-12 hex digits.
func (n NodeID) String() string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", n[0:4], n[4:6], n[6:8], n[8:10], n[10:16])
}

// Header is the decoded 24-byte header. Magic and Version are not stored
// because they are constants: a Header that exists is already valid on those
// two fields.
type Header struct {
	DataType   DataType
	Flags      Flags
	NodeID     NodeID
	PayloadLen uint32
}

// Validate checks the fields that have a limited set of legal values.
func (h Header) Validate() error {
	if !h.DataType.valid() {
		return fmt.Errorf("%w: 0x%02x", ErrDataType, uint8(h.DataType))
	}
	return h.Flags.Validate()
}

// Frame is a decoded header plus its payload bytes.
type Frame struct {
	Header
	Payload []byte
}

// Errors returned by Decode and Encode. Compare with errors.Is, because the
// returned error may wrap one of these with extra detail.
var (
	ErrTooShort               = errors.New("frame: shorter than the 24-byte header")
	ErrBadMagic               = errors.New("frame: bad magic byte")
	ErrVersion                = errors.New("frame: unsupported version")
	ErrDataType               = errors.New("frame: unknown data type")
	ErrReservedFlags          = errors.New("frame: reserved flag bits set")
	ErrZstdWithoutCompression = errors.New("frame: zstd flag set without compressed flag")
	ErrPayloadTooLarge        = errors.New("frame: payload length exceeds limit")
	ErrLength                 = errors.New("frame: data length does not equal 24 + PayloadLen")
)

// Encode lays out a complete frame: header followed by payload. The
// PayloadLen field of h is ignored and taken from len(payload).
func Encode(h Header, payload []byte) ([]byte, error) {
	if len(payload) > math.MaxUint32 {
		return nil, fmt.Errorf("%w: %d bytes", ErrPayloadTooLarge, len(payload))
	}
	h.PayloadLen = uint32(len(payload))
	if err := h.Validate(); err != nil {
		return nil, err
	}

	buf := make([]byte, HeaderSize+len(payload))
	buf[0] = Magic
	buf[1] = Version
	buf[2] = byte(h.DataType)
	buf[3] = byte(h.Flags)
	copy(buf[4:20], h.NodeID[:])
	binary.BigEndian.PutUint32(buf[20:24], h.PayloadLen)
	copy(buf[HeaderSize:], payload)
	return buf, nil
}

// Decode parses one complete frame using DefaultMaxPayload as the size limit.
func Decode(b []byte) (Frame, error) {
	return DecodeLimit(b, DefaultMaxPayload)
}

// DecodeLimit parses one complete frame, rejecting payloads longer than
// maxPayload bytes. The checks run in the order SPEC.md §6 lists them, so the
// cheapest rejection happens first. The returned Payload is a copy, so the
// caller may reuse b (for example a UDP receive buffer) immediately.
func DecodeLimit(b []byte, maxPayload int) (Frame, error) {
	if len(b) < HeaderSize {
		return Frame{}, fmt.Errorf("%w: %d bytes", ErrTooShort, len(b))
	}
	if b[0] != Magic {
		return Frame{}, fmt.Errorf("%w: 0x%02x", ErrBadMagic, b[0])
	}
	if b[1] != Version {
		return Frame{}, fmt.Errorf("%w: %d", ErrVersion, b[1])
	}

	h := Header{
		DataType:   DataType(b[2]),
		Flags:      Flags(b[3]),
		PayloadLen: binary.BigEndian.Uint32(b[20:24]),
	}
	copy(h.NodeID[:], b[4:20])

	if err := h.Validate(); err != nil {
		return Frame{}, err
	}
	// Compare in uint64 so a hostile PayloadLen near 2^32 cannot overflow the
	// addition on 32-bit platforms.
	if uint64(h.PayloadLen) > uint64(maxPayload) {
		return Frame{}, fmt.Errorf("%w: %d > %d", ErrPayloadTooLarge, h.PayloadLen, maxPayload)
	}
	if uint64(len(b)) != HeaderSize+uint64(h.PayloadLen) {
		return Frame{}, fmt.Errorf("%w: got %d bytes, header says %d", ErrLength, len(b), HeaderSize+h.PayloadLen)
	}

	return Frame{Header: h, Payload: bytes.Clone(b[HeaderSize:])}, nil
}
