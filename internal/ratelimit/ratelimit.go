// Package ratelimit counts events per key in fixed time windows, in memory.
// Anchor runs as a single process, so no shared store is needed.
package ratelimit

import (
	"sync"
	"time"
)

type window struct {
	start time.Time
	count int
}

type Limiter struct {
	limit int
	every time.Duration
	now   func() time.Time

	mu   sync.Mutex
	keys map[string]*window
}

// New allows limit events per key in each window of length every.
func New(limit int, every time.Duration) *Limiter {
	return &Limiter{limit: limit, every: every, now: time.Now, keys: map[string]*window{}}
}

func (l *Limiter) current(key string) *window {
	now := l.now()
	w := l.keys[key]
	if w == nil || now.Sub(w.start) >= l.every {
		w = &window{start: now}
		l.keys[key] = w
	}
	if len(l.keys) > 100_000 {
		l.sweep(now)
	}
	return w
}

// Allow records an event for key and reports whether it is within the limit.
// When it is not, retry says how long until the window resets.
func (l *Limiter) Allow(key string) (ok bool, retry time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.current(key)
	if w.count >= l.limit {
		return false, w.start.Add(l.every).Sub(l.now())
	}
	w.count++
	return true, 0
}

// Reset forgets key, e.g. after a successful login, so only failures add up.
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.keys, key)
}

// sweep drops expired windows. Called with mu held.
func (l *Limiter) sweep(now time.Time) {
	for k, w := range l.keys {
		if now.Sub(w.start) >= l.every {
			delete(l.keys, k)
		}
	}
}

// Sweep drops expired windows; call it periodically to bound memory.
func (l *Limiter) Sweep() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(l.now())
}
