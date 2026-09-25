package detect

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/crier-dev/crier/internal/registry"
)

// Documented default thresholds. They are exported so the docs gate can pin
// the numbers the docs print to the constants the server really uses
// (docs/claims.yaml), never to a copy.
const (
	// DefaultFanoutWindow is the sliding window fan-out is measured over.
	DefaultFanoutWindow = 60 * time.Second
	// DefaultFanoutMinTargets is how many DISTINCT targets one sender must
	// reach inside the window to trip fanout_spike.
	DefaultFanoutMinTargets = 5
	// DefaultNewPeerWindow is the sliding window first-contact pairs are
	// measured over.
	DefaultNewPeerWindow = 60 * time.Second
	// DefaultNewPeerMinTargets is how many first-ever sender→target pairs
	// inside the window trip new_peer_burst.
	DefaultNewPeerMinTargets = 3
	// DefaultQuietStartHour and DefaultQuietEndHour bound the odd-hour window
	// in UTC (start inclusive, end exclusive).
	DefaultQuietStartHour = 1
	DefaultQuietEndHour   = 5
	// DefaultQuietMinMessages is how many deliveries from one sender inside
	// the quiet window trip odd_hour_volume.
	DefaultQuietMinMessages = 3
	// DefaultCanaryCount is how many canary tokens the server generates at
	// boot when the operator supplies none.
	DefaultCanaryCount = 2
)

// Signal names. These strings are the contract: they are what
// GET /alerts reports, what the delivery log records and what the spec
// documents.
const (
	// SignalFanoutSpike — one sender reached N distinct targets inside the
	// fan-out window. The signature of a compromised agent doing mass
	// outreach.
	SignalFanoutSpike = "fanout_spike"
	// SignalNewPeerBurst — one sender opened N first-ever conversations
	// inside the window. A new peer is normal; a burst of them is an agent
	// that had never talked to anyone mapping the bus.
	SignalNewPeerBurst = "new_peer_burst"
	// SignalOddHourVolume — one sender delivered N messages inside the
	// documented quiet window.
	SignalOddHourVolume = "odd_hour_volume"
	// SignalCanaryTrip — a delivery was addressed to a canary id or carried a
	// canary token in its payload. There is no benign explanation for this
	// one.
	SignalCanaryTrip = "canary_trip"
)

// Severity levels.
const (
	SeverityWarning  = "warning"
	SeverityCritical = "critical"
)

// Config tunes the detector. The zero value is valid: every window and
// threshold falls back to its documented default.
type Config struct {
	// LogPath is the delivery-log file. Empty disables the log (alerts keep
	// working in memory).
	LogPath string
	// KeyPath is the ed25519 signing-key file for the log. Required when
	// LogPath is set.
	KeyPath string
	// Windows and thresholds; zero means the Default* constant above.
	FanoutWindow      time.Duration
	FanoutMinTargets  int
	NewPeerWindow     time.Duration
	NewPeerMinTargets int
	// QuietStartHour/QuietEndHour bound the odd-hour window in UTC (start
	// inclusive, end exclusive, wrapping past midnight). An EQUAL pair
	// disables the signal; config.Load resolves "unset" to the documented
	// default (01:00–05:00) because the zero pair cannot mean both things.
	QuietStartHour   int
	QuietEndHour     int
	QuietMinMessages int
	// CanaryTokens are the operator-planted canary tokens. Empty means the
	// server generates DefaultCanaryCount of them at boot (and logs them).
	CanaryTokens []string
}

func (c Config) withDefaults() Config {
	if c.FanoutWindow <= 0 {
		c.FanoutWindow = DefaultFanoutWindow
	}
	if c.FanoutMinTargets <= 0 {
		c.FanoutMinTargets = DefaultFanoutMinTargets
	}
	if c.NewPeerWindow <= 0 {
		c.NewPeerWindow = DefaultNewPeerWindow
	}
	if c.NewPeerMinTargets <= 0 {
		c.NewPeerMinTargets = DefaultNewPeerMinTargets
	}
	if c.QuietMinMessages <= 0 {
		c.QuietMinMessages = DefaultQuietMinMessages
	}
	c.QuietStartHour = ((c.QuietStartHour % 24) + 24) % 24
	c.QuietEndHour = ((c.QuietEndHour % 24) + 24) % 24
	return c
}

// Alert is one trip of one baseline signal.
type Alert struct {
	ID       string         `json:"id"`
	At       string         `json:"at"`
	Signal   string         `json:"signal"`
	Agent    string         `json:"agent"`
	Severity string         `json:"severity"`
	Detail   string         `json:"detail"`
	Evidence map[string]any `json:"evidence"`
}

// Quarantine is one contained agent.
type Quarantine struct {
	AgentID string `json:"agent_id"`
	At      string `json:"at"`
	Reason  string `json:"reason,omitempty"`
}

// Canary is one planted canary token with the agent id that stands for it.
type Canary struct {
	ID    string `json:"id"`
	Token string `json:"token"`
}

// RegistryStore is the part of the registry store containment needs.
type RegistryStore interface {
	Get(id string) (*registry.Agent, error)
	Unregister(id string) error
}

// WebhookPauser is the part of the webhook driver containment needs:
// PauseAgent stops the agent's outbound webhook lane and returns how many
// queued deliveries it dropped.
type WebhookPauser interface {
	PauseAgent(agentID string) (int, error)
}

// LeaseRevoker is implemented by stores that can release an agent's unacked
// leases (registry.MemoryStore and registry.PostgresStore do).
type LeaseRevoker interface {
	RevokeLeases(agentID string) (int, error)
}

type stamped struct {
	target string
	at     time.Time
}

// Detector observes deliveries, keeps the per-sender baselines, raises alerts,
// answers quarantine questions and performs containment. It is the
// registry.Detector the handler calls; every method is safe for concurrent
// use.
type Detector struct {
	cfg Config
	// Now is the clock. Tests replace it; production leaves it nil (time.Now).
	Now func() time.Time

	audit *AuditLog

	mu           sync.Mutex
	alerts       []Alert
	alertSeq     int
	quarantined  map[string]Quarantine
	canaries     []Canary
	canaryIDs    map[string]bool
	canaryTokens [][]byte
	fanout       map[string][]stamped
	seenPairs    map[string]map[string]bool
	newPairs     map[string][]stamped
	quiet        map[string][]time.Time
	lastAlert    map[string]time.Time
	observed     int
	verdicts     map[string]int

	store    RegistryStore
	webhooks WebhookPauser
	lastErr  error
}

// New builds a detector. cfg.LogPath empty means no audit log (the alerts and
// containment still work; nothing is persisted).
func New(cfg Config) (*Detector, error) {
	cfg = cfg.withDefaults()
	d := &Detector{
		cfg:         cfg,
		audit:       nil,
		quarantined: map[string]Quarantine{},
		canaryIDs:   map[string]bool{},
		fanout:      map[string][]stamped{},
		seenPairs:   map[string]map[string]bool{},
		newPairs:    map[string][]stamped{},
		quiet:       map[string][]time.Time{},
		lastAlert:   map[string]time.Time{},
		verdicts:    map[string]int{},
	}
	if cfg.LogPath != "" {
		key, err := LoadOrCreateSigner(cfg.KeyPath)
		if err != nil {
			return nil, err
		}
		log, err := OpenAuditLog(cfg.LogPath, key)
		if err != nil {
			return nil, err
		}
		d.audit = log
	}
	d.seedCanaries(cfg.CanaryTokens)
	return d, nil
}

// seedCanaries plants the canary tokens. Operator-supplied tokens get a
// DETERMINISTIC id (so a canary id is stable across restarts and can be
// planted in an allowlist); generated tokens rotate per boot, which the spec
// states.
func (d *Detector) seedCanaries(tokens []string) {
	if len(tokens) == 0 {
		for i := 0; i < DefaultCanaryCount; i++ {
			buf := make([]byte, 16)
			if _, err := rand.Read(buf); err != nil {
				return
			}
			tokens = append(tokens, "crier-canary-"+hex.EncodeToString(buf))
		}
	}
	for _, tok := range tokens {
		if tok == "" {
			continue
		}
		sum := sha256.Sum256([]byte(tok))
		id := "canary-" + hex.EncodeToString(sum[:6])
		d.canaries = append(d.canaries, Canary{ID: id, Token: tok})
		d.canaryIDs[id] = true
		d.canaryTokens = append(d.canaryTokens, []byte(tok))
	}
}

// SetStore wires the registry store containment acts on.
func (d *Detector) SetStore(s RegistryStore) { d.store = s }

// SetWebhookPauser wires the webhook driver containment pauses.
func (d *Detector) SetWebhooks(w WebhookPauser) { d.webhooks = w }

// Audit returns the delivery log (nil when none is configured).
func (d *Detector) Audit() *AuditLog { return d.audit }

// Canaries returns the planted canaries (tokens included: they are planted
// secrets an operator must be able to read, and the route is authenticated).
func (d *Detector) Canaries() []Canary {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Canary, len(d.canaries))
	copy(out, d.canaries)
	return out
}

// Config returns the resolved configuration (defaults applied).
func (d *Detector) Config() Config { return d.cfg }

func (d *Detector) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// Observe records one delivery outcome and runs the baselines over it. It
// implements registry.Detector.
func (d *Detector) Observe(obs registry.DeliveryObservation) {
	if d == nil {
		return
	}
	at := obs.At
	if at.IsZero() {
		at = d.now()
	}
	at = at.UTC()

	// 1. The log line: who sent what to whom, when, and the verdict.
	d.record(Entry{
		At:        at.Format(time.RFC3339Nano),
		Kind:      KindDelivery,
		Sender:    obs.Sender,
		Target:    obs.Target,
		MessageID: obs.MessageID,
		Verdict:   obs.Verdict,
		Transport: obs.Transport,
	})

	// 2. Canaries first: a trip is never a baseline question.
	if trip, canary := d.canaryTrip(obs); trip {
		d.raise(Alert{
			Signal:   SignalCanaryTrip,
			Agent:    obs.Sender,
			Severity: SeverityCritical,
			Detail: fmt.Sprintf("delivery to %s carried canary token of %s (verdict %s): an agent touched a planted secret",
				describeTarget(obs.Target), canary.ID, obs.Verdict),
			Evidence: map[string]any{
				"canary_id":  canary.ID,
				"target":     obs.Target,
				"message_id": obs.MessageID,
				"verdict":    obs.Verdict,
				"threshold":  1,
			},
		}, at)
	}

	// 3. Baselines are about ACCEPTED outreach and the bus's own refusals. A
	// delivery the detector itself refused (quarantined) is not behaviour —
	// counting it would keep an already-contained agent "spiking" forever.
	if obs.Sender == "" || obs.Verdict == registry.VerdictQuarantined {
		d.countVerdict(obs.Verdict)
		return
	}
	d.baselines(obs, at)
	d.countVerdict(obs.Verdict)
}

func describeTarget(t string) string {
	if t == "" {
		return "(no target)"
	}
	return t
}

func (d *Detector) countVerdict(v string) {
	d.mu.Lock()
	d.observed++
	if v != "" {
		d.verdicts[v]++
	}
	d.mu.Unlock()
}

// canaryTrip reports whether this delivery touched a canary — either because
// it was addressed to a canary id, or because a canary token rode in the
// payload (the exfiltration case the token exists for).
//
// Payloads above maxCanaryScanBytes are not scanned for tokens (the address
// check still applies): scanning megabytes on the delivery path would be a
// self-inflicted denial of service, and the cap is documented.
func (d *Detector) canaryTrip(obs registry.DeliveryObservation) (bool, Canary) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.canaries) == 0 {
		return false, Canary{}
	}
	for _, c := range d.canaries {
		if obs.Target != "" && obs.Target == c.ID {
			return true, c
		}
	}
	if len(obs.Payload) > 0 && len(obs.Payload) <= maxCanaryScanBytes {
		for i, tok := range d.canaryTokens {
			if bytes.Contains(obs.Payload, tok) {
				return true, d.canaries[i]
			}
		}
	}
	return false, Canary{}
}

// maxCanaryScanBytes bounds the canary payload scan.
const maxCanaryScanBytes = 64 * 1024

// baselines feeds one delivery into the three behaviour signals.
func (d *Detector) baselines(obs registry.DeliveryObservation, at time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()

	sender, target := obs.Sender, obs.Target

	// --- fan-out -----------------------------------------------------------------
	if target != "" {
		ev := append(d.fanout[sender], stamped{target: target, at: at})
		cut := at.Add(-d.cfg.FanoutWindow)
		kept := ev[:0]
		for _, s := range ev {
			if !s.at.Before(cut) {
				kept = append(kept, s)
			}
		}
		d.fanout[sender] = kept
		distinct := distinctTargets(kept)
		if len(distinct) >= d.cfg.FanoutMinTargets && d.alertDueLocked(SignalFanoutSpike, sender, at, d.cfg.FanoutWindow) {
			d.lastAlert[SignalFanoutSpike+"\x00"+sender] = at
			d.mu.Unlock()
			d.raise(Alert{
				Signal:   SignalFanoutSpike,
				Agent:    sender,
				Severity: SeverityCritical,
				Detail: fmt.Sprintf("%s reached %d distinct targets within %s (threshold %d)",
					sender, len(distinct), d.cfg.FanoutWindow, d.cfg.FanoutMinTargets),
				Evidence: map[string]any{
					"window_seconds":   int(d.cfg.FanoutWindow.Seconds()),
					"distinct_targets": len(distinct),
					"targets":          distinct,
					"threshold":        d.cfg.FanoutMinTargets,
				},
			}, at)
			d.mu.Lock()
		}
	}

	// --- new-peer burst ----------------------------------------------------------
	if target != "" {
		if d.seenPairs[sender] == nil {
			d.seenPairs[sender] = map[string]bool{}
		}
		if !d.seenPairs[sender][target] {
			d.seenPairs[sender][target] = true
			ev := append(d.newPairs[sender], stamped{target: target, at: at})
			cut := at.Add(-d.cfg.NewPeerWindow)
			kept := ev[:0]
			for _, s := range ev {
				if !s.at.Before(cut) {
					kept = append(kept, s)
				}
			}
			d.newPairs[sender] = kept
			if len(kept) >= d.cfg.NewPeerMinTargets && d.alertDueLocked(SignalNewPeerBurst, sender, at, d.cfg.NewPeerWindow) {
				targets := make([]string, 0, len(kept))
				for _, s := range kept {
					targets = append(targets, s.target)
				}
				d.lastAlert[SignalNewPeerBurst+"\x00"+sender] = at
				d.mu.Unlock()
				d.raise(Alert{
					Signal:   SignalNewPeerBurst,
					Agent:    sender,
					Severity: SeverityCritical,
					Detail: fmt.Sprintf("%s opened %d first-ever conversations within %s (threshold %d)",
						sender, len(kept), d.cfg.NewPeerWindow, d.cfg.NewPeerMinTargets),
					Evidence: map[string]any{
						"window_seconds": int(d.cfg.NewPeerWindow.Seconds()),
						"new_peers":      len(kept),
						"targets":        targets,
						"threshold":      d.cfg.NewPeerMinTargets,
					},
				}, at)
				d.mu.Lock()
			}
		}
	}

	// --- odd-hour volume ---------------------------------------------------------
	if d.inQuietHours(at) {
		ev := append(d.quiet[sender], at)
		cut := at.Add(-d.cfg.FanoutWindow)
		kept := ev[:0]
		for _, t := range ev {
			if !t.Before(cut) {
				kept = append(kept, t)
			}
		}
		d.quiet[sender] = kept
		if len(kept) >= d.cfg.QuietMinMessages && d.alertDueLocked(SignalOddHourVolume, sender, at, d.cfg.FanoutWindow) {
			d.lastAlert[SignalOddHourVolume+"\x00"+sender] = at
			d.mu.Unlock()
			d.raise(Alert{
				Signal:   SignalOddHourVolume,
				Agent:    sender,
				Severity: SeverityWarning,
				Detail: fmt.Sprintf("%s delivered %d messages inside the quiet window %02d:00-%02d:00 UTC (threshold %d)",
					sender, len(kept), d.cfg.QuietStartHour, d.cfg.QuietEndHour, d.cfg.QuietMinMessages),
				Evidence: map[string]any{
					"quiet_hours_utc": fmt.Sprintf("%02d:00-%02d:00", d.cfg.QuietStartHour, d.cfg.QuietEndHour),
					"messages":        len(kept),
					"threshold":       d.cfg.QuietMinMessages,
				},
			}, at)
			d.mu.Lock()
		}
	}
}

// alertDueLocked reports whether this signal may fire for this agent now: one
// alert per (signal, agent) per window, so a sustained spike is one alert and
// not one per message.
func (d *Detector) alertDueLocked(signal, agent string, at time.Time, window time.Duration) bool {
	last, ok := d.lastAlert[signal+"\x00"+agent]
	if !ok {
		return true
	}
	return !at.Before(last.Add(window))
}

// inQuietHours reports whether the UTC hour of at is inside the configured
// quiet window (start inclusive, end exclusive, wrapping midnight).
func (d *Detector) inQuietHours(at time.Time) bool {
	s, e := d.cfg.QuietStartHour, d.cfg.QuietEndHour
	if s == e {
		return false
	}
	h := at.UTC().Hour()
	if s < e {
		return h >= s && h < e
	}
	return h >= s || h < e
}

func distinctTargets(ev []stamped) []string {
	set := map[string]bool{}
	for _, s := range ev {
		set[s.target] = true
	}
	out := make([]string, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// raise stores an alert and records it in the delivery log.
func (d *Detector) raise(a Alert, at time.Time) {
	d.mu.Lock()
	d.alertSeq++
	a.ID = fmt.Sprintf("ALERT-%06d", d.alertSeq)
	a.At = at.Format(time.RFC3339Nano)
	d.alerts = append(d.alerts, a)
	d.mu.Unlock()

	d.record(Entry{
		At:      a.At,
		Kind:    KindAlert,
		Sender:  a.Agent,
		Signal:  a.Signal,
		Verdict: a.Severity,
		Detail:  a.Detail,
	})
}

// record appends to the log when one is configured, and is a no-op otherwise.
func (d *Detector) record(e Entry) {
	if d.audit == nil {
		return
	}
	if _, err := d.audit.Append(e); err != nil {
		d.mu.Lock()
		d.lastErr = err
		d.mu.Unlock()
	}
}

// Alerts returns a copy of every alert raised so far, oldest first.
func (d *Detector) Alerts() []Alert {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Alert, len(d.alerts))
	copy(out, d.alerts)
	return out
}

// Quarantined reports whether the agent has been contained. It implements
// registry.Detector: the delivery path asks this before anything else.
func (d *Detector) Quarantined(agentID string) bool {
	if d == nil || agentID == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.quarantined[agentID]
	return ok
}

// Quarantines returns a copy of the contained agents, ordered by agent id.
func (d *Detector) Quarantines() []Quarantine {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Quarantine, 0, len(d.quarantined))
	for _, q := range d.quarantined {
		out = append(out, q)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AgentID < out[j].AgentID })
	return out
}

// Stats is the detector's own tally, for the read API.
type Stats struct {
	Observed          int            `json:"observed"`
	Verdicts          map[string]int `json:"verdicts"`
	Alerts            int            `json:"alerts"`
	QuarantinedAgents int            `json:"quarantined_agents"`
	Canaries          int            `json:"canaries"`
	LogPath           string         `json:"log_path,omitempty"`
	LogEntries        int            `json:"log_entries"`
	LastError         string         `json:"last_error,omitempty"`
}

// Stats returns a snapshot of the detector's counters.
func (d *Detector) Stats() Stats {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := Stats{
		Observed:          d.observed,
		Verdicts:          map[string]int{},
		Alerts:            len(d.alerts),
		QuarantinedAgents: len(d.quarantined),
		Canaries:          len(d.canaries),
	}
	for k, v := range d.verdicts {
		s.Verdicts[k] = v
	}
	if d.audit != nil {
		s.LogPath = d.audit.Path()
		s.LogEntries = d.audit.Total()
	}
	if d.lastErr != nil {
		s.LastError = d.lastErr.Error()
	}
	return s
}

// Close closes the delivery log.
func (d *Detector) Close() error {
	if d == nil || d.audit == nil {
		return nil
	}
	return d.audit.Close()
}
