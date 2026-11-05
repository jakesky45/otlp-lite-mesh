// Package payload converts between OTLP protobuf messages and the raw bytes
// carried in a frame's payload (SPEC.md §3.2 and errata E1).
//
// Exactly one message travels per frame, and the frame's DataType says which:
//
//	frame.DataTypeSpan   -> *tracepb.Span
//	frame.DataTypeMetric -> *metricspb.Metric
//	frame.DataTypeLog    -> *logspb.LogRecord
//
// The Resource and InstrumentationScope that normally wrap these messages are
// not sent; the gateway rebuilds them from the frame's NodeID.
//
// The package also has Describe, which walks a serialised message field by
// field so you can see how many bytes each one costs on the wire.
package payload

import (
	"errors"
	"fmt"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/jakesky45/otlp-lite-mesh/pkg/frame"
)

// Errors returned by Marshal and Unmarshal. Compare with errors.Is.
var (
	ErrUnsupportedMessage = errors.New("payload: message type cannot travel in a frame")
	ErrUnsupportedType    = errors.New("payload: unknown frame data type")
)

// marshalOpts makes the output byte-for-byte stable for the same input, which
// keeps golden tests and size measurements honest.
var marshalOpts = proto.MarshalOptions{Deterministic: true}

// Marshal serialises one OTLP message and reports which DataType the frame
// must carry for it.
func Marshal(m proto.Message) (frame.DataType, []byte, error) {
	var dt frame.DataType
	switch m.(type) {
	case *tracepb.Span:
		dt = frame.DataTypeSpan
	case *metricspb.Metric:
		dt = frame.DataTypeMetric
	case *logspb.LogRecord:
		dt = frame.DataTypeLog
	default:
		return 0, nil, fmt.Errorf("%w: %T", ErrUnsupportedMessage, m)
	}
	b, err := marshalOpts.Marshal(m)
	if err != nil {
		return 0, nil, fmt.Errorf("payload: marshal %v: %w", dt, err)
	}
	return dt, b, nil
}

// Unmarshal parses payload bytes into the message type that dt names. The
// result is a proto.Message; callers that need the concrete type use a type
// assertion, for example m.(*tracepb.Span).
func Unmarshal(dt frame.DataType, b []byte) (proto.Message, error) {
	var m proto.Message
	switch dt {
	case frame.DataTypeSpan:
		m = &tracepb.Span{}
	case frame.DataTypeMetric:
		m = &metricspb.Metric{}
	case frame.DataTypeLog:
		m = &logspb.LogRecord{}
	default:
		return nil, fmt.Errorf("%w: %v", ErrUnsupportedType, dt)
	}
	if err := proto.Unmarshal(b, m); err != nil {
		return nil, fmt.Errorf("payload: unmarshal %v: %w", dt, err)
	}
	return m, nil
}

// Field summarises every occurrence of one field number in a serialised
// message: how many times it appeared and how many bytes it cost in total,
// including the tag and length prefix for each occurrence.
type Field struct {
	Number int
	Name   string // from the message schema, or "?" if the schema does not know it
	Wire   string // varint, fixed32, fixed64, bytes or group
	Count  int
	Bytes  int
}

// Describe walks the top-level fields of a protobuf wire-format message and
// returns one Field per field number, in the order first seen. md supplies
// the field names; pass (&tracepb.Span{}).ProtoReflect().Descriptor() for a
// span. Nested messages (attributes, events) are counted as opaque bytes.
func Describe(b []byte, md protoreflect.MessageDescriptor) ([]Field, error) {
	var out []Field
	index := map[protowire.Number]int{} // field number -> position in out

	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil, fmt.Errorf("payload: bad tag at offset: %w", protowire.ParseError(n))
		}
		m := protowire.ConsumeFieldValue(num, typ, b[n:])
		if m < 0 {
			return nil, fmt.Errorf("payload: bad value for field %d: %w", num, protowire.ParseError(m))
		}
		size := n + m

		i, seen := index[num]
		if !seen {
			i = len(out)
			index[num] = i
			out = append(out, Field{
				Number: int(num),
				Name:   fieldName(md, num),
				Wire:   wireName(typ),
			})
		}
		out[i].Count++
		out[i].Bytes += size

		b = b[size:]
	}
	return out, nil
}

func fieldName(md protoreflect.MessageDescriptor, num protowire.Number) string {
	if md == nil {
		return "?"
	}
	fd := md.Fields().ByNumber(protoreflect.FieldNumber(num))
	if fd == nil {
		return "?"
	}
	return string(fd.Name())
}

func wireName(t protowire.Type) string {
	switch t {
	case protowire.VarintType:
		return "varint"
	case protowire.Fixed32Type:
		return "fixed32"
	case protowire.Fixed64Type:
		return "fixed64"
	case protowire.BytesType:
		return "bytes"
	case protowire.StartGroupType, protowire.EndGroupType:
		return "group"
	default:
		return fmt.Sprintf("type%d", int(t))
	}
}
