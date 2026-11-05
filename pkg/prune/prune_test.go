package prune_test

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/jakesky45/otlp-lite-mesh/pkg/payload"
	"github.com/jakesky45/otlp-lite-mesh/pkg/prune"
)

// size marshals m and returns its wire size, failing the test on error.
func size(t *testing.T, m proto.Message) int {
	t.Helper()
	_, b, err := payload.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return len(b)
}

// attrValue unwraps an attribute's value for printing.
func attrValue(kv *commonpb.KeyValue) any {
	switch v := kv.GetValue().GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return v.StringValue
	case *commonpb.AnyValue_IntValue:
		return v.IntValue
	default:
		return kv.GetValue()
	}
}

func TestSpanKeepsIdentity(t *testing.T) {
	in := payload.SampleSpan()
	out := prune.Span(in, prune.Default)

	if !bytes.Equal(out.TraceId, in.TraceId) {
		t.Error("TraceId changed")
	}
	if !bytes.Equal(out.SpanId, in.SpanId) {
		t.Error("SpanId changed")
	}
	if !bytes.Equal(out.ParentSpanId, in.ParentSpanId) {
		t.Error("ParentSpanId changed")
	}
	if out.Name != in.Name || out.Kind != in.Kind {
		t.Error("Name or Kind changed")
	}
	if out.StartTimeUnixNano != in.StartTimeUnixNano || out.EndTimeUnixNano != in.EndTimeUnixNano {
		t.Error("timestamps changed")
	}
	if out.Status.GetCode() != in.Status.GetCode() {
		t.Error("status code changed")
	}
}

func TestSpanDropsBaggage(t *testing.T) {
	in := payload.SampleSpan()
	out := prune.Span(in, prune.Default)

	if out.TraceState != "" {
		t.Errorf("TraceState kept: %q", out.TraceState)
	}
	if len(out.Events) != 0 || len(out.Links) != 0 {
		t.Errorf("events/links kept: %d/%d", len(out.Events), len(out.Links))
	}
	if out.DroppedEventsCount != uint32(len(in.Events)) {
		t.Errorf("DroppedEventsCount = %d, want %d", out.DroppedEventsCount, len(in.Events))
	}
}

func TestSpanAttributeAllowList(t *testing.T) {
	in := payload.SampleSpan() // 9 attributes incl. error.message, sys.battery, sys.latency
	out := prune.Span(in, prune.Default)

	var keys []string
	for _, kv := range out.Attributes {
		keys = append(keys, kv.Key)
	}
	want := "error.message,sys.battery,sys.latency"
	if got := strings.Join(keys, ","); got != want {
		t.Errorf("kept attributes = %s, want %s (priority order)", got, want)
	}
	if out.DroppedAttributesCount != 6 {
		t.Errorf("DroppedAttributesCount = %d, want 6", out.DroppedAttributesCount)
	}

	// error.message is well over 64 bytes in the sample; it must be cut to 64
	// and still be a prefix of the original.
	original := in.Attributes[8].GetValue().GetStringValue()
	msg := out.Attributes[0].GetValue().GetStringValue()
	if len(msg) != 64 {
		t.Errorf("error.message is %d bytes after pruning, want 64", len(msg))
	}
	if !strings.HasPrefix(original, msg) {
		t.Error("truncated error.message is not a prefix of the original")
	}
}

func TestSpanNoAllowListKeepsFirstN(t *testing.T) {
	out := prune.Span(payload.SampleSpan(), prune.Policy{MaxAttributes: 2})
	if len(out.Attributes) != 2 || out.Attributes[0].Key != "http.request.method" || out.Attributes[1].Key != "http.route" {
		t.Errorf("expected the first two attributes, got %v", out.Attributes)
	}
}

func TestSpanDoesNotMutateInput(t *testing.T) {
	in := payload.SampleSpan()
	before := proto.Clone(in).(*tracepb.Span)

	out := prune.Span(in, prune.Default)
	// Scribble on the output. If prune shared memory with the input, this
	// would change the input too.
	out.Attributes[0].GetValue().GetValue().(*commonpb.AnyValue_StringValue).StringValue = "changed"
	out.TraceId[0] ^= 0xff

	if !proto.Equal(in, before) {
		t.Error("pruning shared memory with the input span")
	}
}

func TestSpanSizeBudget(t *testing.T) {
	in := payload.SampleSpan()
	before := size(t, in)
	after := size(t, prune.Span(in, prune.Default))

	t.Logf("sample span: %d B before, %d B after pruning", before, after)

	const want = 235
	if after != want {
		t.Errorf("pruned span is %d bytes, expected %d (update if the sample or rules changed on purpose)", after, want)
	}
	const loRa = 256
	if after > loRa {
		t.Errorf("pruned span (%d B) does not fit a %d B LoRa frame", after, loRa)
	}
}

func TestTruncateRespectsUTF8(t *testing.T) {
	// "é" is two bytes (0xC3 0xA9). A cut at byte 64 would land between the
	// two bytes of the 33rd rune and leave an invalid trailing byte.
	in := &tracepb.Span{Name: strings.Repeat("é", 40)} // 80 bytes
	out := prune.Span(in, prune.Default)
	if !utf8.ValidString(out.Name) {
		t.Errorf("truncated name is not valid UTF-8: %q", out.Name)
	}
	if len(out.Name) != 64 {
		t.Errorf("truncated name is %d bytes, want 64 (32 two-byte runes)", len(out.Name))
	}
}

func TestTruncateZeroMeansNoLimit(t *testing.T) {
	long := strings.Repeat("x", 500)
	out := prune.Span(&tracepb.Span{Name: long}, prune.Policy{})
	if out.Name != long {
		t.Error("MaxStringBytes 0 should not truncate")
	}
}

func gauge() *metricspb.Metric {
	return &metricspb.Metric{
		Name:        "sys.battery",
		Description: "Battery level as reported by the power management IC, sampled every 30 seconds",
		Unit:        "%",
		Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{
			DataPoints: []*metricspb.NumberDataPoint{{
				TimeUnixNano: 1762336800_000_000_000,
				Value:        &metricspb.NumberDataPoint_AsInt{AsInt: 83},
				Attributes: []*commonpb.KeyValue{
					payload.StringAttr("sys.battery", "main"),
					payload.StringAttr("host.name", "node-7c3e"),
					payload.StringAttr("os.type", "linux"),
					payload.StringAttr("extra", "dropped"),
				},
				Exemplars: []*metricspb.Exemplar{{
					TimeUnixNano: 1762336800_000_000_000,
					SpanId:       []byte{1, 2, 3, 4, 5, 6, 7, 8},
					TraceId:      bytes.Repeat([]byte{9}, 16),
					Value:        &metricspb.Exemplar_AsInt{AsInt: 83},
				}},
			}},
		}},
	}
}

func TestMetricGaugeAllowed(t *testing.T) {
	in := gauge()
	out, err := prune.Metric(in, prune.Default)
	if err != nil {
		t.Fatal(err)
	}
	if out.Description != "" {
		t.Error("description kept")
	}
	pts := out.GetGauge().GetDataPoints()
	if len(pts) != 1 {
		t.Fatalf("got %d data points, want 1", len(pts))
	}
	if len(pts[0].Exemplars) != 0 {
		t.Error("exemplars kept")
	}
	if pts[0].GetAsInt() != 83 {
		t.Errorf("value = %d, want 83", pts[0].GetAsInt())
	}
	// Only sys.battery is on the default allow-list.
	if len(pts[0].Attributes) != 1 || pts[0].Attributes[0].Key != "sys.battery" {
		t.Errorf("attributes = %v", pts[0].Attributes)
	}
	t.Logf("gauge: %d B before, %d B after", size(t, in), size(t, out))
}

func TestMetricSumAllowed(t *testing.T) {
	in := &metricspb.Metric{
		Name: "requests",
		Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{
			IsMonotonic:            true,
			AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
			DataPoints:             []*metricspb.NumberDataPoint{{Value: &metricspb.NumberDataPoint_AsInt{AsInt: 42}}},
		}},
	}
	out, err := prune.Metric(in, prune.Default)
	if err != nil {
		t.Fatal(err)
	}
	if !out.GetSum().GetIsMonotonic() || out.GetSum().GetAggregationTemporality() != in.GetSum().GetAggregationTemporality() {
		t.Error("sum semantics lost")
	}
}

func TestMetricForbiddenTypes(t *testing.T) {
	tests := []struct {
		name string
		m    *metricspb.Metric
	}{
		{"histogram", &metricspb.Metric{Data: &metricspb.Metric_Histogram{Histogram: &metricspb.Histogram{}}}},
		{"exponential histogram", &metricspb.Metric{Data: &metricspb.Metric_ExponentialHistogram{ExponentialHistogram: &metricspb.ExponentialHistogram{}}}},
		{"summary", &metricspb.Metric{Data: &metricspb.Metric_Summary{Summary: &metricspb.Summary{}}}},
		{"no data at all", &metricspb.Metric{Name: "empty"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := prune.Metric(tc.m, prune.Default); !errors.Is(err, prune.ErrMetricType) {
				t.Errorf("error = %v, want ErrMetricType", err)
			}
		})
	}
}

// ExampleSpan prints the before/after picture that Part 4 is built around.
func ExampleSpan() {
	in := payload.SampleSpan()
	out := prune.Span(in, prune.Default)

	_, before, _ := payload.Marshal(in)
	_, after, _ := payload.Marshal(out)

	fmt.Printf("before: %4d bytes, %d attributes, %d events, trace state %d bytes\n",
		len(before), len(in.Attributes), len(in.Events), len(in.TraceState))
	fmt.Printf("after:  %4d bytes, %d attributes, %d events, trace state %d bytes\n",
		len(after), len(out.Attributes), len(out.Events), len(out.TraceState))
	fmt.Printf("dropped: %d attributes, %d events, %d links\n",
		out.DroppedAttributesCount, out.DroppedEventsCount, out.DroppedLinksCount)
	for _, kv := range out.Attributes {
		fmt.Printf("  kept %-13s = %v\n", kv.Key, attrValue(kv))
	}
	// Output:
	// before: 1090 bytes, 9 attributes, 2 events, trace state 39 bytes
	// after:   235 bytes, 3 attributes, 0 events, trace state 0 bytes
	// dropped: 6 attributes, 2 events, 0 links
	//   kept error.message = dial tcp 10.42.0.9:5432: i/o timeout after 10s while fetching re
	//   kept sys.battery   = 83
	//   kept sys.latency   = 10412
}
