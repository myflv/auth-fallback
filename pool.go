package main

import (
	"context"
	"slices"
	"sync"
	"time"
)

// pool hands out API keys.
//
// A model keeps using one key until that key answers 429; only then does it
// move on. Staying put is the point -- the upstream prompt cache is per
// account, so spreading a conversation across keys throws it away.
//
// Cooldowns are per (key, model): one model running out leaves the key free
// for the others.
type pool struct {
	keys []string

	base time.Duration // first 429 cooldown
	// max caps the doubling, and is also the window after which a recovered
	// key's escalation counts as over rather than merely paused.
	max time.Duration

	mu      sync.Mutex
	cursor  map[string]int // model -> the key it is currently using
	cooling map[coolKey]cooldown
}

type coolKey struct {
	index int
	model string
}

type cooldown struct {
	until time.Time
	// parkedAt is when this cooldown was set. A success can only undo a park
	// older than itself -- see succeeded.
	parkedAt time.Time
	// strikes is 429s in a row. Cleared by a success, or by sitting idle past max.
	strikes int
}

// usable reports whether a key may be tried now. A missing entry reads as the
// zero cooldown, which is usable.
func (c cooldown) usable(now time.Time) bool {
	return !now.Before(c.until)
}

// stale reports whether a cooldown is past saying anything. usable is already
// true for such an entry, so dropping it changes no answer.
func (c cooldown) stale(now time.Time, max time.Duration) bool {
	return now.After(c.until.Add(max))
}

// parked is a key still cooling down, and when it comes back.
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

// order returns every key index, best first: the model's current key, then the
// rest, with parked ones last in order of recovery. It changes nothing about
// which key the model is on -- only a 429 or a success does that.
func (p *pool) order(model string, now time.Time) []int {
	p.mu.Lock()
	defer p.mu.Unlock()

	n := len(p.keys)
	if n == 0 {
		return nil
	}

	start := p.cursor[model] % n
	ready := make([]int, 0, n)
	var late []parked

	for offset := range n {
		i := (start + offset) % n
		c := p.cooling[coolKey{i, model}]
		if c.usable(now) {
			ready = append(ready, i)
			continue
		}
		late = append(late, parked{index: i, back: c.until})
	}

	// Stable, so keys due back together keep the cursor's order.
	if len(late) > 1 {
		slices.SortStableFunc(late, func(a, b parked) int { return a.back.Compare(b.back) })
	}
	for _, l := range late {
		ready = append(ready, l.index)
	}
	return ready
}

// rateLimited parks a key for one model after a 429 and reports when it is
// usable again. Each consecutive 429 doubles the wait.
func (p *pool) rateLimited(index int, model string, now time.Time) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()

	k := coolKey{index, model}
	c := p.cooling[k]

	if c.usable(now) {
		// A key idle past max is starting over, not continuing.
		if c.stale(now, p.max) {
			c.strikes = 0
		}
		c.strikes++

		d := p.base
		for n := 1; n < c.strikes && d < p.max; n++ {
			if d > p.max>>1 { // doubling would overflow a Duration
				d = p.max
				break
			}
			d *= 2
		}
		c.until = now.Add(min(d, p.max))
		c.parkedAt = now
	}
	// Still parked means this was a probe, and a probe leaves the deadline
	// alone. Pushing it forward would walk the key's recovery out forever.
	p.cooling[k] = c

	p.cursor[model] = (index + 1) % len(p.keys)

	return c.until
}

// succeeded hands the model to a key that served it, if that is still the
// newest news. sent is when the request went out: a slower request coming back
// 200 after a faster one already took a 429 from the same key is stale, and
// would drag the model back to a key just reported as limited.
func (p *pool) succeeded(index int, model string, sent time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()

	k := coolKey{index, model}
	if p.cooling[k].parkedAt.After(sent) {
		return
	}
	delete(p.cooling, k)
	p.cursor[model] = index
}

// sweepLoop reclaims dead cooldowns on a timer until ctx is done, keeping that
// work off the request path.
func (p *pool) sweepLoop(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			p.sweep(now)
		}
	}
}

// sweep drops cooldowns past saying anything. Cursors are left alone: losing
// one would move a healthy model back to key 0.
func (p *pool) sweep(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for k, c := range p.cooling {
		if c.stale(now, p.max) {
			delete(p.cooling, k)
		}
	}
}

// retryAfter reports how long until some key is usable for a model again,
// rounded up to whole seconds, or 0 if one is free already.
func (p *pool) retryAfter(model string, now time.Time) int {
	p.mu.Lock()
	defer p.mu.Unlock()

	var soonest time.Time
	for i := range p.keys {
		c := p.cooling[coolKey{i, model}]
		if c.usable(now) {
			return 0 // this key is usable right now
		}
		if soonest.IsZero() || c.until.Before(soonest) {
			soonest = c.until
		}
	}
	if soonest.IsZero() {
		return 0
	}
	// Round up: coming back a second early just collects another 429.
	left := soonest.Sub(now)
	return max(int((left+time.Second-1)/time.Second), 1)
}
