package tui

import (
	"strings"
	"time"

	"github.com/hay-kot/weft/internal/events"

	"github.com/hay-kot/weft/internal/store"
)

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func testRun() *store.Run {
	return &store.Run{
		ID:       "20261025T191200.000-a3f9",
		Dir:      "/repo/.weft/runs/deploy/20261025T191200.000-a3f9",
		Workflow: workflowName,
	}
}

func status(state events.State, errText string) *nodeStatus {
	return &nodeStatus{
		State: state, Attempt: 1, ExitCode: 0, Err: errText, Reason: "",
		Started: time.Time{}, Finished: time.Time{},
	}
}

func trimRight(s string) string { return strings.TrimRight(s, " ") }

func joinLines(lines []string) string { return strings.Join(lines, "\n") + "\n" }

// Node IDs of deployYAML.
const (
	nGuard    = "guard"
	nGoCheck  = "go-check"
	nUICheck  = "ui-check"
	nDBBackup = "db-backup"
	nRelease  = "release"
	nDeploy   = "deploy"
	nTofu     = "tofu-apply"

	workflowName = "deploy"
)
