package ulid_test

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coba-ai/ferry-cli/internal/ulid"
)

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// AC5: run ids are 26-character Crockford ULIDs from crypto/rand, monotonic
// within a process, and two independent generators over 10,000 draws produce
// no collision.
//
// The two-generator shape is what makes this a control rather than a spelling
// check: one generator cannot collide with itself even from a fixed seed,
// because the monotonic step guarantees distinctness. Two generators share
// nothing but the entropy source, so replacing crypto/rand with a seeded
// math/rand (mutation M67) makes them produce the same sequence and collide on
// the first draw.
//
// Both generators are built with a nil source so they take the package's own
// entropy. Passing rand.Reader here would make the test assert that
// crypto/rand does not collide — true, and not about this package: M67
// changes which source the package reaches for, and a test that supplies the
// source cannot see that change.
func TestTwoGeneratorsDoNotCollideOver10000Draws(t *testing.T) {
	const draws = 10_000

	a := ulid.NewGenerator(nil, nil)
	b := ulid.NewGenerator(nil, nil)

	seen := make(map[string]string, draws*2)
	for i := 0; i < draws; i++ {
		for name, g := range map[string]*ulid.Generator{"a": a, "b": b} {
			id, err := g.New()
			if err != nil {
				t.Fatalf("generator %s draw %d: %v", name, i, err)
			}
			if prev, dup := seen[id]; dup {
				t.Fatalf("collision at draw %d: %s minted by %s and %s", i, id, prev, name)
			}
			seen[id] = name
		}
	}
	if len(seen) != draws*2 {
		t.Fatalf("recorded %d ids, want %d", len(seen), draws*2)
	}
}

// The collision detector above must be able to fire. A generator reading from
// a fixed byte stream is exactly what math/rand seeded 0 would be.
func TestCollisionDetectorFiresOnASharedDeterministicSource(t *testing.T) {
	clock := func() time.Time { return time.UnixMilli(1_700_000_000_000) }
	a := ulid.NewGenerator(bytes.NewReader(bytes.Repeat([]byte{7}, 64)), clock)
	b := ulid.NewGenerator(bytes.NewReader(bytes.Repeat([]byte{7}, 64)), clock)

	ida, err := a.New()
	if err != nil {
		t.Fatal(err)
	}
	idb, err := b.New()
	if err != nil {
		t.Fatal(err)
	}
	if ida != idb {
		t.Fatalf("two generators on one deterministic source produced %s and %s; the collision test above would not fire on M67", ida, idb)
	}
}

func TestShapeIs26CrockfordCharacters(t *testing.T) {
	for i := 0; i < 500; i++ {
		id, err := ulid.New()
		if err != nil {
			t.Fatal(err)
		}
		if len(id) != 26 {
			t.Fatalf("%q is %d characters, want 26", id, len(id))
		}
		for _, r := range id {
			if !strings.ContainsRune(crockford, r) {
				t.Fatalf("%q contains %q, which is not in the Crockford alphabet", id, r)
			}
		}
		if !ulid.Valid(id) {
			t.Fatalf("Valid rejects a freshly minted id %q", id)
		}
	}
}

func TestMonotonicWithinAMillisecond(t *testing.T) {
	frozen := time.UnixMilli(1_700_000_000_000)
	g := ulid.NewGenerator(rand.Reader, func() time.Time { return frozen })

	prev, err := g.New()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1000; i++ {
		id, err := g.New()
		if err != nil {
			t.Fatal(err)
		}
		if id <= prev {
			t.Fatalf("draw %d: %s does not sort after %s", i, id, prev)
		}
		prev = id
	}
}

// A clock stepped backwards must not be able to produce an id this process has
// already handed out, because that id may already be an idempotency key on a
// request that left.
func TestClockMovingBackwardsStillIncreases(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_000)
	g := ulid.NewGenerator(rand.Reader, func() time.Time { return now })

	first, err := g.New()
	if err != nil {
		t.Fatal(err)
	}

	now = now.Add(-10 * time.Minute) // ntp step, resumed laptop, container clock
	seen := map[string]bool{first: true}
	prev := first
	for i := 0; i < 100; i++ {
		id, err := g.New()
		if err != nil {
			t.Fatal(err)
		}
		if seen[id] {
			t.Fatalf("id %s repeated after the clock went backwards", id)
		}
		if id <= prev {
			t.Fatalf("id %s does not sort after %s after the clock went backwards", id, prev)
		}
		seen[id] = true
		prev = id
	}
}

func TestConcurrentDrawsAreUnique(t *testing.T) {
	g := ulid.NewGenerator(rand.Reader, time.Now)

	const workers, each = 8, 500
	var mu sync.Mutex
	seen := make(map[string]bool, workers*each)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				id, err := g.New()
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				if seen[id] {
					t.Errorf("concurrent collision on %s", id)
				}
				seen[id] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) != workers*each {
		t.Fatalf("got %d distinct ids, want %d", len(seen), workers*each)
	}
}

// An entropy source that fails must produce an error, not a low-entropy id: a
// run id nobody can predict is what keeps two callers from sharing a key.
func TestEntropyFailureIsRefused(t *testing.T) {
	g := ulid.NewGenerator(failingReader{}, time.Now)
	if _, err := g.New(); err == nil {
		t.Fatal("expected an error when the entropy source fails")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("entropy unavailable") }

func TestValidRefusesNearMisses(t *testing.T) {
	good := "01JAAAAAAAAAAAAAAAAAAAAAAA"
	if !ulid.Valid(good) {
		t.Fatalf("Valid rejects %q", good)
	}
	for name, s := range map[string]string{
		"too short":          good[:25],
		"too long":           good + "A",
		"lowercase":          strings.ToLower(good),
		"letter I":           "01JAAAAAAAAAAAAAAAAAAAAAAI",
		"letter L":           "01JAAAAAAAAAAAAAAAAAAAAAAL",
		"letter O":           "01JAAAAAAAAAAAAAAAAAAAAAAO",
		"letter U":           "01JAAAAAAAAAAAAAAAAAAAAAAU",
		"timestamp overflow": "81JAAAAAAAAAAAAAAAAAAAAAAA",
		"empty":              "",
		"path traversal":     "../../etc/passwd0000000000",
	} {
		if ulid.Valid(s) {
			t.Errorf("Valid accepts %s: %q", name, s)
		}
	}
}

func TestTimestampRoundTrips(t *testing.T) {
	want := time.UnixMilli(1_700_000_000_123).UTC()
	g := ulid.NewGenerator(rand.Reader, func() time.Time { return want })
	id, err := g.New()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ulid.Timestamp(id)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(want) {
		t.Fatalf("Timestamp(%s) = %s, want %s", id, got, want)
	}
	if _, err := ulid.Timestamp("nope"); !errors.Is(err, ulid.ErrInvalid) {
		t.Fatalf("Timestamp on a non-ULID: %v", err)
	}
}

// Every bit of the entropy must reach the encoding: an encoder that dropped a
// byte would shrink the keyspace without changing the shape.
func TestEncodingIsInjectiveOverEntropy(t *testing.T) {
	clock := func() time.Time { return time.UnixMilli(1_700_000_000_000) }
	seen := map[string]int{}
	for bit := 0; bit < 80; bit++ {
		ent := make([]byte, 10)
		ent[bit/8] = 1 << (bit % 8)
		g := ulid.NewGenerator(io.LimitReader(bytes.NewReader(ent), 10), clock)
		id, err := g.New()
		if err != nil {
			t.Fatal(err)
		}
		if prev, dup := seen[id]; dup {
			t.Fatalf("entropy bit %d encodes the same as bit %d", bit, prev)
		}
		seen[id] = bit
	}
}
