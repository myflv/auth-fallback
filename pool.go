package main

import (
	"slices"
	"strings"
	"sync"
	"time"
)

// pruneAt is how many cooldowns may pile up before the table is swept. Model
// names come straight out of the request body, so nothing else bounds them.
const pruneAt = 64

// pool hands out API keys.
//
// A model keeps using one key for as long as that key works. Only a 429 moves
// it on to the next one. Staying put matters: the upstream prompt cache is
// per account, so spreading a conversation across keys throws the cache away.
//
// Cooldowns are keyed by model as well as by key. One key's deepseek quota
// running out says nothing about its claude quota, so a deepseek 429 must not
// park that key for every other model, nor move those models to another key.
type pool struct {
	keys []string

	base time.Duration // first 429 cooldown
	max  time.Duration // ceiling for the doubling

	mu      sync.Mutex
	cursor  map[string]int // model -> the key it is currently using
	cooling map[coolKey]cooldown
}

type coolKey struct {
	index int
	model string
}

type cooldown struct {
	until   time.Time
	strikes int // consecutive 429s, cleared by a success
}

// parked is a key that is still cooling down, and when it comes back.
type parked struct {
	index int
	back  time.Time
}

func newPool(keys []string, base, max time.Duration) *pool {
	return &pool{
		keys:    keys,
		base:    base,
		max:     max,
		cursor:  make(map[string]int),
		cooling: make(map[coolKey]cooldown),
	}
}

// key never changes after construction, so it needs no lock.
func (p *pool) key(index int) string { return p.keys[index] }

// backAtsLocked reports, for every key, when it becomes usable for a model
// again. The zero time means it is usable right now.
func (p *pool) backAtsLocked(model string, now time.Time) []time.Time {
	back := make([]time.Time, len(p.keys))
	for i := range p.keys {
		if c, ok := p.cooling[coolKey{i, model}]; ok && now.Before(c.until) {
			back[i] = c.until
		}
	}
	return back
}

// order returns every key index, best first: the model's current key, then the
// rest, with the parked ones last in order of recovery.
//
// Nothing here changes which key the model is on -- that only happens on a 429.
// Parked keys are still listed so a request with nothing left to lose can try
// one that may have recovered early.
func (p *pool) order(model string, now time.Time) []int {
	p.mu.Lock()
	defer p.mu.Unlock()

	n := len(p.keys)
	if n == 0 {
		return nil
	}

	back := p.backAtsLocked(model, now)
	start := p.cursor[model] % n

	ready := make([]int, 0, n)
	var late []parked

	for offset := range n {
		i := (start + offset) % n
		if back[i].IsZero() {
			ready = append(ready, i)
			continue
		}
		late = append(late, parked{index: i, back: back[i]})
	}

	if len(late) > 1 {
		slices.SortStableFunc(late, func(a, b parked) int { return a.back.Compare(b.back) })
	}
	for _, l := range late {
		ready = append(ready, l.index)
	}
	return ready
}

// rateLimited parks a key for one model after a 429 and reports when it is
// usable again. Each consecutive 429 doubles the wait, so a key whose quota is
// spent backs off instead of being hammered every minute.
func (p *pool) rateLimited(index int, model string, now time.Time) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()

	k := coolKey{index, model}
	c := p.cooling[k]

	// Only escalate when the key had actually come back. A key still inside its
	// cooldown is being probed optimistically, and that probe failing says
	// nothing new about the quota.
	if !now.Before(c.until) {
		c.strikes++
	}

	d := p.base
	for n := 1; n < c.strikes && d < p.max; n++ {
		d *= 2
	}
	c.until = now.Add(min(d, p.max))
	p.cooling[k] = c

	// This is the only thing that moves a model off its key.
	p.cursor[model] = (index + 1) % len(p.keys)

	if len(p.cooling) > pruneAt {
		p.pruneLocked(now)
	}
	return c.until
}

// succeeded clears the cooldown, so an escalation does not survive a key coming
// back to life. It deliberately leaves the cursor alone: a key that works keeps
// the model.
func (p *pool) succeeded(index int, model string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	delete(p.cooling, coolKey{index, model})
}

// pruneLocked drops cooldowns that expired long enough ago to say nothing more:
// past p.max the doubling is capped anyway, so the entry is only memory. The
// cursor is left alone -- forgetting it would silently move a healthy model
// back to key 0, which is the switch this whole design avoids.
func (p *pool) pruneLocked(now time.Time) {
	for k, c := range p.cooling {
		if now.After(c.until.Add(p.max)) {
			delete(p.cooling, k)
		}
	}
}

// retryAfter reports how long until some key is usable for a model again,
// rounded up to whole seconds, or 0 if one is free already.
func (p *pool) retryAfter(model string, now time.Time) int {
	p.mu.Lock()
	defer p.mu.Unlock()

	back := p.backAtsLocked(model, now)
	if len(back) == 0 {
		return 0
	}
	// The zero time sorts below every real one, so a single free key makes this
	// come out zero on its own.
	soonest := slices.MinFunc(back, time.Time.Compare)
	if soonest.IsZero() {
		return 0
	}
	return max(int(soonest.Sub(now).Seconds()), 1)
}

type keyStatus struct {
	Index int    `json:"index"`
	Key   string `json:"key"` // masked
	// Cooling maps each model this key is parked for to how much longer it
	// stays that way.
	Cooling map[string]string `json:"cooling,omitempty"`
	// PreferredFor lists the models currently using this key.
	PreferredFor []string `json:"preferred_for,omitempty"`
}

// snapshot is the state behind /status: what explains where traffic is landing.
func (p *pool) snapshot(now time.Time) []keyStatus {
	p.mu.Lock()
	defer p.mu.Unlock()

	out := make([]keyStatus, len(p.keys))
	for i, k := range p.keys {
		out[i] = keyStatus{Index: i, Key: mask(k)}
	}

	// One pass over the cooldowns. Scanning the whole table per key would make
	// this quadratic in a table that a caller can grow.
	for ck, c := range p.cooling {
		if !now.Before(c.until) {
			continue
		}
		if out[ck.index].Cooling == nil {
			out[ck.index].Cooling = map[string]string{}
		}
		out[ck.index].Cooling[ck.model] = c.until.Sub(now).Round(time.Second).String()
	}
	for model, cursor := range p.cursor {
		out[cursor].PreferredFor = append(out[cursor].PreferredFor, model)
	}
	for i := range out {
		slices.Sort(out[i].PreferredFor)
	}
	return out
}

// mask keeps enough of a key to tell two apart without printing the secret.
func mask(k string) string {
	if len(k) <= 10 {
		return strings.Repeat("*", len(k))
	}
	return k[:6] + "…" + k[len(k)-4:]
}
