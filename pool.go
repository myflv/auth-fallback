package main

import (
	"sort"
	"strings"
	"sync"
	"time"
)

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

func (p *pool) len() int { return len(p.keys) }

// backAtLocked reports when a key becomes usable for a model again, or the zero
// time if it is usable right now.
func (p *pool) backAtLocked(index int, model string, now time.Time) time.Time {
	if c, ok := p.cooling[coolKey{index, model}]; ok && now.Before(c.until) {
		return c.until
	}
	return time.Time{}
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

	start := p.cursor[model] % n
	ready := make([]int, 0, n)
	backAt := make(map[int]time.Time, n)

	for offset := range n {
		i := (start + offset) % n
		if t := p.backAtLocked(i, model, now); t.IsZero() {
			ready = append(ready, i)
		} else {
			backAt[i] = t
		}
	}

	parked := make([]int, 0, len(backAt))
	for i := range backAt {
		parked = append(parked, i)
	}
	sort.Slice(parked, func(a, b int) bool {
		return backAt[parked[a]].Before(backAt[parked[b]])
	})

	return append(ready, parked...)
}

// rateLimited parks a key for one model after a 429 and reports when it is
// usable again. Each consecutive 429 doubles the wait, so a key whose quota is
// spent backs off instead of being hammered every minute.
func (p *pool) rateLimited(index int, model string, now time.Time) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()

	k := coolKey{index, model}
	c := p.cooling[k]
	c.strikes++

	d := p.base
	for n := 1; n < c.strikes && d < p.max; n++ {
		d *= 2
	}
	if d > p.max {
		d = p.max
	}
	c.until = now.Add(d)
	p.cooling[k] = c

	// This is the only thing that moves a model off its key.
	if n := len(p.keys); n > 0 {
		p.cursor[model] = (index + 1) % n
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

// retryAfter reports how long until some key is usable for a model again,
// rounded up to whole seconds, or 0 if one is free already.
func (p *pool) retryAfter(model string, now time.Time) int {
	p.mu.Lock()
	defer p.mu.Unlock()

	var soonest time.Time
	for i := range p.keys {
		t := p.backAtLocked(i, model, now)
		if t.IsZero() {
			return 0
		}
		if soonest.IsZero() || t.Before(soonest) {
			soonest = t
		}
	}
	if soonest.IsZero() {
		return 0
	}
	secs := int(soonest.Sub(now).Seconds())
	if secs < 1 {
		secs = 1
	}
	return secs
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

	out := make([]keyStatus, 0, len(p.keys))
	for i, k := range p.keys {
		st := keyStatus{Index: i, Key: mask(k)}
		for ck, c := range p.cooling {
			if ck.index == i && now.Before(c.until) {
				if st.Cooling == nil {
					st.Cooling = map[string]string{}
				}
				st.Cooling[ck.model] = c.until.Sub(now).Round(time.Second).String()
			}
		}
		for model, cursor := range p.cursor {
			if cursor == i {
				st.PreferredFor = append(st.PreferredFor, model)
			}
		}
		sort.Strings(st.PreferredFor)
		out = append(out, st)
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
