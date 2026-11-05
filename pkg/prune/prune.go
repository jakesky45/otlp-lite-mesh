// Package prune cuts OTLP messages down to what can afford to cross a radio
// (SPEC.md §4). It never modifies its input; every function returns a new
// message, so the caller can still export the full span elsewhere.
//
// For spans the rules are:
//
//   - TraceId, SpanId and ParentSpanId are kept untouched. Lose them and the
//     trace graph breaks (§4.1).
//   - Name, Kind, start/end time, Flags and Status are kept. Long strings are
//     truncated.
//   - TraceState, Events and Links are dropped.
//   - At most Policy.MaxAttributes attributes survive, chosen from
//     Policy.KeepAttributes in priority order. String values are truncated to
//     Policy.MaxStringBytes.
//   - The OTLP Dropped*Count fields record what was removed, so the backend
//     can tell "this span had no events" from "this span's events were cut".
//
// For metrics, only Gauge and Sum data are allowed (§4.2).
package prune

import (
	"bytes"
	"errors"
	"fmt"
	"unicode/utf8"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// ErrMetricType is returned for metric types the spec forbids over the air.
var ErrMetricType = errors.New("prune: metric type not allowed over the mesh")

// Policy says how aggressively to prune. The zero value keeps nothing
// optional; use Default for the spec's recommended settings.
type Policy struct {
	// MaxAttributes is the most attributes a span (or metric data point) may
	// keep. The spec says 3.
	MaxAttributes int

	// KeepAttributes lists attribute keys to keep, highest priority first.
	// Keys not in the list are always dropped. If the list is empty, the first
	// MaxAttributes attributes are kept in their original order.
	KeepAttributes []string

	// MaxStringBytes truncates string values (attribute values, span name,
	// status message) to this many bytes. Zero means no truncation.
	MaxStringBytes int
}

// Default is the policy from SPEC.md §4.1: three attributes, the recommended
// keys, strings cut at 64 bytes.
var Default = Policy{
	MaxAttributes:  3,
	KeepAttributes: []string{"error.message", "sys.battery", "sys.latency"},
	MaxStringBytes: 64,
}

// Span returns a pruned copy of in according to p. The input is not modified.
func Span(in *tracepb.Span, p Policy) *tracepb.Span {
	kept, dropped := p.selectAttributes(in.Attributes)

	return &tracepb.Span{
		// bytes.Clone, not plain assignment: a []byte is a pointer to shared
		// memory, so copying the field alone would let a change to the pruned
		// span's ID alter the original's.
		TraceId:      bytes.Clone(in.TraceId),
		SpanId:       bytes.Clone(in.SpanId),
		ParentSpanId: bytes.Clone(in.ParentSpanId),
		Flags:        in.Flags,
		Name:         truncate(in.Name, p.MaxStringBytes),
		Kind:         in.Kind,

		StartTimeUnixNano: in.StartTimeUnixNano,
		EndTimeUnixNano:   in.EndTimeUnixNano,

		Attributes:             kept,
		DroppedAttributesCount: in.DroppedAttributesCount + dropped,
		DroppedEventsCount:     in.DroppedEventsCount + uint32(len(in.Events)),
		DroppedLinksCount:      in.DroppedLinksCount + uint32(len(in.Links)),

		Status: pruneStatus(in.Status, p),
	}
}

// Metric returns a pruned copy of in, or ErrMetricType if the metric is a
// kind the spec forbids. Exemplars are removed and data point attributes are
// capped with the same rules as span attributes. Description is dropped.
func Metric(in *metricspb.Metric, p Policy) (*metricspb.Metric, error) {
	out := &metricspb.Metric{
		Name: truncate(in.Name, p.MaxStringBytes),
		Unit: in.Unit,
	}

	switch d := in.Data.(type) {
	case *metricspb.Metric_Gauge:
		out.Data = &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{
			DataPoints: p.pruneNumberPoints(d.Gauge.GetDataPoints()),
		}}
	case *metricspb.Metric_Sum:
		out.Data = &metricspb.Metric_Sum{Sum: &metricspb.Sum{
			DataPoints:             p.pruneNumberPoints(d.Sum.GetDataPoints()),
			AggregationTemporality: d.Sum.GetAggregationTemporality(),
			IsMonotonic:            d.Sum.GetIsMonotonic(),
		}}
	case *metricspb.Metric_Histogram:
		return nil, fmt.Errorf("%w: histogram", ErrMetricType)
	case *metricspb.Metric_ExponentialHistogram:
		return nil, fmt.Errorf("%w: exponential histogram", ErrMetricType)
	case *metricspb.Metric_Summary:
		return nil, fmt.Errorf("%w: summary", ErrMetricType)
	default:
		return nil, fmt.Errorf("%w: %T", ErrMetricType, in.Data)
	}
	return out, nil
}

// selectAttributes applies the allow-list and cap, returning the survivors
// (as copies, with strings truncated) and how many were dropped.
func (p Policy) selectAttributes(attrs []*commonpb.KeyValue) (kept []*commonpb.KeyValue, dropped uint32) {
	if len(p.KeepAttributes) == 0 {
		// No allow-list: keep the first MaxAttributes in original order.
		for _, kv := range attrs {
			if len(kept) >= p.MaxAttributes {
				break
			}
			kept = append(kept, p.copyAttribute(kv))
		}
		return kept, uint32(len(attrs) - len(kept))
	}

	// Allow-list: walk it in priority order and take the first match for each
	// key. A map gives O(1) lookup instead of a nested loop.
	byKey := make(map[string]*commonpb.KeyValue, len(attrs))
	for _, kv := range attrs {
		if _, seen := byKey[kv.Key]; !seen {
			byKey[kv.Key] = kv
		}
	}
	for _, key := range p.KeepAttributes {
		if len(kept) >= p.MaxAttributes {
			break
		}
		if kv, ok := byKey[key]; ok {
			kept = append(kept, p.copyAttribute(kv))
		}
	}
	return kept, uint32(len(attrs) - len(kept))
}

// copyAttribute clones kv so the pruned message shares nothing with the
// input, then truncates a string value if the policy asks for it.
func (p Policy) copyAttribute(kv *commonpb.KeyValue) *commonpb.KeyValue {
	out := proto.Clone(kv).(*commonpb.KeyValue)
	if s, ok := out.GetValue().GetValue().(*commonpb.AnyValue_StringValue); ok {
		s.StringValue = truncate(s.StringValue, p.MaxStringBytes)
	}
	return out
}

func (p Policy) pruneNumberPoints(points []*metricspb.NumberDataPoint) []*metricspb.NumberDataPoint {
	out := make([]*metricspb.NumberDataPoint, 0, len(points))
	for _, pt := range points {
		kept, _ := p.selectAttributes(pt.Attributes)
		out = append(out, &metricspb.NumberDataPoint{
			Attributes:        kept,
			StartTimeUnixNano: pt.StartTimeUnixNano,
			TimeUnixNano:      pt.TimeUnixNano,
			Value:             pt.Value,
			Flags:             pt.Flags,
			// Exemplars deliberately dropped: each carries a trace ID, span ID
			// and its own attributes.
		})
	}
	return out
}

func pruneStatus(s *tracepb.Status, p Policy) *tracepb.Status {
	if s == nil {
		return nil
	}
	return &tracepb.Status{
		Code:    s.Code,
		Message: truncate(s.Message, p.MaxStringBytes),
	}
}

// truncate cuts s to at most max bytes without splitting a UTF-8 character.
// Go strings are byte sequences, so s[:max] alone could cut a multi-byte rune
// in half and produce invalid text. max <= 0 means no limit.
func truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
