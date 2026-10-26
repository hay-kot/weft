package store

import (
	"bufio"
	"encoding/json"
	"fmt"
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
	default:
		return fmt.Sprintf("%T", e)
	}
}

// WriteEvents appends every event from ch to the run's events.jsonl until ch
// closes. Node output is left out: it is already in the per-attempt logs,
// and copying it would double the disk use and the places secrets can leak.
func (r *Run) WriteEvents(ch <-chan events.Event) error {
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
	seq := 0
	for e := range ch {
		if _, ok := e.(events.NodeOutput); ok {
			continue
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
