package main

import (
	"context"
	"slices"
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
	if got := p.order("m", now)[0]; got != 1 {
		t.Errorf("model moved to key %d, want key 1", got)
	}

	// Both keys are parked for the same length now, so the order settles on the
	// cursor -- which has wrapped back round to key 0.
	p.rateLimited(1, "m", now)
	if got := p.order("m", now)[0]; got != 0 {
		t.Errorf("model moved to key %d, want it to wrap back to key 0", got)
	}
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
	// for its history. It has to come after the park it is undoing.
	now = now.Add(5 * time.Minute)
	p.succeeded(0, "m", now)
	if got := p.rateLimited(0, "m", now).Sub(now); got != time.Minute {
		t.Errorf("after a success the cooldown was %s, want %s", got, time.Minute)
	}
}

// Only a key that came back and failed again is backing off. One still inside
// its cooldown is being probed optimistically, and that probe failing says
// nothing new -- and, the part that matters under steady traffic, it must not
// push the key's recovery further out either.
func TestProbingAParkedKeyDoesNotEscalate(t *testing.T) {
	p := newPool([]string{"a"}, time.Minute, time.Hour)
	now := time.Now()

	until := p.rateLimited(0, "m", now)

	again := p.rateLimited(0, "m", now.Add(10*time.Second))
	if again != until {
		t.Errorf("the probe moved the deadline from %s to %s, want it left alone",
			until.Sub(now), again.Sub(now))
	}
	if got := p.cooling[coolKey{0, "m"}].strikes; got != 1 {
		t.Errorf("strikes = %d after a probe, want 1", got)
	}

	// However many probes arrive while it is parked, the key still comes back
	// when it was always due to.
	for i := 1; i <= 5; i++ {
		p.rateLimited(0, "m", now.Add(time.Duration(i)*10*time.Second))
	}
	if got := p.retryAfter("m", now.Add(time.Minute+time.Second)); got != 0 {
		t.Errorf("retryAfter = %ds just past the deadline, want the key back", got)
	}
	if got := p.cooling[coolKey{0, "m"}].strikes; got != 1 {
		t.Errorf("strikes = %d after five probes, want them all ignored", got)
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

// The escalation is about consecutive failures. A key that has been out of
// cooldown for longer than the longest cooldown is not still failing, whatever
// else happens to be in the table.
func TestEscalationDecaysOnTheClock(t *testing.T) {
	p := newPool([]string{"a"}, time.Minute, 5*time.Minute)
	now := time.Now()
	p.rateLimited(0, "m", now)

	// Straight after recovery it is still the same run of failures.
	soon := now.Add(time.Minute + time.Second)
	if got := p.rateLimited(0, "m", soon).Sub(soon); got != 2*time.Minute {
		t.Errorf("a 429 just after recovery parked for %s, want 2m", got)
	}

	// After a long idle the slate is clean.
	q := newPool([]string{"a"}, time.Minute, 5*time.Minute)
	q.rateLimited(0, "m", now)
	idle := now.Add(time.Minute + 5*time.Minute + time.Second)
	if got := q.rateLimited(0, "m", idle).Sub(idle); got != time.Minute {
		t.Errorf("a 429 after a long idle parked for %s, want the base 1m", got)
	}
}

// A cooldown is only reclaimed once it is past saying anything: staleness is
// the point at which decay has already discounted it and order already reads it
// as usable, so dropping it changes no answer.
func TestOnlyStaleCooldownsAreSwept(t *testing.T) {
	// Two keys, so a cursor can sit somewhere other than key 0.
	p := newPool([]string{"a", "b"}, time.Minute, time.Hour)
	now := time.Now()

	p.rateLimited(0, "spent", now)

	// No cooldown has been over long enough to be dead yet.
	p.sweep(now.Add(time.Minute))
	if _, ok := p.cooling[coolKey{0, "spent"}]; !ok {
		t.Error("the sweep dropped a cooldown that had not gone stale")
	}

	// Long past saying anything -- and a fresh one recorded at the same moment,
	// which must survive.
	later := now.Add(time.Hour + 2*time.Minute)
	p.rateLimited(0, "fresh", later)
	p.sweep(later)

	if _, ok := p.cooling[coolKey{0, "spent"}]; ok {
		t.Error("a long-stale cooldown survived the sweep")
	}
	if _, ok := p.cooling[coolKey{0, "fresh"}]; !ok {
		t.Error("the sweep dropped a cooldown that had just been recorded")
	}
	// The sweep sheds cooldowns, never cursors: forgetting one would move a
	// healthy model back to key 0, which is the switch the design avoids.
	if got := p.cursor["spent"]; got != 1 {
		t.Errorf("cursor for the swept model is %d, want it left at 1", got)
	}
}

// Reclaiming is on a timer off the request path, so nothing happens at all
// unless something runs the loop.
func TestSweepLoopReclaimsDeadCooldowns(t *testing.T) {
	p := newPool([]string{"a"}, time.Minute, time.Hour)
	p.cooling[coolKey{0, "spent"}] = cooldown{until: time.Now().Add(-2 * time.Hour)}

	gone := func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		_, ok := p.cooling[coolKey{0, "spent"}]
		return !ok
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go p.sweepLoop(ctx, time.Millisecond)

	for !gone() {
		select {
		case <-ctx.Done():
			t.Fatal("the sweep loop never reclaimed the dead cooldown")
		case <-time.After(time.Millisecond):
		}
	}
}

// A key that recovers must not take a model back from the key that is serving
// it. That would be a switch with no 429 behind it, costing exactly the cache
// stickiness exists to keep.
func TestARecoveredKeyDoesNotStealTheModelBack(t *testing.T) {
	p := newPool([]string{"a", "b", "c"}, time.Minute, time.Hour)
	now := time.Now()

	p.rateLimited(0, "m", now) // cursor -> 1
	p.rateLimited(2, "m", now) // cursor wraps back round onto parked key 0
	p.succeeded(1, "m", now)   // but key 1 is the one doing the work

	// By now every cooldown has run out. The model still belongs to key 1.
	later := now.Add(2 * time.Minute)
	if got := p.order("m", later)[0]; got != 1 {
		t.Errorf("model moved to key %d, want it left on key 1", got)
	}
}

// Both edges of the window are exact. A cooldown that has just run out is the
// same run continuing, and one idle for exactly the longest cooldown still is;
// a nanosecond more and it starts over.
func TestDecayBoundaries(t *testing.T) {
	now := time.Now()
	fresh := func() *pool {
		p := newPool([]string{"a"}, time.Minute, 5*time.Minute)
		p.rateLimited(0, "m", now)
		return p
	}

	at := now.Add(time.Minute) // exactly when the key is due back
	if p := fresh(); p.rateLimited(0, "m", at).Sub(at) != 2*time.Minute {
		t.Errorf("at the deadline the cooldown was %s, want 2m", p.rateLimited(0, "m", at).Sub(at))
	}

	edge := now.Add(time.Minute + 5*time.Minute) // exactly the end of the window
	if p := fresh(); p.rateLimited(0, "m", edge).Sub(edge) != 2*time.Minute {
		t.Errorf("at the edge of the window the cooldown was %s, want 2m", p.rateLimited(0, "m", edge).Sub(edge))
	}

	past := edge.Add(time.Nanosecond)
	if p := fresh(); p.rateLimited(0, "m", past).Sub(past) != time.Minute {
		t.Errorf("past the window the cooldown was %s, want the base 1m", p.rateLimited(0, "m", past).Sub(past))
	}
}

// A success from a request that was already in flight when the key got parked
// must not undo that. The 429 is the newer news, and letting the older request
// win would walk the model back to a key the proxy has just been told is
// limited -- a switch with nothing behind it.
func TestASuccessCannotUndoANewerPark(t *testing.T) {
	p := newPool([]string{"a", "b"}, time.Minute, time.Hour)
	sent := time.Now()

	// This request goes out at sent, and comes back 200 only much later.
	// Meanwhile a faster one has already taken a 429 from the same key.
	p.rateLimited(0, "m", sent.Add(time.Second))
	p.succeeded(0, "m", sent)

	if _, ok := p.cooling[coolKey{0, "m"}]; !ok {
		t.Error("a stale success cleared a park that was newer than it")
	}
	if got := p.order("m", sent.Add(2*time.Second))[0]; got != 1 {
		t.Errorf("the model went back to key %d, want it left on key 1", got)
	}
}

// The opposite case: the key was already parked when the request went out, and
// it came back 200 -- a probe finding the key had recovered early. That one
// does count, or the key would sit out a cooldown it has just disproved.
func TestAProbeThatSucceedsClearsThePark(t *testing.T) {
	p := newPool([]string{"a", "b"}, time.Minute, time.Hour)
	now := time.Now()
	p.rateLimited(0, "m", now)

	p.succeeded(0, "m", now.Add(time.Second)) // sent while key 0 was parked

	if _, ok := p.cooling[coolKey{0, "m"}]; ok {
		t.Error("a probe that came back 200 left the park in place")
	}
	if got := p.order("m", now.Add(2*time.Second))[0]; got != 0 {
		t.Errorf("the model is on key %d, want the key that just served it", got)
	}
}

// Which of the two replies arrives first must not change where the model ends
// up. Whichever order they land in, the 429 is what decides.
func TestARaceSettlesTheSameEitherWay(t *testing.T) {
	settle := func(successFirst bool) (int, bool) {
		p := newPool([]string{"a", "b"}, time.Minute, time.Hour)
		sent := time.Now() // both requests went out here
		early := sent.Add(time.Second)
		late := sent.Add(2 * time.Second)

		if successFirst {
			p.succeeded(0, "m", sent)
			p.rateLimited(0, "m", early)
		} else {
			p.rateLimited(0, "m", early)
			p.succeeded(0, "m", sent)
		}
		_, parked := p.cooling[coolKey{0, "m"}]
		return p.order("m", late)[0], parked
	}

	firstKey, firstParked := settle(true)
	secondKey, secondParked := settle(false)

	if firstKey != secondKey || firstParked != secondParked {
		t.Errorf("the race settled by arrival order: success-first -> key %d parked=%v, 429-first -> key %d parked=%v",
			firstKey, firstParked, secondKey, secondParked)
	}
	if firstKey != 1 || !firstParked {
		t.Errorf("settled on key %d with key 0 parked=%v, want key 1 with key 0 still parked",
			firstKey, firstParked)
	}
}
