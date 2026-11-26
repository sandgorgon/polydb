package authn

import (
	"context"
	"sync"
	"time"
)

// Limiter slows down repeated failed logins from one source: after n
// failures in a row, the next attempt waits base·2^(n−1), up to max
// (spec/protocol.md §3.3). A success clears the count.
type Limiter struct {
	Base, Max time.Duration
	Now       func() time.Time

	mu    sync.Mutex
	state map[string]*failures
}

type failures struct {
	n    int
	last time.Time
}

// NewLimiter returns a limiter with a 100 ms base delay capped at 30 s.
func NewLimiter() *Limiter {
	return &Limiter{Base: 100 * time.Millisecond, Max: 30 * time.Second, Now: time.Now, state: map[string]*failures{}}
}

// Delay returns how long the next attempt from addr must wait.
func (l *Limiter) Delay(addr string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.state[addr]
	if f == nil || f.n == 0 {
		return 0
	}
	d := l.Base << min(f.n-1, 30)
	if d > l.Max || d <= 0 {
		d = l.Max
	}
	if wait := d - l.Now().Sub(f.last); wait > 0 {
		return wait
	}
	return 0
}

// Wait sleeps for the delay of addr, or until ctx ends.
func (l *Limiter) Wait(ctx context.Context, addr string) error {
	d := l.Delay(addr)
	if d == 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Fail records a failed login from addr.
func (l *Limiter) Fail(addr string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.state[addr]
	if f == nil {
		f = &failures{}
		l.state[addr] = f
	}
	f.n++
	f.last = l.Now()
	// Forget sources that have been quiet for a while.
	if len(l.state) > 10_000 {
		for a, s := range l.state {
			if l.Now().Sub(s.last) > l.Max {
				delete(l.state, a)
			}
		}
	}
}

// Succeed clears the failures of addr.
func (l *Limiter) Succeed(addr string) {
	l.mu.Lock()
	delete(l.state, addr)
	l.mu.Unlock()
}

// Failures returns the number of failures in a row from addr, for
// show sessions and the server log.
func (l *Limiter) Failures(addr string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if f := l.state[addr]; f != nil {
		return f.n
	}
	return 0
}
