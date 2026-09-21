// Package ulid mints run identifiers.
//
// A run id is the root of the idempotency key for every money request the CLI
// sends: the keys are <run>-simulate and <run>-execute (PLAN §5.3). Two runs
// that share an id would share a key, and two different transfers under one
// key is the failure the whole ledger exists to prevent — so the entropy comes
// from crypto/rand and nowhere else (AC5).
package ulid

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// Length is the character length of the Crockford base32 encoding of a ULID:
// 48 bits of timestamp and 80 bits of entropy over a 5-bit alphabet.
const Length = 26

// encoding is Crockford base32: the digits and the uppercase letters, less
// I, L, O and U.
const encoding = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// decoding maps a byte to its value, or 0xFF if it is not in the alphabet.
var decoding = func() [256]byte {
	var t [256]byte
	for i := range t {
		t[i] = 0xFF
	}
	for i := 0; i < len(encoding); i++ {
		t[encoding[i]] = byte(i)
	}
	return t
}()

// ErrInvalid reports a string that is not a ULID.
var ErrInvalid = errors.New("ulid: not a 26-character Crockford ULID")

// Generator mints ULIDs that increase strictly within its own lifetime.
//
// Monotonicity is not cosmetic. Within one millisecond the entropy is
// incremented rather than redrawn, so two ids from one generator can never
// collide even if the clock stands still; and because the recorded timestamp
// never goes backwards, a clock that is stepped backwards (ntp, a suspended
// laptop, a container clock) cannot make a new run id sort before — or equal —
// one this process already handed out.
type Generator struct {
	mu      sync.Mutex
	entropy io.Reader
	now     func() time.Time

	lastMS   uint64
	lastRand [10]byte
	seeded   bool
}

// NewGenerator returns a generator reading entropy from r and time from now.
// Both are injectable so a test can prove what happens when they misbehave;
// production uses [New], which is crypto/rand and the wall clock.
func NewGenerator(r io.Reader, now func() time.Time) *Generator {
	if r == nil {
		r = rand.Reader
	}
	if now == nil {
		now = time.Now
	}
	return &Generator{entropy: r, now: now}
}

var defaultGenerator = NewGenerator(rand.Reader, time.Now)

// New mints a run id from the process-wide generator.
func New() (string, error) { return defaultGenerator.New() }

// New mints the next id.
func (g *Generator) New() (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	ms := uint64(g.now().UnixMilli())
	switch {
	case !g.seeded || ms > g.lastMS:
		// A new millisecond: draw fresh entropy.
		if _, err := io.ReadFull(g.entropy, g.lastRand[:]); err != nil {
			return "", fmt.Errorf("ulid: entropy: %w", err)
		}
		g.lastMS = ms
		g.seeded = true
	default:
		// Same millisecond, or the clock went backwards. Hold the timestamp at
		// the highest value seen and step the entropy, which keeps the
		// sequence strictly increasing either way.
		if err := increment(&g.lastRand); err != nil {
			// 2^80 ids inside one millisecond is not reachable; if it were,
			// refusing is the only safe answer, because wrapping would repeat
			// an id this process has already used as an idempotency key.
			return "", err
		}
	}

	return encode(g.lastMS, g.lastRand), nil
}

// errEntropyExhausted is unreachable in practice and is refused rather than
// wrapped: a wrapped counter would repeat a key.
var errEntropyExhausted = errors.New("ulid: entropy exhausted within a millisecond")

func increment(b *[10]byte) error {
	for i := len(b) - 1; i >= 0; i-- {
		b[i]++
		if b[i] != 0 {
			return nil
		}
	}
	return errEntropyExhausted
}

func encode(ms uint64, ent [10]byte) string {
	// 16 bytes: 6 of timestamp (big endian) then 10 of entropy, rendered as 26
	// base32 characters. The first character carries only 3 significant bits.
	var raw [16]byte
	raw[0] = byte(ms >> 40)
	raw[1] = byte(ms >> 32)
	raw[2] = byte(ms >> 24)
	raw[3] = byte(ms >> 16)
	raw[4] = byte(ms >> 8)
	raw[5] = byte(ms)
	copy(raw[6:], ent[:])

	var out [Length]byte
	out[0] = encoding[(raw[0]&224)>>5]
	out[1] = encoding[raw[0]&31]
	out[2] = encoding[(raw[1]&248)>>3]
	out[3] = encoding[((raw[1]&7)<<2)|((raw[2]&192)>>6)]
	out[4] = encoding[(raw[2]&62)>>1]
	out[5] = encoding[((raw[2]&1)<<4)|((raw[3]&240)>>4)]
	out[6] = encoding[((raw[3]&15)<<1)|((raw[4]&128)>>7)]
	out[7] = encoding[(raw[4]&124)>>2]
	out[8] = encoding[((raw[4]&3)<<3)|((raw[5]&224)>>5)]
	out[9] = encoding[raw[5]&31]
	out[10] = encoding[(raw[6]&248)>>3]
	out[11] = encoding[((raw[6]&7)<<2)|((raw[7]&192)>>6)]
	out[12] = encoding[(raw[7]&62)>>1]
	out[13] = encoding[((raw[7]&1)<<4)|((raw[8]&240)>>4)]
	out[14] = encoding[((raw[8]&15)<<1)|((raw[9]&128)>>7)]
	out[15] = encoding[(raw[9]&124)>>2]
	out[16] = encoding[((raw[9]&3)<<3)|((raw[10]&224)>>5)]
	out[17] = encoding[raw[10]&31]
	out[18] = encoding[(raw[11]&248)>>3]
	out[19] = encoding[((raw[11]&7)<<2)|((raw[12]&192)>>6)]
	out[20] = encoding[(raw[12]&62)>>1]
	out[21] = encoding[((raw[12]&1)<<4)|((raw[13]&240)>>4)]
	out[22] = encoding[((raw[13]&15)<<1)|((raw[14]&128)>>7)]
	out[23] = encoding[(raw[14]&124)>>2]
	out[24] = encoding[((raw[14]&3)<<3)|((raw[15]&224)>>5)]
	out[25] = encoding[raw[15]&31]
	return string(out[:])
}

// Valid reports whether s is a 26-character Crockford ULID this package would
// have produced. It is strict: lowercase and the ambiguous letters Crockford
// allows on input (I, L, O) are refused, because a run id is a filename and a
// map key and two spellings of one id would be two runs.
func Valid(s string) bool {
	if len(s) != Length {
		return false
	}
	for i := 0; i < len(s); i++ {
		if decoding[s[i]] == 0xFF {
			return false
		}
	}
	// The timestamp is 48 bits and the first character holds 3 of them, so a
	// leading character above '7' would overflow.
	return s[0] <= '7'
}

// Timestamp recovers the millisecond a ULID was minted at.
func Timestamp(s string) (time.Time, error) {
	if !Valid(s) {
		return time.Time{}, ErrInvalid
	}
	var ms uint64
	for i := 0; i < 10; i++ {
		ms = ms<<5 | uint64(decoding[s[i]])
	}
	return time.UnixMilli(int64(ms)).UTC(), nil
}
