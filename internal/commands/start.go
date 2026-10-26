package commands

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/hay-kot/weft/internal/events"
	"github.com/hay-kot/weft/internal/plan"
	"github.com/hay-kot/weft/internal/store"
	"github.com/hay-kot/weft/internal/workflow"
)

// selection is what --resume, --from, --only, and --force ask for.
type selection struct {
	resume bool
	from   string
	only   []string
	force  bool
}

// mode returns the store.Mode* value for the selection.
func (s selection) mode() string {
	switch {
	case s.resume:
		return store.ModeResume
	case s.from != "":
		return store.ModeFrom
	case len(s.only) > 0:
		return store.ModeOnly
	default:
		return store.ModeRun
	}
}

// flag returns the flag as the user typed it, for messages.
func (s selection) flag() string {
	switch s.mode() {
	case store.ModeResume:
		return "--resume"
	case store.ModeFrom:
		return "--from " + s.from
	case store.ModeOnly:
		return "--only " + strings.Join(s.only, ",")
	default:
		return ""
	}
}

func (s selection) validate() error {
	n := 0
	for _, set := range []bool{s.resume, s.from != "", len(s.only) > 0} {
		if set {
			n++
		}
	}
	if n > 1 {
		return errors.New("--resume, --from, and --only can't be combined; pick one")
	}
	if s.force && n == 0 {
		return errors.New("--force only applies with --resume, --from, or --only")
	}
	return nil
}

// errNothingToRun means a resume found every node already succeeded.
var errNothingToRun = errors.New("nothing to run")

// prepared is a run ready to start: its directory, held lock, plan, and
// the state the engine will keep writing.
type prepared struct {
	run  *store.Run
	lock *store.Lock
	plan plan.Plan
	// prior is the earlier run's state when the plan continues it.
	prior *store.State
	state *store.State
	// drift lists what changed since the earlier run last started. Going
	// ahead needs --force or the user's confirmation.
	drift []string
	// alive lists lost attempts whose process group still exists.
	alive []lostAttempt
	head  string
}

// lostAttempt is an attempt the earlier state shows as running: the weft
// that ran it exited without seeing it end.
type lostAttempt struct {
	node    string
	attempt int
	pgid    int
}

// prepare finds or creates the run directory for sel and plans it. It
// leaves the run locked; the caller releases prep.lock.
func prepare(ctx context.Context, wf *workflow.Workflow, sel selection, now time.Time) (*prepared, error) {
	head := gitHead(ctx, wf.Root)
	prep := &prepared{
		run: nil, lock: nil, plan: plan.Fresh(wf), prior: nil, state: nil,
		drift: nil, alive: nil, head: head,
	}

	if sel.mode() != store.ModeRun {
		if err := prep.planFromLatest(wf, sel); err != nil {
			prep.lock.Release()
			return nil, err
		}
	}
	if !prep.plan.Continue {
		prep.lock.Release()
		if err := prep.createRun(wf, now); err != nil {
			return nil, err
		}
	}

	if prep.prior != nil {
		prep.drift = plan.Drift(prep.prior.Last(), wf.Hash, head)
		prep.alive = aliveAttempts(wf, prep.prior)
		prep.state = prep.prior.Clone()
		prep.state.WorkflowPath = wf.Path
	} else {
		prep.state = store.NewState(prep.run, wf.Path, now)
	}
	prep.state.History = append(prep.state.History, store.Start{
		At: now, PID: os.Getpid(), Mode: prep.plan.Mode, Nodes: prep.plan.Selection,
		WorkflowHash: wf.Hash, GitHead: head, Forced: len(prep.drift) > 0,
	})
	return prep, nil
}

// planFromLatest locks the latest run, reads its state, and plans sel
// against it. When the plan does not need the latest run, the lock stays
// held only until prepare creates the new run.
func (p *prepared) planFromLatest(wf *workflow.Workflow, sel selection) error {
	latest, err := store.Latest(wf.Root, wf.Name)
	switch {
	case errors.Is(err, store.ErrNoRuns) && sel.resume:
		return fmt.Errorf("--resume: workflow %s has no earlier run to resume; start one with weft run %s", wf.Name, wf.Name)
	case errors.Is(err, store.ErrNoRuns):
	case err != nil:
		return fmt.Errorf("find the latest run: %w", err)
	default:
		if p.lock, err = latest.Lock(); err != nil {
			if locked, ok := errors.AsType[*store.LockedError](err); ok {
				return fmt.Errorf("%s: %w; wait for it to finish, or stop it, then try again", sel.flag(), locked)
			}
			return err
		}
		p.run = latest
	}

	var stateErr error
	if p.run != nil {
		p.prior, stateErr = p.run.LoadState()
		if stateErr != nil && !errors.Is(stateErr, store.ErrNoState) {
			return fmt.Errorf("%s: %w", sel.flag(), stateErr)
		}
	}
	if sel.resume {
		if stateErr != nil {
			return fmt.Errorf("--resume: run %s has no state.json, so weft can't tell which nodes succeeded; start a new run", p.run.ID)
		}
		p.plan = plan.Resume(wf, p.prior)
		if len(p.plan.Pending(wf)) == 0 {
			return fmt.Errorf("run %s: %w: every node already succeeded", p.run.ID, errNothingToRun)
		}
		return nil
	}

	if sel.from != "" {
		p.plan, err = plan.From(wf, p.prior, sel.from)
	} else {
		p.plan, err = plan.Only(wf, p.prior, sel.only)
	}
	if unmet, ok := errors.AsType[*plan.UnmetNeedError](err); ok && unmet.RunID == "" && stateErr != nil {
		return fmt.Errorf("%s: %s needs %s, which is not selected, and the latest run %s has no state.json to take its result from", sel.flag(), unmet.Node, unmet.Need, p.run.ID)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", sel.flag(), err)
	}
	if !p.plan.Continue {
		p.prior = nil
	}
	return nil
}

func (p *prepared) createRun(wf *workflow.Workflow, now time.Time) error {
	// Prune before creating so the new run is one of the kept ones.
	if err := store.Prune(wf.Root, wf.Name, store.KeepRuns-1); err != nil {
		// Old runs only cost disk; the new run doesn't depend on them.
		log.Warn().Err(err).Msg("prune old runs")
	}
	run, err := store.Create(wf.Root, wf.Name, now)
	if err != nil {
		return err
	}
	lock, err := run.Lock()
	if err != nil {
		return err
	}
	p.run, p.lock = run, lock
	return nil
}

// recordLost writes an attempt record for each attempt the earlier weft
// lost track of, so the run directory says what happened to it. A record
// that exists is left alone: the attempt ended before weft exited.
func (p *prepared) recordLost(wf *workflow.Workflow) error {
	if p.prior == nil {
		return nil
	}
	for _, id := range plan.Lost(wf, p.prior) {
		ns := p.prior.Nodes[id]
		if _, err := os.Stat(p.run.AttemptRecordPath(id, ns.Attempt)); !errors.Is(err, fs.ErrNotExist) {
			continue
		}
		n := wf.Node(id)
		rec := store.AttemptRecord{
			Node: id, Attempt: ns.Attempt, Command: n.Run, Shell: n.Shell, Dir: filepath.Join(wf.Root, n.Dir), Interactive: n.Interactive,
			State: string(events.Failed), Reason: events.ReasonAbandoned, ExitCode: -1, Signal: "",
			Error:   "weft exited while this attempt was running; it never saw the attempt end",
			Started: ns.Started, Finished: p.prior.Updated, Log: ns.Log,
		}
		if err := p.run.WriteAttemptRecord(rec); err != nil {
			return fmt.Errorf("record lost attempt %d of %s: %w", ns.Attempt, id, err)
		}
	}
	return nil
}

// aliveAttempts returns the lost attempts whose process group still has
// processes. A pgid can be reused after its group is gone, so this can
// report an unrelated process; --force goes ahead regardless.
func aliveAttempts(wf *workflow.Workflow, prior *store.State) []lostAttempt {
	var out []lostAttempt
	for _, id := range plan.Lost(wf, prior) {
		ns := prior.Nodes[id]
		if ns.PGID > 0 && groupAlive(ns.PGID) {
			out = append(out, lostAttempt{node: id, attempt: ns.Attempt, pgid: ns.PGID})
		}
	}
	return out
}

func groupAlive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// aliveError explains lost attempts that may still be running.
func aliveError(sel selection, runID string, alive []lostAttempt) error {
	parts := make([]string, 0, len(alive))
	for _, a := range alive {
		parts = append(parts, fmt.Sprintf("%s (attempt %d, process group %d)", a.node, a.attempt, a.pgid))
	}
	return fmt.Errorf("%s: run %s was cut off while nodes ran, and their processes may still be running: %s. "+
		"Stop them (kill -INT -<group>) or let them finish, then try again. --force goes ahead anyway",
		sel.flag(), runID, strings.Join(parts, ", "))
}

// driftError explains why a continued run needs --force.
func driftError(sel selection, runID string, drift []string) error {
	return fmt.Errorf("%s takes results from run %s, but since it last started %s. "+
		"Those results may not match what would run now. Pass --force to go ahead anyway, or start a new run",
		sel.flag(), runID, strings.Join(drift, " and "))
}

// gitHeadTimeout bounds the git call; a hung git must not hold up a run.
const gitHeadTimeout = 5 * time.Second

// gitHead returns the commit checked out at dir, or empty when dir is not
// in a git repository or git is unavailable.
func gitHead(ctx context.Context, dir string) string {
	ctx, cancel := context.WithTimeout(ctx, gitHeadTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--verify", "--quiet", "HEAD") //nolint:gosec // dir is the workflow root, passed as one argument
	// git must not prompt or wait on anything; it reads the repository only.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// startNotice describes a run that is not a plain fresh run, or returns
// empty for one that is.
func startNotice(wf *workflow.Workflow, p plan.Plan, runID string, sel selection) string {
	var run, skipped, excluded []string
	for _, n := range wf.Nodes {
		switch p.Nodes[n.ID].State {
		case events.Skipped:
			skipped = append(skipped, n.ID)
		case events.Excluded:
			excluded = append(excluded, n.ID)
		default:
			run = append(run, n.ID)
		}
	}
	if sel.mode() == store.ModeRun {
		return ""
	}
	var b strings.Builder
	if p.Continue {
		fmt.Fprintf(&b, "%s continues run %s: ", sel.flag(), runID)
	} else {
		fmt.Fprintf(&b, "%s: ", sel.flag())
	}
	fmt.Fprintf(&b, "running %s", strings.Join(run, ", "))
	if len(skipped) > 0 {
		fmt.Fprintf(&b, "; skipping %d that succeeded", len(skipped))
	}
	if len(excluded) > 0 {
		fmt.Fprintf(&b, "; leaving out %s", strings.Join(excluded, ", "))
	}
	return b.String()
}
