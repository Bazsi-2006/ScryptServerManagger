// Package console holds the building blocks of the server console: a bounded
// line buffer with fan-out to live subscribers, a terminal-output sanitizer
// and a file tailer.
package console

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
)

// Line is one console line. Seq increases by one per line within an epoch.
type Line struct {
	Seq  uint64 `json:"seq"`
	Text string `json:"text"`
}

// Event is delivered to subscribers. Reset means the buffer was cleared and
// the subscriber should discard what it has shown so far.
type Event struct {
	Line  Line
	Reset bool
	Epoch string
}

// Subscriber receives live events. C is closed when the subscription ends,
// either through Unsubscribe or because the subscriber fell too far behind
// (Lagged reports true in that case).
type Subscriber struct {
	C      chan Event
	buf    *Buffer
	lagged bool
	closed bool
}

// Lagged reports whether the subscription was dropped for being too slow.
func (s *Subscriber) Lagged() bool {
	s.buf.mu.Lock()
	defer s.buf.mu.Unlock()
	return s.lagged
}

// Buffer is a fixed-capacity ring of recent lines. Memory use is bounded by
// capacity × maximum line length; slow subscribers are disconnected rather
// than buffered without limit.
type Buffer struct {
	mu      sync.Mutex
	ring    []Line
	start   int
	count   int
	nextSeq uint64
	epoch   string
	subs    map[*Subscriber]struct{}
}

func newEpoch() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func NewBuffer(capacity int) *Buffer {
	if capacity < 1 {
		capacity = 1
	}
	return &Buffer{ring: make([]Line, capacity), nextSeq: 1, epoch: newEpoch(), subs: map[*Subscriber]struct{}{}}
}

// Append adds a line and delivers it to all subscribers.
func (b *Buffer) Append(text string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	l := Line{Seq: b.nextSeq, Text: text}
	b.nextSeq++
	idx := (b.start + b.count) % len(b.ring)
	b.ring[idx] = l
	if b.count < len(b.ring) {
		b.count++
	} else {
		b.start = (b.start + 1) % len(b.ring)
	}
	b.broadcast(Event{Line: l, Epoch: b.epoch})
}

func (b *Buffer) broadcast(ev Event) {
	for s := range b.subs {
		select {
		case s.C <- ev:
		default:
			// The subscriber cannot keep up; drop it so it reconnects and
			// resynchronises instead of stalling everyone else.
			s.lagged = true
			s.closed = true
			close(s.C)
			delete(b.subs, s)
		}
	}
}

// Reset clears the buffer and starts a new epoch.
func (b *Buffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.start, b.count = 0, 0
	b.nextSeq = 1
	b.epoch = newEpoch()
	b.broadcast(Event{Reset: true, Epoch: b.epoch})
}

// LastSeq returns the sequence number of the newest line (0 if none).
func (b *Buffer) LastSeq() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.nextSeq - 1
}

func (b *Buffer) Epoch() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.epoch
}

func (b *Buffer) tailLocked(n int) []Line {
	if n <= 0 || n > b.count {
		n = b.count
	}
	out := make([]Line, n)
	first := b.count - n
	for i := 0; i < n; i++ {
		out[i] = b.ring[(b.start+first+i)%len(b.ring)]
	}
	return out
}

// Tail returns up to n of the most recent lines and the current epoch.
func (b *Buffer) Tail(n int) ([]Line, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tailLocked(n), b.epoch
}

// sinceLocked returns the lines with Seq > after, or ok=false when some of
// them have already been evicted (or `after` is from the future).
func (b *Buffer) sinceLocked(after uint64) ([]Line, bool) {
	last := b.nextSeq - 1
	if after > last {
		return nil, false
	}
	if after == last {
		return nil, true
	}
	if b.count == 0 {
		return nil, false
	}
	oldest := b.ring[b.start].Seq
	if after+1 < oldest {
		return nil, false
	}
	return b.tailLocked(int(last - after)), true
}

// Subscribe atomically captures a backlog and registers for live events, so
// the caller sees every line exactly once.
//
// If epoch matches the current epoch and the lines after afterSeq are still
// buffered, the backlog continues from there (reset=false). Otherwise the
// caller gets the latest snapshotLines lines and reset=true.
func (b *Buffer) Subscribe(epoch string, afterSeq uint64, snapshotLines, chanSize int) (backlog []Line, reset bool, curEpoch string, sub *Subscriber) {
	if chanSize < 16 {
		chanSize = 16
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if epoch == b.epoch {
		if lines, ok := b.sinceLocked(afterSeq); ok {
			backlog = lines
		} else {
			reset = true
		}
	} else {
		reset = true
	}
	if reset {
		backlog = b.tailLocked(snapshotLines)
	}
	sub = &Subscriber{C: make(chan Event, chanSize), buf: b}
	b.subs[sub] = struct{}{}
	return backlog, reset, b.epoch, sub
}

// Unsubscribe ends a subscription. It is safe to call more than once.
func (b *Buffer) Unsubscribe(s *Subscriber) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.C)
	}
	delete(b.subs, s)
}

// Subscribers returns the number of live subscribers.
func (b *Buffer) Subscribers() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

// CloseAll disconnects every subscriber (used when a server is removed).
func (b *Buffer) CloseAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for s := range b.subs {
		s.closed = true
		close(s.C)
		delete(b.subs, s)
	}
}
