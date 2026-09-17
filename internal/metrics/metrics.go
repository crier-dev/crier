// Package metrics implements the tiny Prometheus text-exposition registry
// behind the opt-in GET /metrics surface (DF-CRIER-142). It is deliberately
// dependency-free: counters are sync/atomic float64s, gauges are callbacks,
// and Handler renders the v0.0.4 text format — no prometheus/client_golang,
// no go.mod change.
//
// Series ownership follows the code that increments them: registry, relay,
// webhook and cmd/server each construct their own counters on Default at
// package init, while gauges whose objects live in the composition layer
// (federation hold depth, live WS subscribers) are registered from
// cmd/server. Registration is idempotent: re-creating a counter name returns
// the existing series and re-registering a gauge replaces it, so a process
// that boots the server more than once (tests do) never renders duplicates.
package metrics

import (
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// contentType is the Prometheus text exposition format (version 0.0.4)
// content type, sent verbatim on every scrape.
const contentType = "text/plain; version=0.0.4; charset=utf-8"

// Default is the process-wide registry crier's packages register their
// series into. There is exactly one exposition per process; /metrics serves
// Default.Handler().
var Default = NewRegistry()

// ---------- Counter ----------

// Counter is a monotonically increasing value. Adds are lock-free
// (atomic float64 bits, CAS loop). Negative and zero deltas are ignored:
// counters never go down.
type Counter struct {
	val atomic.Uint64 // math.Float64bits of the current value
}

// Add increments the counter by v (no-op for v <= 0).
func (c *Counter) Add(v float64) {
	if v <= 0 {
		return
	}
	for {
		old := c.val.Load()
		next := math.Float64frombits(old) + v
		if c.val.CompareAndSwap(old, math.Float64bits(next)) {
			return
		}
	}
}

// Inc increments the counter by one.
func (c *Counter) Inc() { c.Add(1) }

// value returns the current counter value.
func (c *Counter) value() float64 { return math.Float64frombits(c.val.Load()) }

// discarded is the sink for arity-mismatched With calls: a delivery path
// must never panic over a metric, so a wrong-arity child is dropped rather
// than fatal. All in-tree call sites pass literals and cannot hit this.
var discarded = &Counter{}

// CounterVec is a counter partitioned by label values (e.g.
// http_requests_total{code="200"}). Children are created on first use.
type CounterVec struct {
	name       string
	help       string
	labelNames []string

	mu       sync.Mutex
	children map[string]*Counter // joined label values -> child
	keys     map[string][]string // joined label values -> the values themselves
}

// With returns the child counter for the given label values, creating it on
// first use. labelValues must match the label names' arity, in order; a
// mismatched call returns a discarded counter (never panics on a hot path).
func (v *CounterVec) With(labelValues ...string) *Counter {
	if len(labelValues) != len(v.labelNames) {
		return discarded
	}
	key := strings.Join(labelValues, "\xff")
	v.mu.Lock()
	defer v.mu.Unlock()
	if c, ok := v.children[key]; ok {
		return c
	}
	c := &Counter{}
	if v.children == nil {
		v.children = make(map[string]*Counter)
		v.keys = make(map[string][]string)
	}
	v.children[key] = c
	v.keys[key] = append([]string(nil), labelValues...)
	return c
}

// ---------- gauge ----------

// gaugeFunc renders a value computed at scrape time.
type gaugeFunc struct {
	name string
	help string
	fn   func() float64
}

// ---------- registry ----------

// series is one exposable entity: metadata plus zero or more samples.
type series interface {
	meta() (name, help, typ string)
	writeSamples(b *strings.Builder)
}

type counterSeries struct {
	c    *Counter
	name string
	help string
}

func (s *counterSeries) meta() (string, string, string) { return s.name, s.help, "counter" }
func (s *counterSeries) writeSamples(b *strings.Builder) {
	b.WriteString(s.name)
	b.WriteByte(' ')
	writeFloat(b, s.c.value())
	b.WriteByte('\n')
}

type counterVecSeries struct {
	v *CounterVec
}

func (s *counterVecSeries) meta() (string, string, string) {
	return s.v.name, s.v.help, "counter"
}

func (s *counterVecSeries) writeSamples(b *strings.Builder) {
	s.v.mu.Lock()
	keys := make([]string, 0, len(s.v.children))
	for k := range s.v.children {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic exposition across scrapes
	labelValues := make(map[string][]string, len(keys))
	for _, k := range keys {
		labelValues[k] = s.v.keys[k]
	}
	s.v.mu.Unlock()

	for _, k := range keys {
		b.WriteString(s.v.name)
		writeLabels(b, s.v.labelNames, labelValues[k])
		b.WriteByte(' ')
		writeFloat(b, s.v.children[k].value())
		b.WriteByte('\n')
	}
}

type gaugeSeries struct {
	g gaugeFunc
}

func (s *gaugeSeries) meta() (string, string, string) { return s.g.name, s.g.help, "gauge" }
func (s *gaugeSeries) writeSamples(b *strings.Builder) {
	b.WriteString(s.g.name)
	b.WriteByte(' ')
	writeFloat(b, s.g.fn())
	b.WriteByte('\n')
}

// Registry holds an ordered set of series and renders them in the Prometheus
// text exposition format. Safe for concurrent use.
type Registry struct {
	mu     sync.RWMutex
	series []series
	byName map[string]series
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{byName: make(map[string]series)}
}

// NewCounter registers (or returns the already-registered) counter under
// name. Re-registering the same name from a second boot of the server in
// one process returns the existing series — values keep accumulating, which
// is the Prometheus contract anyway.
func (r *Registry) NewCounter(name, help string) *Counter {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.byName[name]; ok {
		if cs, ok := s.(*counterSeries); ok {
			return cs.c
		}
	}
	c := &Counter{}
	r.add(&counterSeries{c: c, name: name, help: help})
	return c
}

// NewCounterVec registers (or returns the already-registered) labelled
// counter under name. labelNames are rendered in order on every sample.
func (r *Registry) NewCounterVec(name, help string, labelNames ...string) *CounterVec {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.byName[name]; ok {
		if cs, ok := s.(*counterVecSeries); ok {
			return cs.v
		}
	}
	v := &CounterVec{name: name, help: help, labelNames: append([]string(nil), labelNames...)}
	r.add(&counterVecSeries{v: v})
	return v
}

// RegisterGaugeFunc registers a gauge whose value is fn() evaluated at
// scrape time. Registering an existing name replaces the gauge (the
// composition layer re-registers per boot; the latest fn wins).
func (r *Registry) RegisterGaugeFunc(name, help string, fn func() float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.add(&gaugeSeries{g: gaugeFunc{name: name, help: help, fn: fn}})
}

// add appends s, replacing any same-name series in place (locked caller).
func (r *Registry) add(s series) {
	name, _, _ := s.meta()
	if old, ok := r.byName[name]; ok {
		for i, cur := range r.series {
			if cur == old {
				r.series[i] = s
				break
			}
		}
	} else {
		r.series = append(r.series, s)
	}
	r.byName[name] = s
}

// render writes the whole exposition: every registered series' HELP/TYPE
// metadata (even with zero samples — the series list is the contract), then
// its samples in deterministic order.
func (r *Registry) render() string {
	r.mu.RLock()
	series := make([]series, len(r.series))
	copy(series, r.series)
	r.mu.RUnlock()

	var b strings.Builder
	for _, s := range series {
		name, help, typ := s.meta()
		b.WriteString("# HELP ")
		b.WriteString(name)
		b.WriteByte(' ')
		b.WriteString(escapeHelp(help))
		b.WriteByte('\n')
		b.WriteString("# TYPE ")
		b.WriteString(name)
		b.WriteByte(' ')
		b.WriteString(typ)
		b.WriteByte('\n')
		s.writeSamples(&b)
	}
	return b.String()
}

// Handler serves the registry on GET: always 200 with the v0.0.4 text
// exposition content type.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body := r.render()
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	})
}

// Handler serves the Default registry — what cmd/server mounts at /metrics.
func Handler() http.Handler { return Default.Handler() }

// ---------- text format helpers ----------

// writeLabels renders {k="v",…}; label values are escaped.
func writeLabels(b *strings.Builder, names, values []string) {
	b.WriteByte('{')
	for i := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(names[i])
		b.WriteString(`="`)
		b.WriteString(escapeLabelValue(values[i]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
}

// writeFloat renders a sample value: plain integers stay plain, everything
// else uses the shortest float representation.
func writeFloat(b *strings.Builder, v float64) {
	b.WriteString(strconv.FormatFloat(v, 'g', -1, 64))
}

// escapeLabelValue escapes a label value per the text format: backslash,
// double quote and line feed.
func escapeLabelValue(v string) string {
	r := strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		"\n", `\n`,
	)
	return r.Replace(v)
}

// escapeHelp escapes a HELP string: backslash and line feed only.
func escapeHelp(h string) string {
	r := strings.NewReplacer(
		`\`, `\\`,
		"\n", `\n`,
	)
	return r.Replace(h)
}
