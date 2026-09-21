package main

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// The reason keys are sticky: the upstream prompt cache is per account, so a
// key that works should keep the conversation.
func TestKeyStaysPutUntilItIsRateLimited(t *testing.T) {
	p := newPool([]string{"a", "b", "c"}, time.Minute, time.Hour)
	now := time.Now()

	for i := range 5 {
		if got := p.order("m", now); got[0] != 0 {
			t.Fatalf("request %d used key %d, want key 0 every time (order %v)", i, got[0], got)
		}
	}

	// A 429 is the only thing that moves a model on.
	p.rateLimited(0, "m", now)
	if got := p.order("m", now); got[0] != 1 {
		t.Errorf("after a 429 order = %v, want key 1", got)
	}
	p.rateLimited(1, "m", now)
	if got := p.order("m", now); got[0] != 2 {
		t.Errorf("order = %v, want key 2", got)
	}
}

// Past the last key it wraps, so nothing is stranded at either end of the list.
func TestCursorWrapsAround(t *testing.T) {
	p := newPool([]string{"a", "b"}, time.Minute, time.Hour)
	now := time.Now()

	p.rateLimited(0, "m", now)
	if got := preferred(p, "m"); got != 1 {
		t.Errorf("model moved to key %d, want key 1", got)
	}
	p.rateLimited(1, "m", now)
	if got := preferred(p, "m"); got != 0 {
		t.Errorf("model moved to key %d, want it to wrap back to key 0", got)
	}
}

// preferred reads the model's current key back out of the published state, so
// the test asserts what /status shows rather than poking at internals.
func preferred(p *pool, model string) int {
	for _, st := range p.snapshot(time.Now()) {
		if slices.Contains(st.PreferredFor, model) {
			return st.Index
		}
	}
	return -1
}

// The whole point of the pool: a key that ran out of deepseek quota must stay
// available for everything else, and must not drag other models off their key.
func TestCooldownDoesNotLeakAcrossModels(t *testing.T) {
	p := newPool([]string{"a", "b"}, time.Minute, time.Hour)
	now := time.Now()
	p.rateLimited(0, deepseek, now)

	// deepseek moves on...
	if got := p.order(deepseek, now); got[0] != 1 {
		t.Errorf("deepseek order = %v, want the next key", got)
	}
	// ...and claude does not.
	if got := p.order(claude, now); got[0] != 0 {
		t.Errorf("claude moved to key %d: a deepseek 429 must not move another model", got[0])
	}
	if got := p.retryAfter(claude, now); got != 0 {
		t.Errorf("claude waits %ds: the deepseek cooldown leaked across models", got)
	}
}

func TestCooldownDoublesThenCaps(t *testing.T) {
	p := newPool([]string{"a"}, time.Minute, 5*time.Minute)
	now := time.Now()

	for i, want := range []time.Duration{
		time.Minute, 2 * time.Minute, 4 * time.Minute,
		5 * time.Minute, // capped
		5 * time.Minute,
	} {
		// Each 429 has to land after the previous cooldown ran out, otherwise
		// it is just a probe of a key that never came back.
		now = now.Add(want)
		if got := p.rateLimited(0, "m", now).Sub(now); got != want {
			t.Errorf("429 #%d parked for %s, want %s", i+1, got, want)
		}
	}

	// One success resets the escalation, so a key that recovers is not punished
	// for its history.
	p.succeeded(0, "m")
	if got := p.rateLimited(0, "m", now).Sub(now); got != time.Minute {
		t.Errorf("after a success the cooldown was %s, want %s", got, time.Minute)
	}
}

// Only a key that came back and failed again is backing off. One still inside
// its cooldown is being probed optimistically, and that probe failing says
// nothing new about the quota.
func TestProbingAParkedKeyDoesNotEscalate(t *testing.T) {
	p := newPool([]string{"a"}, time.Minute, time.Hour)
	now := time.Now()

	first := p.rateLimited(0, "m", now)
	probe := now.Add(10 * time.Second)
	second := p.rateLimited(0, "m", probe)

	if got := second.Sub(probe); got != time.Minute {
		t.Errorf("the probe parked the key for %s, want the cooldown still %s", got, time.Minute)
	}
	if !second.After(first) {
		t.Errorf("second = %s, want it pushed past the first (%s)", second, first)
	}
}

func TestParkedKeysAreOfferedEarliestFirst(t *testing.T) {
	p := newPool([]string{"a", "b", "c"}, time.Minute, time.Hour)
	now := time.Now()

	p.rateLimited(2, "m", now)                     // c: back at +1m
	p.rateLimited(1, "m", now.Add(30*time.Second)) // b: back at +1m30s

	got := p.order("m", now)
	if len(got) != 3 {
		t.Fatalf("order = %v, want every key offered", got)
	}
	if got[0] != 0 {
		t.Errorf("order = %v, want the free key first", got)
	}
	// Parked keys are still offered, soonest first: an optimistic attempt beats
	// failing outright.
	if got[1] != 2 || got[2] != 1 {
		t.Errorf("order = %v, want [0 2 1] (c recovers before b)", got)
	}
}

// A key that recovers goes back into rotation, but the model does not
// automatically drift back to it -- that would be another cache-throwing
// switch.
func TestRecoveredKeyIsOfferedButDoesNotTakeOver(t *testing.T) {
	p := newPool([]string{"a", "b"}, time.Minute, time.Hour)
	now := time.Now()
	p.rateLimited(0, "m", now) // the model moves to key 1

	later := now.Add(2 * time.Minute)
	if got := p.retryAfter("m", later); got != 0 {
		t.Errorf("retryAfter = %d, want 0 once the key is free", got)
	}
	got := p.order("m", later)
	if !slices.Contains(got, 0) {
		t.Errorf("order = %v, want the recovered key offered again", got)
	}
	if got[0] != 1 {
		t.Errorf("order = %v, want the model to stay on key 1", got)
	}
}

func TestRetryAfter(t *testing.T) {
	p := newPool([]string{"a", "b"}, 30*time.Second, time.Hour)
	now := time.Now()

	if got := p.retryAfter("m", now); got != 0 {
		t.Errorf("retryAfter with a free key = %d, want 0", got)
	}

	// One key still free means the model is not waiting on anything.
	p.rateLimited(0, "m", now)
	if got := p.retryAfter("m", now); got != 0 {
		t.Errorf("retryAfter with a second key spare = %d, want 0", got)
	}

	p.rateLimited(1, "m", now)
	if got := p.retryAfter("m", now); got != 30 {
		t.Errorf("retryAfter = %d, want 30", got)
	}
	if got := p.retryAfter(deepseek, now); got != 0 {
		t.Errorf("another model = %d, want 0", got)
	}
}

// Model names come out of the request body, so the cooldown table needs a way
// to forget the ones nobody is using any more.
func TestExpiredCooldownsArePruned(t *testing.T) {
	p := newPool([]string{"a"}, time.Minute, time.Hour)
	now := time.Now()

	for i := range pruneAt + 5 {
		p.rateLimited(0, fmt.Sprintf("model-%d", i), now)
	}
	if len(p.cooling) <= pruneAt {
		t.Fatalf("table holds %d entries, expected the sweep threshold to be crossed", len(p.cooling))
	}

	// Long enough that every entry is past the point where it says anything.
	later := now.Add(3 * time.Hour)
	p.rateLimited(0, "fresh", later)

	if _, ok := p.cooling[coolKey{0, "model-0"}]; ok {
		t.Error("a long-expired cooldown survived the sweep")
	}
	if _, ok := p.cooling[coolKey{0, "fresh"}]; !ok {
		t.Error("the sweep dropped the cooldown it had just been handed")
	}
}

// Forgetting a cursor would silently move a healthy model back to key 0, which
// is exactly the switch the design avoids.
func TestPruningKeepsCursors(t *testing.T) {
	p := newPool([]string{"a", "b"}, time.Minute, time.Hour)
	now := time.Now()
	p.rateLimited(0, "m", now)

	p.pruneLocked(now.Add(24 * time.Hour))
	if got := preferred(p, "m"); got != 1 {
		t.Errorf("the model is on key %d after a sweep, want it still on key 1", got)
	}
}

func TestSnapshotMasksKeys(t *testing.T) {
	p := newPool([]string{"sk-cline-abcdefghijklmnop"}, time.Minute, time.Hour)
	now := time.Now()
	p.rateLimited(0, deepseek, now)

	snap := p.snapshot(now)
	if len(snap) != 1 {
		t.Fatalf("snapshot = %v", snap)
	}
	if strings.Contains(snap[0].Key, "abcdefghij") {
		t.Errorf("the key leaked into /status: %q", snap[0].Key)
	}
	if snap[0].Cooling[deepseek] == "" {
		t.Errorf("snapshot does not say what is parked: %+v", snap[0])
	}
	if !slices.Contains(snap[0].PreferredFor, deepseek) {
		t.Errorf("snapshot does not say which key the model is on: %+v", snap[0])
	}
}
