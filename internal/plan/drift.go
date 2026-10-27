package plan

import (
	"fmt"
	"strings"

	"github.com/hay-kot/weft/internal/store"
)

// Drift returns what changed between the last start of a run and now, in
// words for the user. Resuming after HEAD moved is how the wrong commit
// ships, so weft asks before it does. An empty result means nothing
// changed.
func Drift(last store.Start, workflowHash, gitHead string) []string {
	if last.Mode == "" {
		return nil
	}
	var out []string
	if last.WorkflowHash != "" && last.WorkflowHash != workflowHash {
		out = append(out, "the workflow file changed")
	}
	if last.GitHead != gitHead {
		out = append(out, fmt.Sprintf("git HEAD moved from %s to %s", shortHead(last.GitHead), shortHead(gitHead)))
	}
	return out
}

func shortHead(h string) string {
	switch {
	case h == "":
		return "none"
	case len(h) > 12:
		return h[:12]
	default:
		return h
	}
}

// DriftError means the workflow file or git HEAD changed since the run last
// started, and the user has not confirmed going ahead.
type DriftError struct {
	RunID   string
	Reasons []string
}

func (e *DriftError) Error() string {
	return fmt.Sprintf("since run %s last started, %s", e.RunID, strings.Join(e.Reasons, " and "))
}
