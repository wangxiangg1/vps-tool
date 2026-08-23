package backoff

import (
	"math/rand"
	"time"
)

var schedule = []time.Duration{
	1 * time.Second,
	2 * time.Second,
	4 * time.Second,
	8 * time.Second,
	16 * time.Second,
	30 * time.Second,
	60 * time.Second,
}

type Policy struct {
	index int
	rng   *rand.Rand
}

func New(seed int64) *Policy {
	return &Policy{rng: rand.New(rand.NewSource(seed))}
}

func (p *Policy) Reset() { p.index = 0 }

func (p *Policy) Next() time.Duration {
	if p.rng == nil {
		p.rng = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	index := p.index
	if index >= len(schedule) {
		index = len(schedule) - 1
	}
	if p.index < len(schedule)-1 {
		p.index++
	}
	base := schedule[index]
	// Keep reconnect attempts bounded while avoiding a synchronized fleet.
	jitter := 0.8 + p.rng.Float64()*0.4
	return time.Duration(float64(base) * jitter)
}

func Schedule() []time.Duration {
	return append([]time.Duration(nil), schedule...)
}
