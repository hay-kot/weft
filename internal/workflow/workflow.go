// Package workflow loads and validates weft workflow files.
package workflow

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// DefaultShell runs node scripts when neither the workflow nor the node sets
// `shell`.
var DefaultShell = []string{"bash", "-eo", "pipefail", "-c"}

// DefaultKillTimeout is how long a canceled node gets between SIGINT and
// SIGTERM.
const DefaultKillTimeout = 30 * time.Second

// DefaultConcurrency applies when the workflow does not set `concurrency`.
const DefaultConcurrency = 4

// Workflow is a parsed, validated workflow file.
type Workflow struct {
	// Name is the file name without extension, used for the run directory.
	Name string
	// Path is the absolute path of the workflow file.
	Path string
	// Root is the directory commands run from by default.
	Root string
	// Hash is the hex SHA-256 of the file contents.
	Hash        string
	Concurrency int
	// Nodes lists nodes in declaration order.
	Nodes []*Node

	index map[string]*Node
}

// Node is one step of a workflow.
type Node struct {
	ID              string
	Label           string
	Run             string
	Needs           []string
	Dir             string
	Env             map[string]string
	Shell           []string
	Interactive     bool
	Timeout         time.Duration
	KillTimeout     time.Duration
	ContinueOnError bool
}

// DisplayName returns the label, or the ID when no label is set.
func (n *Node) DisplayName() string {
	if n.Label != "" {
		return n.Label
	}
	return n.ID
}

// Node returns the node with the given ID, or nil.
func (w *Workflow) Node(id string) *Node {
	return w.index[id]
}

// Dependents returns the IDs of nodes that list id in their needs, in
// declaration order.
func (w *Workflow) Dependents(id string) []string {
	var out []string
	for _, n := range w.Nodes {
		if slices.Contains(n.Needs, id) {
			out = append(out, n.ID)
		}
	}
	return out
}

// Downstream returns id and every node that transitively depends on it, in
// declaration order.
func (w *Workflow) Downstream(id string) []string {
	seen := map[string]bool{id: true}
	changed := true
	for changed {
		changed = false
		for _, n := range w.Nodes {
			if seen[n.ID] {
				continue
			}
			for _, need := range n.Needs {
				if seen[need] {
					seen[n.ID] = true
					changed = true
					break
				}
			}
		}
	}

	var out []string
	for _, n := range w.Nodes {
		if seen[n.ID] {
			out = append(out, n.ID)
		}
	}
	return out
}

// Levels returns each node's level: 0 for roots, otherwise one more than the
// highest level among its needs.
func (w *Workflow) Levels() map[string]int {
	levels := make(map[string]int, len(w.Nodes))
	var visit func(n *Node) int
	visit = func(n *Node) int {
		if l, ok := levels[n.ID]; ok {
			return l
		}
		l := 0
		for _, need := range n.Needs {
			l = max(l, visit(w.index[need])+1)
		}
		levels[n.ID] = l
		return l
	}
	for _, n := range w.Nodes {
		visit(n)
	}
	return levels
}

// ValidationError lists every problem found in a workflow file.
type ValidationError struct {
	Path     string
	Problems []string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("%s: invalid workflow:\n  - %s", e.Path, strings.Join(e.Problems, "\n  - "))
}
