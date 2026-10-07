package daggerctl

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Service is crier's run registry and control surface. It holds the bridge
// (the only thing that talks to the executor), the run store (the records
// crier owns) and the deliverer (the shipped inbox path a terminal outcome
// travels on). It contains no execution logic: every method is a request to
// the bridge plus a record update plus — once, on the first terminal
// observation — one inbox delivery.
type Service struct {
	bridge  DaggerBridge
	store   Store
	deliver Deliverer
	now     func() time.Time

	// targets is the execution-target table (CR-CHAT-035). Nil means the
	// single-bridge deployment: every create resolves to the default bridge
	// and records TargetLocal.
	targets *TargetTable

	// interval is the background watch period. Zero disables the watcher (the
	// default), which is what tests and a status-poll-only deployment want.
	interval time.Duration

	// mu serialises every operation. The store is a log and the notification
	// rule is exactly-once, so two concurrent polls must not both decide to
	// notify; one lock for the whole surface is the simplest honest answer.
	mu sync.Mutex
}

var _ Control = (*Service)(nil)

// NewService builds the control surface. A nil bridge is legal: the service
// then answers ErrUnconfigured (a named 503) instead of pretending a run
// exists. A nil deliverer means terminal outcomes are not delivered — the
// record still carries them.
func NewService(bridge DaggerBridge, store Store, deliver Deliverer) *Service {
	return &Service{bridge: bridge, store: store, deliver: deliver, now: time.Now}
}

// SetWatchInterval arms the background watcher: every d, each non-terminal run
// is polled once and a run that went terminal is delivered without anyone
// asking. A non-positive duration disables it.
func (s *Service) SetWatchInterval(d time.Duration) { s.interval = d }

// SetTargetTable installs the execution-target table (CR-CHAT-035). A nil
// table keeps the single-bridge behaviour: every create records TargetLocal
// and talks to the default bridge.
func (s *Service) SetTargetTable(t *TargetTable) { s.targets = t }

// Store exposes the run store (used by wiring code that needs the raw records).
func (s *Service) Store() Store { return s.store }

// available reports whether the surface can act at all.
func (s *Service) available() error {
	if s.bridge == nil {
		return fmt.Errorf("%w: no dagger bridge is wired (set CR_DAGGER_URL)", ErrUnconfigured)
	}
	if s.store == nil {
		return fmt.Errorf("%w: no run store is wired", ErrUnconfigured)
	}
	return nil
}

// resolveTarget maps a requested target name onto the bridge it runs on and
// the name the record must carry (CR-CHAT-035). With no table installed every
// request resolves to the default bridge under TargetLocal; with a table, a
// non-empty name must resolve or the create is refused before the bridge is
// touched. The name is resolved ONCE here — the record freezes it.
func (s *Service) resolveTarget(req string) (DaggerBridge, string, error) {
	if s.targets == nil {
		return s.bridge, TargetLocal, nil
	}
	url, err := s.targets.Resolve(req)
	if err != nil {
		return nil, "", err
	}
	if !s.targets.remote(req) {
		return s.bridge, TargetLocal, nil
	}
	bridge, err := NewHTTPBridge(url)
	if err != nil {
		return nil, "", err
	}
	return bridge, strings.ToLower(strings.TrimSpace(req)), nil
}

// CreateRun starts a prompt-driven DAG and records it. The run id comes from
// the executor; crier never mints one, because a run id crier invented would
// name nothing the executor could be asked about.
//
// The target is resolved at create time and recorded on the run (CR-CHAT-035):
// absent resolves to TargetLocal, a named remote target is refused outright if
// the table does not hold it. Every later status read of this run goes to the
// recorded target, never to a re-resolved one.
func (s *Service) CreateRun(ctx context.Context, req CreateRunRequest) (*RunRecord, error) {
	if err := s.available(); err != nil {
		return nil, err
	}
	if err := validateAgent(req.AgentID); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, fmt.Errorf("%w: prompt is required", ErrInvalidInput)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	bridge, target, err := s.resolveTarget(req.Target)
	if err != nil {
		return nil, err
	}
	view, err := bridge.CreateRun(ctx, req)
	if err != nil {
		return nil, err
	}
	rec := s.newRecord(view, req.AgentID, KindPrompt, req.Prompt, "", target)
	return s.commit(ctx, rec)
}

// RunSkill runs a skill registered with the executor and records it. The
// target follows the same resolve-once rule as CreateRun (CR-CHAT-035).
func (s *Service) RunSkill(ctx context.Context, req RunSkillRequest) (*RunRecord, error) {
	if err := s.available(); err != nil {
		return nil, err
	}
	if err := validateAgent(req.AgentID); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.Skill) == "" {
		return nil, fmt.Errorf("%w: skill is required", ErrInvalidInput)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	bridge, target, err := s.resolveTarget(req.Target)
	if err != nil {
		return nil, err
	}
	view, err := bridge.RunSkill(ctx, req)
	if err != nil {
		return nil, err
	}
	rec := s.newRecord(view, req.AgentID, KindSkill, "", req.Skill, target)
	return s.commit(ctx, rec)
}

// RunStatus observes a run and returns its record. The bridge's answer wins;
// the stored record is what carries the requesting agent and the notification
// state forward.
//
// The observation goes to the run's RECORDED target (CR-CHAT-035), never to a
// re-resolved one. When the target is remote and the bridge cannot be reached
// while the run is non-terminal, the record is marked link_lost (with a
// timestamp) and returned — an explicit held/unknown indication, never an
// invented success or failure. A later successful observation clears it. A
// LOCAL bridge failure stays a returned error: nothing in the record is
// fabricated either way.
func (s *Service) RunStatus(ctx context.Context, runID string) (*RunRecord, error) {
	if err := s.available(); err != nil {
		return nil, err
	}
	if err := validateRunID(runID); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	rec, err := s.store.Get(ctx, runID)
	if err != nil {
		return nil, err
	}
	bridge, err := s.targetBridge(rec.Target)
	if err != nil {
		return nil, err
	}
	view, err := bridge.RunStatus(ctx, runID)
	if err != nil {
		return nil, s.markLinkLost(ctx, rec, err)
	}
	s.clearLinkLost(rec)
	s.applyView(rec, view)
	return s.commit(ctx, rec)
}

// markLinkLost records a lost link on a REMOTE, non-terminal run and returns
// the error unchanged (CR-CHAT-035). The record persists with link_lost set
// and its state EXACTLY as last reported — a lost link is never allowed to
// read as a terminal outcome. A local run's failure and a terminal run's
// failure are returned without touching the record.
func (s *Service) markLinkLost(ctx context.Context, rec *RunRecord, bridgeErr error) error {
	if bridgeErr == nil || rec == nil || rec.State.Terminal() {
		return bridgeErr
	}
	if s.targets == nil || !s.targets.remote(rec.Target) {
		return bridgeErr
	}
	now := s.now()
	rec.LinkLost = true
	rec.LinkLostAt = &now
	rec.UpdatedAt = now
	if err := s.store.Append(ctx, rec); err != nil {
		return bridgeErr
	}
	return fmt.Errorf("%w: (run %s on target %q is held — link lost, state stays %q)", bridgeErr, rec.RunID, rec.Target, rec.State)
}

// clearLinkLost resets the link-lost indication once an observation succeeded
// (CR-CHAT-035). It must precede applyView so the same observation that proves
// the link also carries the fresh state.
func (s *Service) clearLinkLost(rec *RunRecord) {
	if rec == nil {
		return
	}
	rec.LinkLost = false
	rec.LinkLostAt = nil
}

// targetBridge returns the bridge that speaks to a RECORDED target name. The
// recorded name is authoritative: a target removed from the table after the
// run was created still resolves from the record's name, or refuses loudly —
// it is never silently re-routed to the local bridge (CR-CHAT-035).
func (s *Service) targetBridge(recorded string) (DaggerBridge, error) {
	if s.targets == nil {
		return s.bridge, nil
	}
	return s.targets.BridgeFor(recorded, s.bridge)
}

// Cancel cancels a running DAG. A bridge answer naming the resulting state
// wins; when the bridge reports none, cancel's own semantics name it —
// cancelling a run is the vocabulary's StateCancelled.
func (s *Service) Cancel(ctx context.Context, runID string) (*RunRecord, error) {
	return s.operate(ctx, runID, StateCancelled, func(ctx context.Context) (*RunView, error) {
		return s.bridge.Cancel(ctx, runID)
	})
}

// Resume continues a paused DAG. As with Cancel, an unnamed resulting state
// falls back to the operation's own semantics: a resumed run is running.
func (s *Service) Resume(ctx context.Context, runID string) (*RunRecord, error) {
	return s.operate(ctx, runID, StateRunning, func(ctx context.Context) (*RunView, error) {
		return s.bridge.Resume(ctx, runID)
	})
}

// Rewind discards checkpoints from nodeID onward. nodeID is passed to the
// executor verbatim — crier never validates a node against a graph it does not
// hold.
func (s *Service) Rewind(ctx context.Context, runID, nodeID string) (*RunRecord, error) {
	if strings.TrimSpace(nodeID) == "" {
		return nil, fmt.Errorf("%w: node_id is required", ErrInvalidInput)
	}
	return s.operate(ctx, runID, StateRunning, func(ctx context.Context) (*RunView, error) {
		return s.bridge.Rewind(ctx, runID, nodeID)
	})
}

// operate is the shared shape of the three mutating verbs: load the record,
// ask the bridge, fold the answer in, commit.
func (s *Service) operate(ctx context.Context, runID string, fallback RunState, op func(context.Context) (*RunView, error)) (*RunRecord, error) {
	if err := s.available(); err != nil {
		return nil, err
	}
	if err := validateRunID(runID); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	rec, err := s.store.Get(ctx, runID)
	if err != nil {
		return nil, err
	}
	view, err := op(ctx)
	if err != nil {
		return nil, err
	}
	s.applyView(rec, view)
	if view.State == "" {
		// The executor answered the operation but named no resulting state.
		// The operation's own meaning is then the honest reading: a cancel
		// cancelled, a resume is running. An UNRECOGNISED word is left as
		// StateUnknown — crier does not overwrite a report it could not read.
		rec.State = fallback
	}
	return s.commit(ctx, rec)
}

// newRecord builds the first version of a run's record. target is the
// execution target resolved at create time (CR-CHAT-035) — recorded verbatim
// and never re-resolved.
func (s *Service) newRecord(view *RunView, agentID, kind, prompt, skill, target string) *RunRecord {
	state := view.State
	if state == "" {
		// The bridge confirmed the create, so the run exists and has not
		// finished; a status word it did not supply cannot be invented, but
		// "not finished" is exactly the vocabulary's non-terminal value. An
		// unrecognised WORD still lands on StateUnknown via normalizeState.
		state = StateRunning
	}
	if strings.TrimSpace(target) == "" {
		target = TargetLocal
	}
	now := s.now()
	return &RunRecord{
		RunID:           view.RunID,
		State:           state,
		RequestingAgent: agentID,
		Kind:            kind,
		Prompt:          prompt,
		Skill:           skill,
		Target:          target,
		Evidence:        view.Evidence,
		Nodes:           view.Nodes,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
}

// applyView folds one status observation into the record. A field the
// observation did not carry is left as it was — a status read that omits
// evidence does not erase the evidence a previous read reported.
func (s *Service) applyView(rec *RunRecord, view *RunView) {
	if rec == nil || view == nil {
		return
	}
	if view.State != "" {
		rec.State = view.State
	}
	if view.Evidence != nil {
		rec.Evidence = view.Evidence
	}
	if view.Nodes != nil {
		rec.Nodes = view.Nodes
	}
	rec.UpdatedAt = s.now()
}

// commit notifies on a first terminal observation and persists the version.
func (s *Service) commit(ctx context.Context, rec *RunRecord) (*RunRecord, error) {
	if rec != nil && rec.State.Terminal() && !rec.Notified {
		s.notify(rec)
	}
	if err := s.store.Append(ctx, rec); err != nil {
		return nil, err
	}
	return cloneRecord(rec), nil
}

// notify delivers the terminal outcome to the requesting agent's inbox through
// the shipped delivery path. It is idempotent through rec.Notified, which is
// persisted with the record: a repeated status poll cannot deliver twice.
//
// A delivery failure does not fail the caller — the run's state is already
// known and readable — but it is never silent: the reason is recorded on the
// record as notify_error and the record is persisted with it.
func (s *Service) notify(rec *RunRecord) {
	if rec == nil || rec.Notified || s.deliver == nil {
		return
	}
	payload, err := json.Marshal(RunNotification{
		Kind:            "dagger_run",
		Code:            notificationCode(rec.State),
		RunID:           rec.RunID,
		State:           rec.State,
		RequestingAgent: rec.RequestingAgent,
		Prompt:          rec.Prompt,
		Skill:           rec.Skill,
		Evidence:        rec.Evidence,
		Nodes:           rec.Nodes,
		UpdatedAt:       rec.UpdatedAt,
	})
	if err != nil {
		rec.NotifyError = "encode notification: " + err.Error()
		return
	}
	if err := s.deliver.DeliverToInbox(rec.RequestingAgent, payload); err != nil {
		rec.NotifyError = err.Error()
		return
	}
	rec.Notified = true
	rec.NotifyError = ""
}

// Watch polls every non-terminal run until ctx is done. It is how a run that
// finishes on its own still reaches its requester without the requester
// polling — the outcome arrives in the inbox, not on a socket.
func (s *Service) Watch(ctx context.Context) {
	if s.interval <= 0 || s.bridge == nil || s.store == nil {
		return
	}
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.pollOnce(ctx)
		}
	}
}

// pollOnce is one watcher pass: observe every non-terminal run, best effort.
// A failure here is not fatal to the watcher — the next pass retries — but it
// is logged, so a bridge that is down is visible.
func (s *Service) pollOnce(ctx context.Context) {
	recs, err := s.store.List(ctx)
	if err != nil {
		slog.Warn("dagger watch: list runs", "error", err)
		return
	}
	for _, rec := range recs {
		if rec.State.Terminal() || rec.Notified {
			continue
		}
		if _, err := s.RunStatus(ctx, rec.RunID); err != nil {
			slog.Warn("dagger watch: poll run", "run_id", rec.RunID, "error", err)
		}
	}
}
