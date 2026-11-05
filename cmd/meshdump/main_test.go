package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/jakesky45/otlp-lite-mesh/pkg/frame"
)

// demoBytes returns the -demo frame as bytes by calling writeDemo with a
// buffer instead of stdout. That is why writeDemo takes an io.Writer.
func demoBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := writeDemo(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestDumpValidFrame(t *testing.T) {
	var out bytes.Buffer
	if err := dump(&out, demoBytes(t)); err != nil {
		t.Fatalf("dump: %v", err)
	}
	for _, want := range []string{
		"34 bytes total",
		"Magic      'O' ok",
		"DataType   span",
		"compressed|zstd",
		"00112233-4455-6677-8899-aabbccddeeff",
		"PayloadLen 10 (have 10)",
		"|hello mesh|",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q\n%s", want, out.String())
		}
	}
}

// TestDumpNeverPanics feeds dump every way a header byte can be wrong. It
// must report INVALID and return an error, never crash. The zstd-without-
// compressed case is the one that originally panicked.
func TestDumpNeverPanics(t *testing.T) {
	demo := demoBytes(t)
	mutate := func(off int, v byte) []byte {
		b := bytes.Clone(demo)
		b[off] = v
		return b
	}
	tests := []struct {
		name string
		in   []byte
	}{
		{"empty", nil},
		{"one byte", demo[:1]},
		{"header only, no payload", demo[:frame.HeaderSize]},
		{"bad magic", mutate(0, 'X')},
		{"bad version", mutate(1, 9)},
		{"unknown data type", mutate(2, 0x7f)},
		{"reserved flag", mutate(3, 0x80)},
		{"zstd without compressed", mutate(3, 0x02)},
		{"payload length lies", mutate(23, 0x20)},
		{"trailing junk", append(bytes.Clone(demo), 'a', 'b', 'c')},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := dump(&out, tc.in)
			if err == nil {
				t.Fatalf("expected an error, output:\n%s", out.String())
			}
			if !strings.Contains(out.String(), "INVALID") {
				t.Errorf("output should say INVALID:\n%s", out.String())
			}
		})
	}
}
