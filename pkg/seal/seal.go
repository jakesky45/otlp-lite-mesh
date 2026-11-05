// Package seal encrypts and authenticates frames (SPEC.md errata E7). It is
// the replacement for the TLS the spec threw away, costing 20 bytes per frame
// and no handshake.
//
// A sealed frame looks like this on the wire:
//
//	24-byte header (clear, authenticated)  Flags has FlagSealed set
//	 4-byte counter (clear, authenticated)  per-node, strictly increasing
//	 N-byte ciphertext                      the compressed/pruned payload
//	16-byte tag                             proves nothing above was altered
//
// The header is passed to the cipher as "associated data": it is not
// encrypted, so relays can still route on it, but it is covered by the tag,
// so changing a single header bit makes the frame fail to open.
//
// Keys: one 32-byte master key per mesh. Each node's key is derived from it
// with HKDF and the node's ID, so compromising one device does not expose
// the others' traffic, and the gateway can derive any node's key on demand.
//
// Nonces: ChaCha20-Poly1305 needs a 12-byte nonce that is never reused with
// the same key. Ours is NodeID[0:8] followed by the counter, so it is unique
// per node for 2^32 frames. The Sealer refuses to continue past that; rotate
// the master key before it happens.
//
// Replay: the Opener remembers, per node, the highest counter seen and a
// 64-frame window below it. A frame is accepted if its counter is new; a
// repeat, or one older than the window, is rejected. UDP may reorder packets,
// so a strict "must be greater than last" rule would drop legitimate traffic.
//
// LEARN: this package uses Go's crypto in the recommended way: never
// implement a primitive yourself, compose the standard ones. cipher.AEAD is
// the interface, chacha20poly1305 is one implementation, hkdf derives keys,
// crypto/rand makes them. Everything here is glue.
package seal

import (
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sync"

	"golang.org/x/crypto/chacha20poly1305"

	"github.com/jakesky45/otlp-lite-mesh/pkg/frame"
)

// Sizes, in bytes.
const (
	KeySize     = chacha20poly1305.KeySize // 32
	counterSize = 4
	TagSize     = chacha20poly1305.Overhead // 16

	// Overhead is what sealing adds to a payload: the clear counter plus the tag.
	Overhead = counterSize + TagSize // 20

	// windowSize is how far behind the newest counter a frame may arrive and
	// still be accepted, for UDP reordering. 64 fits in one uint64 bitmap.
	windowSize = 64
)

// Errors returned by Open. Compare with errors.Is.
var (
	ErrUnsealed         = errors.New("seal: frame is not sealed and the receiver requires it")
	ErrTooShort         = errors.New("seal: sealed payload shorter than counter + tag")
	ErrAuth             = errors.New("seal: authentication failed (wrong key, or header/payload tampered)")
	ErrReplay           = errors.New("seal: counter already seen or too old (replay)")
	ErrCounterExhausted = errors.New("seal: node counter exhausted, rotate the master key")
	ErrBadKey           = errors.New("seal: master key must be 32 bytes of hex")
)

// MasterKey is the mesh-wide secret every node and gateway shares.
//
// LEARN: an array, not a slice, for the same reason as frame.NodeID: fixed
// size, copied by value, comparable. It also means a MasterKey can never be
// the wrong length; the type guarantees it.
type MasterKey [KeySize]byte

// NewMasterKey makes a random key. crypto/rand is the operating system's
// secure random source; math/rand must never be used for keys.
func NewMasterKey() (MasterKey, error) {
	var k MasterKey
	if _, err := rand.Read(k[:]); err != nil {
		return MasterKey{}, fmt.Errorf("seal: generating key: %w", err)
	}
	return k, nil
}

// ParseMasterKey reads a key from 64 hex characters, the form it takes in an
// environment variable or a secrets file.
func ParseMasterKey(s string) (MasterKey, error) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != KeySize {
		return MasterKey{}, ErrBadKey
	}
	var k MasterKey
	copy(k[:], b)
	return k, nil
}

// String renders the key as hex. Be careful where you print it.
func (k MasterKey) String() string { return hex.EncodeToString(k[:]) }

// NodeKey derives the per-node key from the master key and the node's ID.
//
// LEARN: HKDF is "HMAC-based key derivation": a standard way to turn one
// secret into many unrelated ones. The info string binds the result to this
// protocol and this node, so the same master key used elsewhere produces
// different keys. hkdf.Key is in the standard library since Go 1.24.
func NodeKey(master MasterKey, node frame.NodeID) ([]byte, error) {
	info := append([]byte("otlp-lite-mesh/v1/node-key/"), node[:]...)
	key, err := hkdf.Key(sha256.New, master[:], nil, string(info), KeySize)
	if err != nil {
		return nil, fmt.Errorf("seal: deriving node key: %w", err)
	}
	return key, nil
}

func newAEAD(master MasterKey, node frame.NodeID) (cipher.AEAD, error) {
	key, err := NodeKey(master, node)
	if err != nil {
		return nil, err
	}
	return chacha20poly1305.New(key)
}

// nonce builds the 12-byte nonce: 8 bytes of node ID, 4 bytes of counter.
func nonce(node frame.NodeID, counter uint32) []byte {
	n := make([]byte, chacha20poly1305.NonceSize)
	copy(n[:8], node[:8])
	binary.BigEndian.PutUint32(n[8:], counter)
	return n
}

// Sealer produces sealed frames for one node. It is not safe for concurrent
// use; a node has one radio and one Sealer.
type Sealer struct {
	aead    cipher.AEAD
	node    frame.NodeID
	counter uint32 // the next counter to use
}

// NewSealer derives the node's key and starts its counter at zero.
//
// LEARN: returning *Sealer (a pointer) rather than Sealer is deliberate: the
// Sealer has state (the counter) that Seal must change, and a method can only
// change the caller's copy through a pointer.
func NewSealer(master MasterKey, node frame.NodeID) (*Sealer, error) {
	aead, err := newAEAD(master, node)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead, node: node}, nil
}

// Seal encrypts plaintext and returns a complete frame: header plus sealed
// payload. It takes the whole frame's worth of header because the header
// bytes are authenticated, so they must be fixed before the tag is computed.
// h.NodeID is overridden with the Sealer's node; h.PayloadLen is computed.
func (s *Sealer) Seal(h frame.Header, plaintext []byte) ([]byte, error) {
	if s.counter == math.MaxUint32 {
		return nil, ErrCounterExhausted
	}
	h.NodeID = s.node
	h.Flags |= frame.FlagSealed

	// Lay out the frame with an empty payload of the final size, so the
	// header bytes (including PayloadLen) are exactly what will be sent.
	sealedLen := len(plaintext) + Overhead
	out, err := frame.Encode(h, make([]byte, sealedLen))
	if err != nil {
		return nil, err
	}
	header := out[:frame.HeaderSize]

	counter := s.counter
	s.counter++
	binary.BigEndian.PutUint32(out[frame.HeaderSize:], counter)

	// LEARN: aead.Seal(dst, nonce, plaintext, additionalData) APPENDS the
	// ciphertext and tag to dst and returns the extended slice. Passing
	// out[:header+counter] as dst makes it write straight into the frame
	// buffer after the counter: no second allocation, no copy.
	prefix := out[:frame.HeaderSize+counterSize]
	out = s.aead.Seal(prefix, nonce(s.node, counter), plaintext, header)
	return out, nil
}

// Opener verifies and decrypts frames from any node in the mesh. It is safe
// for concurrent use: a gateway opens frames from many nodes at once.
type Opener struct {
	// RequireSealed rejects frames that are not sealed. It is true by default
	// (errata E7); set it to false only for tests and local experiments.
	RequireSealed bool

	master MasterKey

	// LEARN: a sync.Mutex guards the two maps below. Maps in Go are not safe
	// for concurrent writes; the UDP receiver will call Open from several
	// goroutines, so every access to the maps happens with the lock held.
	mu      sync.Mutex
	aeads   map[frame.NodeID]cipher.AEAD
	windows map[frame.NodeID]*window
}

// NewOpener makes an Opener that requires sealing.
func NewOpener(master MasterKey) *Opener {
	return &Opener{
		RequireSealed: true,
		master:        master,
		aeads:         make(map[frame.NodeID]cipher.AEAD),
		windows:       make(map[frame.NodeID]*window),
	}
}

// Open decodes a raw frame, and if it is sealed, verifies and decrypts it.
// The returned Frame has the plaintext payload, FlagSealed cleared and
// PayloadLen set to the plaintext length, so later stages see an ordinary
// frame. An unsealed frame is returned as-is when RequireSealed is false.
func (o *Opener) Open(raw []byte) (frame.Frame, error) {
	fr, err := frame.Decode(raw)
	if err != nil {
		return frame.Frame{}, err
	}
	if !fr.Flags.Sealed() {
		if o.RequireSealed {
			return frame.Frame{}, ErrUnsealed
		}
		return fr, nil
	}
	if len(fr.Payload) < Overhead {
		return frame.Frame{}, fmt.Errorf("%w: %d bytes", ErrTooShort, len(fr.Payload))
	}

	counter := binary.BigEndian.Uint32(fr.Payload[:counterSize])
	ciphertext := fr.Payload[counterSize:]
	header := raw[:frame.HeaderSize]

	aead, err := o.aeadFor(fr.NodeID)
	if err != nil {
		return frame.Frame{}, err
	}
	plaintext, err := aead.Open(nil, nonce(fr.NodeID, counter), ciphertext, header)
	if err != nil {
		// LEARN: the library's error is deliberately vague ("message
		// authentication failed") and we keep it that way. Telling an
		// attacker whether the key or the tag was wrong helps them.
		return frame.Frame{}, ErrAuth
	}

	// Replay is checked AFTER authentication, so a forged frame with a
	// huge counter cannot poison the window and block the real node.
	if !o.accept(fr.NodeID, counter) {
		return frame.Frame{}, fmt.Errorf("%w: counter %d", ErrReplay, counter)
	}

	fr.Payload = plaintext
	fr.Flags &^= frame.FlagSealed // LEARN: &^ is "AND NOT": clear these bits
	fr.PayloadLen = uint32(len(plaintext))
	return fr, nil
}

// aeadFor returns the cached cipher for a node, deriving it on first sight.
func (o *Opener) aeadFor(node frame.NodeID) (cipher.AEAD, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if a, ok := o.aeads[node]; ok {
		return a, nil
	}
	a, err := newAEAD(o.master, node)
	if err != nil {
		return nil, err
	}
	o.aeads[node] = a
	return a, nil
}

// accept records counter for node and reports whether it was fresh.
func (o *Opener) accept(node frame.NodeID, counter uint32) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	w, ok := o.windows[node]
	if !ok {
		w = &window{}
		o.windows[node] = w
	}
	return w.check(counter)
}

// window is a sliding anti-replay window in the style of IPsec (RFC 4303):
// the highest counter seen, and one bit per counter in the 64 below it.
//
// LEARN: bit manipulation. bits is a uint64 used as 64 booleans. Bit 0 is
// "max itself was seen", bit k is "max-k was seen". Shifting left by n moves
// the window up by n counters; the bits that fall off the top are forgotten.
type window struct {
	started bool
	max     uint32
	bits    uint64
}

func (w *window) check(n uint32) bool {
	if !w.started {
		w.started, w.max, w.bits = true, n, 1
		return true
	}
	if n > w.max {
		shift := n - w.max
		if shift >= windowSize {
			w.bits = 1 // jumped so far that nothing old is still in range
		} else {
			w.bits = w.bits<<shift | 1
		}
		w.max = n
		return true
	}
	diff := w.max - n
	if diff >= windowSize {
		return false // older than the window: cannot tell, so reject
	}
	mask := uint64(1) << diff
	if w.bits&mask != 0 {
		return false // already seen
	}
	w.bits |= mask
	return true
}
