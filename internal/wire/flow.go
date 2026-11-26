package wire

import (
	"context"
	"math"
	"sync"
)

// Flow control (spec/protocol.md §8) works like HTTP/2: each stream and
// the connection have a send window of payload bytes. Only ROWS and
// BLOB_DATA count.

// Window is a send window. It is safe for concurrent use.
type Window struct {
	mu     sync.Mutex
	avail  int64
	wake   chan struct{} // closed and replaced when the window grows
	closed error
}

// NewWindow returns a window with n bytes available.
func NewWindow(n int64) *Window { return &Window{avail: n, wake: make(chan struct{})} }

// Available returns the bytes available now.
func (w *Window) Available() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.avail
}

// Take waits until the window has room and takes up to want bytes,
// returning how many it took (at least 1).
func (w *Window) Take(ctx context.Context, want int) (int, error) {
	for {
		w.mu.Lock()
		if w.closed != nil {
			err := w.closed
			w.mu.Unlock()
			return 0, err
		}
		if w.avail > 0 {
			n := int(min(int64(want), w.avail))
			w.avail -= int64(n)
			w.mu.Unlock()
			return n, nil
		}
		wake := w.wake
		w.mu.Unlock()
		select {
		case <-wake:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
}

// Give returns bytes taken but not used.
func (w *Window) Give(n int) {
	w.mu.Lock()
	w.avail += int64(n)
	w.signal()
	w.mu.Unlock()
}

// Add grows the window by a WINDOW_UPDATE increment. A window may not
// exceed 2³¹−1 bytes; going past it is a flow_control error.
func (w *Window) Add(n int64) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if n <= 0 || w.avail+n > math.MaxInt32 {
		return &ConnError{Code: "flow_control", Msg: "window update overflows the window"}
	}
	w.avail += n
	w.signal()
	return nil
}

// Close makes waiting and future Takes fail with err.
func (w *Window) Close(err error) {
	w.mu.Lock()
	if w.closed == nil {
		w.closed = err
		w.signal()
	}
	w.mu.Unlock()
}

func (w *Window) signal() {
	close(w.wake)
	w.wake = make(chan struct{})
}

// TakeBoth takes from a stream window and the connection window, so a
// frame fits both.
func TakeBoth(ctx context.Context, stream, conn *Window, want int) (int, error) {
	n, err := stream.Take(ctx, want)
	if err != nil {
		return 0, err
	}
	m, err := conn.Take(ctx, n)
	if err != nil {
		stream.Give(n)
		return 0, err
	}
	if m < n {
		stream.Give(n - m)
	}
	return m, nil
}

// Credit tracks a receive window: how much the peer may still send. The
// receiver reports consumed bytes and sends WINDOW_UPDATE when Consume
// returns an increment, once half the window has been used.
type Credit struct {
	mu       sync.Mutex
	size     int64
	unacked  int64
	received int64 // bytes received beyond what was granted
}

// NewCredit returns a credit for a window of size bytes.
func NewCredit(size int64) *Credit { return &Credit{size: size} }

// Receive records n bytes arriving, failing if the peer sent more than
// its window allowed.
func (c *Credit) Receive(n int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.received += int64(n)
	if c.received > c.size {
		return &ConnError{Code: "flow_control", Msg: "the peer sent more than its window allowed"}
	}
	return nil
}

// Consume records n bytes processed and returns the increment to send in
// a WINDOW_UPDATE, or 0 if it's too early to send one.
func (c *Credit) Consume(n int) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.unacked += int64(n)
	if c.unacked < c.size/2 {
		return 0
	}
	inc := c.unacked
	c.unacked = 0
	c.received -= inc
	return inc
}
