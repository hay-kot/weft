package events

import (
	"sync"
	"time"
)

// DefaultOutputBudget is how many bytes of node output may wait in one
// subscriber's queue before further lines are dropped for it.
const DefaultOutputBudget = 8 << 20

// Bus fans events out to subscribers. Publish never blocks: each subscriber
// has its own queue and goroutine, so a subscriber stuck on a paused pipe
// can't stall the engine or delay a cancel. State events are never dropped;
// a run has a handful per node. Node output is dropped for a subscriber
// once its queued output passes the subscriber's budget, and the
// subscriber gets an OutputDropped event in the place of the gap.
type Bus struct {
	mu     sync.Mutex
	subs   []*Subscription
	closed bool
}

// NewBus returns an empty bus.
func NewBus() *Bus {
	return &Bus{mu: sync.Mutex{}, subs: nil, closed: false}
}

// Subscription is one subscriber's view of the bus.
type Subscription struct {
	// C receives events in publish order. It closes after the bus closes
	// and every queued event has been delivered.
	C <-chan Event

	out    chan Event
	wake   chan struct{}
	budget int

	mu     sync.Mutex
	queue  []item
	queued int // bytes of NodeOutput in queue
	// dropping maps a node to the counter of the drop marker that is in the
	// queue for its current gap, if any.
	dropping map[string]*int
	closed   bool
	finished chan struct{}
}

// item is one queue entry: an event, or a marker where a node's output was
// dropped. The marker is delivered as an OutputDropped with the count the
// gap reached, so a reader learns about the gap where it happened and the
// queue grows by one entry per gap, not per dropped line.
type item struct {
	ev   Event
	drop *int
	node string
}

// Subscribe adds a subscriber. outputBudget caps the bytes of queued node
// output; zero means DefaultOutputBudget and a negative value never drops.
func (b *Bus) Subscribe(outputBudget int) *Subscription {
	if outputBudget == 0 {
		outputBudget = DefaultOutputBudget
	}
	out := make(chan Event)
	s := &Subscription{
		C:        out,
		out:      out,
		wake:     make(chan struct{}, 1),
		budget:   outputBudget,
		mu:       sync.Mutex{},
		queue:    nil,
		queued:   0,
		dropping: map[string]*int{},
		closed:   false,
		finished: make(chan struct{}),
	}
	go s.pump()

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		s.close()
		return s
	}
	b.subs = append(b.subs, s)
	return s
}

// Publish queues e for every subscriber and returns at once. Publishing
// after Close is a no-op.
func (b *Bus) Publish(e Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	for _, s := range b.subs {
		s.push(e)
	}
}

// Close stops publishing. Each subscription's channel closes once its queue
// drains.
func (b *Bus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for _, s := range b.subs {
		s.close()
	}
}

// Barrier queues a Barrier event behind every event published so far and
// waits until the subscriber calls Done on it, the subscription finishes,
// or timeout passes. It reports whether the subscriber reached it.
func (s *Subscription) Barrier(timeout time.Duration) bool {
	b := Barrier{done: make(chan struct{})}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return false
	}
	s.queue = append(s.queue, item{ev: b, drop: nil, node: ""})
	s.mu.Unlock()
	s.signal()

	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-b.done:
		return true
	case <-s.finished:
		return false
	case <-t.C:
		return false
	}
}

func (s *Subscription) push(e Event) {
	s.mu.Lock()
	if out, ok := e.(NodeOutput); ok {
		if s.budget > 0 && s.queued+len(out.Line) > s.budget {
			if c := s.dropping[out.Node]; c != nil {
				*c++
				s.mu.Unlock()
				return
			}
			c := new(int)
			*c = 1
			s.dropping[out.Node] = c
			s.queue = append(s.queue, item{ev: nil, drop: c, node: out.Node})
			s.mu.Unlock()
			s.signal()
			return
		}
		s.queued += len(out.Line)
		// A line got through, so a later drop is a new gap with its own
		// marker behind this line.
		delete(s.dropping, out.Node)
	}
	s.queue = append(s.queue, item{ev: e, drop: nil, node: ""})
	s.mu.Unlock()
	s.signal()
}

func (s *Subscription) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Subscription) close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.signal()
}

// next pops the next event to deliver.
func (s *Subscription) next() (Event, bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) == 0 {
		return nil, false, s.closed
	}
	it := s.queue[0]
	s.queue[0] = item{ev: nil, drop: nil, node: ""}
	s.queue = s.queue[1:]
	if it.drop != nil {
		// Drops that follow delivery of the marker start a new gap.
		if s.dropping[it.node] == it.drop {
			delete(s.dropping, it.node)
		}
		return OutputDropped{Node: it.node, Lines: *it.drop}, true, false
	}
	if out, ok := it.ev.(NodeOutput); ok {
		s.queued -= len(out.Line)
	}
	return it.ev, true, false
}

func (s *Subscription) pump() {
	defer close(s.finished)
	defer close(s.out)
	for {
		e, ok, done := s.next()
		if done {
			return
		}
		if !ok {
			<-s.wake
			continue
		}
		s.out <- e
	}
}
