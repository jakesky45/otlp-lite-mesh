// Command meshdump reads one OTLP-Lite-Mesh frame and prints it field by
// field, so you can see exactly where every byte sits.
//
//	meshdump frame.bin          # decode a frame from a file
//	cat frame.bin | meshdump    # or from stdin
//	meshdump -demo | meshdump   # generate a sample frame and decode it
//
// With -demo it writes a sample frame's raw bytes to stdout instead, which is
// handy for piping into other tools (xxd, nc, meshdump itself).
package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/jakesky45/otlp-lite-mesh/pkg/frame"
)

func main() {
	demo := flag.Bool("demo", false, "write a sample frame's raw bytes to stdout and exit")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: meshdump [-demo] [file]\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if *demo {
		if err := writeDemo(os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "meshdump:", err)
			os.Exit(1)
		}
		return
	}

	data, err := readInput(flag.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "meshdump:", err)
		os.Exit(1)
	}
	// dump has already printed the error inline with the bytes; just signal
	// failure through the exit code so scripts can tell valid from invalid.
	if err := dump(os.Stdout, data); err != nil {
		os.Exit(1)
	}
}

// readInput returns the whole contents of the named file, or of stdin when
// name is empty.
func readInput(name string) ([]byte, error) {
	if name == "" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(name)
}

// writeDemo encodes a small, recognisable span frame.
func writeDemo(w io.Writer) error {
	b, err := frame.Encode(frame.Header{
		DataType: frame.DataTypeSpan,
		Flags:    frame.FlagCompressed | frame.FlagZstd,
		NodeID: frame.NodeID{
			0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77,
			0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff,
		},
	}, []byte("hello mesh"))
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// dump prints the header one field per line, then the payload as a classic
// hexdump. If the frame is invalid it still prints the raw bytes so you can
// see what went wrong, then returns the decode error.
func dump(w io.Writer, data []byte) error {
	fr, decodeErr := frame.Decode(data)

	fmt.Fprintf(w, "%d bytes total\n\n", len(data))
	fmt.Fprintf(w, "%-6s  %-49s %s\n", "offset", "bytes", "field")

	// Print whatever header bytes exist, even if the frame is too short.
	field := func(off, n int, label string) {
		if off >= len(data) {
			return
		}
		end := min(off+n, len(data))
		fmt.Fprintf(w, "0x%02x    %-49s %s\n", off, hexBytes(data[off:end]), label)
	}

	field(0, 1, fmt.Sprintf("Magic      %s", describeMagic(data)))
	field(1, 1, fmt.Sprintf("Version    %s", describeByte(data, 1)))
	field(2, 1, fmt.Sprintf("DataType   %s", describeDataType(data)))
	field(3, 1, fmt.Sprintf("Flags      %s", describeFlags(data)))
	field(4, 16, fmt.Sprintf("NodeID     %s", describeNodeID(data)))
	field(20, 4, fmt.Sprintf("PayloadLen %s", describePayloadLen(data)))

	if decodeErr != nil {
		fmt.Fprintf(w, "\nINVALID: %v\n", decodeErr)
		if len(data) > frame.HeaderSize {
			fmt.Fprintf(w, "\nremaining %d bytes:\n%s", len(data)-frame.HeaderSize, hex.Dump(data[frame.HeaderSize:]))
		}
		return decodeErr
	}

	fmt.Fprintf(w, "\npayload (%d bytes):\n%s", len(fr.Payload), hex.Dump(fr.Payload))
	return nil
}

func hexBytes(b []byte) string {
	s := make([]byte, 0, len(b)*3)
	for i, c := range b {
		if i > 0 {
			s = append(s, ' ')
		}
		s = append(s, fmt.Sprintf("%02x", c)...)
	}
	return string(s)
}

func describeMagic(data []byte) string {
	if len(data) == 0 {
		return "missing"
	}
	if data[0] == frame.Magic {
		return "'O' ok"
	}
	return fmt.Sprintf("0x%02x, expected 0x%02x", data[0], frame.Magic)
}

func describeByte(data []byte, off int) string {
	if off >= len(data) {
		return "missing"
	}
	return fmt.Sprintf("%d", data[off])
}

func describeDataType(data []byte) string {
	if len(data) < 3 {
		return "missing"
	}
	return frame.DataType(data[2]).String()
}

func describeFlags(data []byte) string {
	if len(data) < 4 {
		return "missing"
	}
	f := frame.Flags(data[3])
	s := fmt.Sprintf("0b%08b %s", uint8(f), f)
	if err := f.Validate(); err != nil {
		// Not errors.Unwrap(err): some Validate errors are bare sentinels
		// with nothing inside, and Unwrap would return nil. Found by piping
		// a frame with Flags=0x02 into meshdump and watching it panic.
		s += " (" + err.Error() + ")"
	}
	return s
}

func describeNodeID(data []byte) string {
	if len(data) < 20 {
		return "truncated"
	}
	var n frame.NodeID
	copy(n[:], data[4:20])
	return n.String()
}

func describePayloadLen(data []byte) string {
	if len(data) < 24 {
		return "truncated"
	}
	n := uint32(data[20])<<24 | uint32(data[21])<<16 | uint32(data[22])<<8 | uint32(data[23])
	return fmt.Sprintf("%d (have %d)", n, len(data)-frame.HeaderSize)
}
