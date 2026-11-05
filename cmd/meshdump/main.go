// Command meshdump reads one OTLP-Lite-Mesh frame and prints it field by
// field, so you can see exactly where every byte sits.
//
//	meshdump frame.bin          # decode a frame from a file
//	cat frame.bin | meshdump    # or from stdin
//	meshdump -demo | meshdump   # generate a sample frame and decode it
//
// With -demo it writes a sample frame's raw bytes to stdout instead, which is
// handy for piping into other tools (xxd, nc, meshdump itself).
//
// LEARN: the comment block directly above "package" is the package's
// documentation. `go doc ./cmd/meshdump` prints it. For programs the
// convention is to start with "Command <name> ...".
package main

// LEARN: "package main" is the one package name with special meaning: it tells
// the compiler to build an executable that starts at func main(). Library
// packages (pkg/frame, pkg/payload...) use any other name and have no main.

// LEARN: imports come in two groups separated by a blank line: standard
// library first, then our own module. gofmt sorts each group. An import that
// is not used is a compile error, not a warning. We hit that when "errors"
// stopped being used and had to be removed.
import (
	"encoding/hex" // hex.Dump: the classic 16-bytes-per-row hexdump
	"flag"         // command-line flags like -demo
	"fmt"          // formatted printing
	"io"           // io.Writer, io.ReadAll
	"os"           // os.Stdin, os.Stdout, os.Exit, os.ReadFile

	"github.com/jakesky45/otlp-lite-mesh/pkg/frame" // our own package, imported by full module path
)

func main() {
	// LEARN: flag.Bool registers a "-demo" flag and returns a *bool, a POINTER
	// to a bool, not the bool itself. It has to be a pointer: the value isn't
	// known until flag.Parse() runs below and fills it in. That is why we read
	// it as *demo (the * means "the value this pointer points at").
	demo := flag.Bool("demo", false, "write a sample frame's raw bytes to stdout and exit")

	// LEARN: flag.Usage is a package-level variable that holds a function.
	// We replace it with our own anonymous function (a function with no name,
	// written inline) so `meshdump -h` prints a one-line usage before the
	// flag list. Functions are ordinary values in Go: you can store them in
	// variables, pass them as arguments, return them.
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: meshdump [-demo] [file]\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if *demo {
		// LEARN: functions that can fail return an error as their last result.
		// nil means "no error". So: run the function, keep the error, test it.
		err := writeDemo(os.Stdout)
		if err != nil {
			fmt.Fprintln(os.Stderr, "meshdump:", err)
			// LEARN: os.Exit(1) ends the process with a non-zero code so shells
			// and scripts know it failed. A normal "return" from main is exit 0.
			os.Exit(1)
		}
		// LEARN: most Go code squashes those two lines into one:
		//     if err := writeDemo(os.Stdout); err != nil { ... }
		// The part before the ";" runs first, the part after is the condition.
		// It means exactly the same thing; err just stops existing after the
		// block. You'll see the short form everywhere, so it's worth getting
		// used to, but the long form is fine and we'll use it for now.
		return
	}

	// LEARN: flag.Arg(0) is the first argument AFTER the flags, here an
	// optional filename. It returns "" if there isn't one. Two return values
	// (result, error) is how nearly every Go function that can fail is shaped.
	data, err := readInput(flag.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "meshdump:", err)
		os.Exit(1)
	}
	// dump has already printed the error inline with the bytes; just signal
	// failure through the exit code so scripts can tell valid from invalid.
	//
	// LEARN: "=" not ":=" here, because err was already declared by the
	// readInput line above. := declares a new variable; = assigns to an
	// existing one. Mixing them up is a compile error you'll meet often.
	err = dump(os.Stdout, data)
	if err != nil {
		os.Exit(1)
	}
}

// readInput returns the whole contents of the named file, or of stdin when
// name is empty.
func readInput(name string) ([]byte, error) {
	// LEARN: io.ReadAll and os.ReadFile both return ([]byte, error), the same
	// shape as this function, so we can return their result directly without
	// unpacking it into variables first.
	if name == "" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(name)
}

// writeDemo encodes a small, recognisable span frame.
//
// LEARN: this is the most important design choice in the file. The parameter
// is io.Writer, an INTERFACE with a single method: Write([]byte) (int, error).
// Anything with that method satisfies it automatically, no declaration
// needed: os.Stdout, a bytes.Buffer, a file, a network connection. So main
// can pass the real stdout and the test (main_test.go) can pass a buffer and
// inspect what was written. Same function, no "test mode".
func writeDemo(w io.Writer) error {
	b, err := frame.Encode(frame.Header{
		DataType: frame.DataTypeSpan,
		Flags:    frame.FlagCompressed | frame.FlagZstd, // LEARN: | is bitwise OR, combining two single-bit flags into 0b00000011
		// LEARN: an array literal for a [16]byte. Exactly 16 values. Give it
		// 15 and the compiler zero-fills the rest; give it 17 and it refuses.
		NodeID: frame.NodeID{
			0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77,
			0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff,
		},
	}, []byte("hello mesh")) // LEARN: []byte("...") converts a string to its bytes
	if err != nil {
		return err
	}
	// LEARN: Write returns (bytes written, error). We don't need the count, so
	// _ throws it away. Note "=" not ":=": err already exists from above, so
	// this is assignment to an existing variable, not a new declaration.
	_, err = w.Write(b)
	return err
}

// dump prints the header one field per line, then the payload as a classic
// hexdump. If the frame is invalid it still prints the raw bytes so you can
// see what went wrong, then returns the decode error.
func dump(w io.Writer, data []byte) error {
	// LEARN: decode first but do NOT return on failure. The whole point of the
	// tool is to show the bytes even when they are wrong, so the error is held
	// in decodeErr and used at the end.
	fr, decodeErr := frame.Decode(data)

	// LEARN: Fprintf = "print formatted to a writer". %d is a decimal number,
	// %s a string. %-49s means: string, padded to 49 characters, left-aligned
	// (the minus). That is what lines the columns up.
	fmt.Fprintf(w, "%d bytes total\n\n", len(data))
	fmt.Fprintf(w, "%-6s  %-49s %s\n", "offset", "bytes", "field")

	// Print whatever header bytes exist, even if the frame is too short.
	//
	// LEARN: this is a CLOSURE: an anonymous function stored in a local
	// variable. It can see w and data from the enclosing function without
	// being handed them as parameters. We use it to print one row and call
	// it six times below.
	field := func(off, n int, label string) {
		if off >= len(data) {
			return
		}
		// LEARN: min is a built-in (since Go 1.21). It keeps data[off:end] in
		// bounds on a short frame; slicing past the end of a slice panics.
		end := min(off+n, len(data))
		// LEARN: 0x%02x prints the offset as two hex digits with a 0x prefix.
		// data[off:end] is a slice expression: a view of those bytes, no copy.
		fmt.Fprintf(w, "0x%02x    %-49s %s\n", off, hexBytes(data[off:end]), label)
	}

	// LEARN: fmt.Sprintf is Fprintf's sibling that returns the string instead
	// of writing it. Each describeX helper turns raw bytes into meaning.
	field(0, 1, fmt.Sprintf("Magic      %s", describeMagic(data)))
	field(1, 1, fmt.Sprintf("Version    %s", describeByte(data, 1)))
	field(2, 1, fmt.Sprintf("DataType   %s", describeDataType(data)))
	field(3, 1, fmt.Sprintf("Flags      %s", describeFlags(data)))
	field(4, 16, fmt.Sprintf("NodeID     %s", describeNodeID(data)))
	field(20, 4, fmt.Sprintf("PayloadLen %s", describePayloadLen(data)))

	if decodeErr != nil {
		// LEARN: %v prints any value in its default form. For an error that is
		// its message.
		fmt.Fprintf(w, "\nINVALID: %v\n", decodeErr)
		if len(data) > frame.HeaderSize {
			// LEARN: hex.Dump is the standard library's hexdump, same layout
			// as `hexdump -C`: offset, 16 bytes, ASCII column.
			fmt.Fprintf(w, "\nremaining %d bytes:\n%s", len(data)-frame.HeaderSize, hex.Dump(data[frame.HeaderSize:]))
		}
		return decodeErr
	}

	// LEARN: fr is a frame.Frame, which EMBEDS frame.Header, so fr.Payload
	// and (if we wanted) fr.DataType both work without writing fr.Header.X.
	fmt.Fprintf(w, "\npayload (%d bytes):\n%s", len(fr.Payload), hex.Dump(fr.Payload))
	return nil
}

// hexBytes renders bytes as "4f 01 03", space separated.
func hexBytes(b []byte) string {
	// LEARN: make([]byte, 0, n) creates an EMPTY slice (length 0) with room
	// for n bytes already reserved (capacity n). append then never has to
	// reallocate. Each byte becomes 2 hex digits + 1 space, hence *3.
	s := make([]byte, 0, len(b)*3)
	// LEARN: range over a slice yields (index, value) on each iteration.
	for i, c := range b {
		if i > 0 {
			s = append(s, ' ')
		}
		// LEARN: the trailing ... spreads the string's bytes into append's
		// variadic arguments. Without it you'd be appending a string to a
		// []byte, which does not type-check.
		s = append(s, fmt.Sprintf("%02x", c)...)
	}
	// LEARN: string(s) converts the byte slice to a string, once, at the end.
	return string(s)
}

// LEARN: every describe helper below has the same shape: GUARD, then
// INTERPRET. The guard checks the bytes exist before indexing. Go panics on an
// out-of-range index rather than reading garbage memory; two of these helpers
// originally skipped the guard and crashed on bad input. Found by piping
// broken frames into meshdump; fixed and covered by TestDumpNeverPanics.

func describeMagic(data []byte) string {
	if len(data) == 0 { // the guard that was missing
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
	// LEARN: frame.DataType(data[2]) converts a plain byte to our named type.
	// It costs nothing at runtime; it just tells the compiler what the byte
	// means, which is what lets us call .String() on it.
	return frame.DataType(data[2]).String()
}

func describeFlags(data []byte) string {
	if len(data) < 4 {
		return "missing"
	}
	f := frame.Flags(data[3])
	// LEARN: the same value printed two ways. uint8(f) with %08b gives the raw
	// bits, zero-padded to 8 digits. f with %s calls our Flags.String() method
	// and gives "compressed|zstd". Same byte, two renderings.
	s := fmt.Sprintf("0b%08b %s", uint8(f), f)
	if err := f.Validate(); err != nil {
		// Not errors.Unwrap(err): some Validate errors are bare sentinels
		// with nothing inside, and Unwrap would return nil. Found by piping
		// a frame with Flags=0x02 into meshdump and watching it panic.
		//
		// LEARN: calling a method on a nil interface value panics with "nil
		// pointer dereference". Always ask whether a value can be nil before
		// calling methods on it.
		s += " (" + err.Error() + ")"
	}
	return s
}

func describeNodeID(data []byte) string {
	if len(data) < 20 {
		return "truncated"
	}
	// LEARN: var n frame.NodeID declares a zero-filled [16]byte array. n[:]
	// makes the array usable as a slice so copy can write into it. Then we
	// borrow the array's String() method for the UUID formatting.
	var n frame.NodeID
	copy(n[:], data[4:20])
	return n.String()
}

func describePayloadLen(data []byte) string {
	if len(data) < 24 {
		return "truncated"
	}
	// LEARN: big-endian by hand. Shift byte 20 up 24 bits, byte 21 up 16,
	// byte 22 up 8, and OR them together. This is exactly what
	// binary.BigEndian.Uint32 does; it is spelt out here so you see the
	// arithmetic once. The frame package uses the library call.
	n := uint32(data[20])<<24 | uint32(data[21])<<16 | uint32(data[22])<<8 | uint32(data[23])
	return fmt.Sprintf("%d (have %d)", n, len(data)-frame.HeaderSize)
}
