package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/hay-kot/weft/internal/events"
)

// EventRecord is one line of events.jsonl.
type EventRecord struct {
	Seq   int          `json:"seq"`
	Time  time.Time    `json:"time"`
	Type  string       `json:"type"`
	Event events.Event `json:"event"`
}

// EventType returns the events.jsonl type name of e.
func EventType(e events.Event) string {
	switch e.(type) {
	case events.RunStarted:
		return "run_started"
	case events.NodeChanged:
		return "node_changed"
	case events.NodeOutput:
		return "node_output"
	case events.RunFinished:
		return "run_finished"
	case events.OutputDropped:
		return "output_dropped"
	case events.Barrier:
		return "barrier"
	default:
		return fmt.Sprintf("%T", e)
	}
}

// WriteEvents appends every event from ch to the run's events.jsonl until ch
// closes. Node output is left out: it is already in the per-attempt logs,
// and copying it would double the disk use and the places secrets can leak.
// A resumed run appends to the same file, and seq carries on from its last
// line.
func (r *Run) WriteEvents(ch <-chan events.Event) error {
	seq, err := countLines(r.EventsPath())
	if err != nil {
		for range ch {
		}
		return fmt.Errorf("read event log: %w", err)
	}
	f, err := os.OpenFile(r.EventsPath(), os.O_WRONLY|os.O_CREATE|os.O_APPEND, fileMode)
	if err != nil {
		// Drain so the bus never blocks on this subscriber.
		for range ch {
		}
		return fmt.Errorf("open event log: %w", err)
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)

	var firstErr error
	for e := range ch {
		switch e.(type) {
		case events.NodeOutput, events.Barrier:
			continue
		case events.RunStarted, events.NodeChanged, events.OutputDropped, events.RunFinished:
		}
		seq++
		rec := EventRecord{Seq: seq, Time: time.Now(), Type: EventType(e), Event: e}
		if err := enc.Encode(rec); err != nil && firstErr == nil {
			firstErr = err
		}
		if err := w.Flush(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if err := f.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

func countLines(path string) (int, error) {
	f, err := os.Open(path) //nolint:gosec // path is inside a run directory
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		n++
	}
	return n, sc.Err()
}
