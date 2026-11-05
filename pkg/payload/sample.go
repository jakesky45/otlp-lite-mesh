package payload

import (
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// StringAttr builds a string-valued OTLP attribute.
func StringAttr(key, value string) *commonpb.KeyValue {
	return &commonpb.KeyValue{
		Key:   key,
		Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}},
	}
}

// IntAttr builds an integer-valued OTLP attribute.
func IntAttr(key string, value int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{
		Key:   key,
		Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: value}},
	}
}

// SampleSpan returns a span shaped like one a real SDK would emit for a failed
// HTTP request on a field device: W3C example trace and span IDs, a trace
// state, eight attributes following the OpenTelemetry semantic conventions, an
// exception event with a stack trace, and an error status.
//
// It is the "before" picture for everything that follows. Stage 3 prunes it;
// Stage 4 compresses it; the tests measure each step. It is deterministic so
// that sizes in tests and blog posts stay stable.
func SampleSpan() *tracepb.Span {
	const start = uint64(1762336800_000_000_000) // 2026-11-05T10:00:00Z
	return &tracepb.Span{
		TraceId:      []byte{0x4b, 0xf9, 0x2f, 0x35, 0x77, 0xb3, 0x4d, 0xa6, 0xa3, 0xce, 0x92, 0x9d, 0x0e, 0x0e, 0x47, 0x36},
		SpanId:       []byte{0x00, 0xf0, 0x67, 0xaa, 0x0b, 0xa9, 0x02, 0xb7},
		ParentSpanId: []byte{0xb7, 0xad, 0x6b, 0x71, 0x69, 0x20, 0x33, 0x31},
		TraceState:   "congo=t61rcWkgMzE,rojo=00f067aa0ba902b7",
		Name:         "GET /api/v1/readings/{id}",
		Kind:         tracepb.Span_SPAN_KIND_SERVER,

		StartTimeUnixNano: start,
		EndTimeUnixNano:   start + 10_412_000_000, // 10.4 s, it timed out

		Attributes: []*commonpb.KeyValue{
			StringAttr("http.request.method", "GET"),
			StringAttr("http.route", "/api/v1/readings/{id}"),
			IntAttr("http.response.status_code", 504),
			StringAttr("url.path", "/api/v1/readings/7c3e"),
			StringAttr("user_agent.original", "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Mobile Safari/537.36"),
			StringAttr("network.peer.address", "10.42.0.17"),
			IntAttr("sys.battery", 83),
			IntAttr("sys.latency", 10412),
			StringAttr("error.message", "dial tcp 10.42.0.9:5432: i/o timeout after 10s while fetching reading 7c3e from the sensor store (attempt 3 of 3)"),
		},

		Events: []*tracepb.Span_Event{
			{
				TimeUnixNano: start + 10_400_000_000,
				Name:         "exception",
				Attributes: []*commonpb.KeyValue{
					StringAttr("exception.type", "TimeoutError"),
					StringAttr("exception.message", "dial tcp 10.42.0.9:5432: i/o timeout"),
					StringAttr("exception.stacktrace", "TimeoutError: dial tcp 10.42.0.9:5432: i/o timeout\n"+
						"    at SensorStore.fetch (store.go:142)\n"+
						"    at ReadingsHandler.ServeHTTP (handler.go:88)\n"+
						"    at net/http.(*ServeMux).ServeHTTP (server.go:2683)\n"+
						"    at net/http.serverHandler.ServeHTTP (server.go:3137)"),
				},
			},
			{
				TimeUnixNano: start + 10_410_000_000,
				Name:         "retry.exhausted",
				Attributes:   []*commonpb.KeyValue{IntAttr("retry.attempt", 3)},
			},
		},

		Status: &tracepb.Status{
			Code:    tracepb.Status_STATUS_CODE_ERROR,
			Message: "upstream timeout",
		},
	}
}
