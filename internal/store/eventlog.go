package store

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	seq, terminated, err := countLines(r.EventsPath())
	if err != nil {
		for range ch {
		}
		return fmt.Errorf("read event log: %w", err)
	}
	f, err := appendRegular(r.EventsPath())
	if err != nil {
		// Drain so the bus never blocks on this subscriber.
		for range ch {
		}
		return fmt.Errorf("open event log: %w", err)
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)

	var firstErr error
	if !terminated {
		// A weft killed mid-write left a cut-off record. It took a seq above;
		// ending its line keeps the next record parseable on its own.
		if _, err := w.WriteString("\n"); err != nil {
			firstErr = err
		}
	}
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

// countLines counts the lines of the file at path, a cut-off last line
// included, and reports whether that last line ends with a newline. It
// counts bytes rather than scanning lines, so no line is too long for it.
func countLines(path string) (n int, terminated bool, err error) {
	f, err := OpenRegular(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, true, nil
	}
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, 64<<10)
	last := byte('\n')
	for {
		k, rerr := f.Read(buf)
		if k > 0 {
			n += bytes.Count(buf[:k], []byte{'\n'})
			last = buf[k-1] //nolint:gosec // k > 0 was checked
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return 0, false, rerr
		}
	}
	if last != '\n' {
		return n + 1, false, nil
	}
	return n, true, nil
}
