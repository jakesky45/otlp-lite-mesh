// This file is in package payload_test rather than payload: an "external test
// package". It can only use the exported API, exactly like any other caller,
// which keeps the tests honest about what the package really offers.
package payload_test

import (
	"errors"
	"fmt"
	"testing"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/jakesky45/otlp-lite-mesh/pkg/frame"
	"github.com/jakesky45/otlp-lite-mesh/pkg/payload"
)

func TestMarshalPicksDataType(t *testing.T) {
	tests := []struct {
		name string
		msg  proto.Message
		want frame.DataType
	}{
		{"span", &tracepb.Span{Name: "x"}, frame.DataTypeSpan},
		{"metric", &metricspb.Metric{Name: "x"}, frame.DataTypeMetric},
		{"log", &logspb.LogRecord{SeverityText: "x"}, frame.DataTypeLog},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dt, b, err := payload.Marshal(tc.msg)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if dt != tc.want {
				t.Errorf("DataType = %v, want %v", dt, tc.want)
			}
			if len(b) == 0 {
				t.Error("Marshal returned no bytes")
			}
		})
	}
}

func TestMarshalRejectsOtherMessages(t *testing.T) {
	// A KeyValue is a valid protobuf message but not one a frame may carry.
	_, _, err := payload.Marshal(&commonpb.KeyValue{Key: "k"})
	if !errors.Is(err, payload.ErrUnsupportedMessage) {
		t.Errorf("error = %v, want ErrUnsupportedMessage", err)
	}
}

func TestRoundTrip(t *testing.T) {
	in := payload.SampleSpan()
	dt, b, err := payload.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	m, err := payload.Unmarshal(dt, b)
	if err != nil {
		t.Fatal(err)
	}
	out, ok := m.(*tracepb.Span) // type assertion: is this proto.Message really a *Span?
	if !ok {
		t.Fatalf("Unmarshal returned %T, want *tracepb.Span", m)
	}
	// proto.Equal, not ==: the structs hold pointers and internal state, so
	// Go's == would compare addresses rather than content.
	if !proto.Equal(in, out) {
		t.Errorf("round trip changed the span:\n in  %v\n out %v", in, out)
	}
}

func TestUnmarshalErrors(t *testing.T) {
	if _, err := payload.Unmarshal(frame.DataType(0x7f), nil); !errors.Is(err, payload.ErrUnsupportedType) {
		t.Errorf("unknown type: error = %v", err)
	}
	// 0xff is a tag byte that promises more bytes than exist.
	if _, err := payload.Unmarshal(frame.DataTypeSpan, []byte{0xff}); err == nil {
		t.Error("garbage bytes: expected an error")
	}
}

func TestMarshalIsDeterministic(t *testing.T) {
	_, a, _ := payload.Marshal(payload.SampleSpan())
	_, b, _ := payload.Marshal(payload.SampleSpan())
	if string(a) != string(b) {
		t.Error("two marshals of the same span differ")
	}
}

// TestSampleSpanSize pins the size of the sample so a change to it (or to the
// protobuf library) is noticed. The number itself is the headline of Part 3:
// this is how big one ordinary span is before any pruning.
func TestSampleSpanSize(t *testing.T) {
	_, b, err := payload.Marshal(payload.SampleSpan())
	if err != nil {
		t.Fatal(err)
	}
	const want = 1090
	if len(b) != want {
		t.Errorf("sample span is %d bytes, expected %d (update the constant if the sample changed on purpose)", len(b), want)
	}
	if len(b) <= frame.DefaultMaxPayload {
		t.Logf("note: sample span (%d B) fits a %d B payload before pruning", len(b), frame.DefaultMaxPayload)
	}
}

func TestDescribeAccountsForEveryByte(t *testing.T) {
	_, b, _ := payload.Marshal(payload.SampleSpan())
	fields, err := payload.Describe(b, (&tracepb.Span{}).ProtoReflect().Descriptor())
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, f := range fields {
		total += f.Bytes
	}
	if total != len(b) {
		t.Errorf("fields sum to %d bytes, message is %d", total, len(b))
	}
}

func TestDescribeRejectsGarbage(t *testing.T) {
	if _, err := payload.Describe([]byte{0xff, 0xff, 0xff}, nil); err == nil {
		t.Error("expected an error for a truncated tag")
	}
}

// ExampleDescribe is both documentation and a test: go test runs it and
// compares what it prints against the "Output:" comment below. It is also
// the table in Part 3.
func ExampleDescribe() {
	_, b, _ := payload.Marshal(payload.SampleSpan())
	fields, _ := payload.Describe(b, (&tracepb.Span{}).ProtoReflect().Descriptor())

	fmt.Printf("%3s  %-22s %-8s %5s  %5s\n", "#", "field", "wire", "count", "bytes")
	for _, f := range fields {
		fmt.Printf("%3d  %-22s %-8s %5d  %5d\n", f.Number, f.Name, f.Wire, f.Count, f.Bytes)
	}
	fmt.Printf("%41s  %5d\n", "total", len(b))
	// Output:
	//   #  field                  wire     count  bytes
	//   1  trace_id               bytes        1     18
	//   2  span_id                bytes        1     10
	//   3  trace_state            bytes        1     41
	//   4  parent_span_id         bytes        1     10
	//   5  name                   bytes        1     27
	//   6  kind                   varint       1      2
	//   7  start_time_unix_nano   fixed64      1      9
	//   8  end_time_unix_nano     fixed64      1      9
	//   9  attributes             bytes        9    493
	//  11  events                 bytes        2    449
	//  15  status                 bytes        1     22
	//                                     total   1090
}
