// Package metrics implements the tiny Prometheus text-exposition registry
// behind the opt-in GET /metrics surface (DF-CRIER-142). It is deliberately
// dependency-free: counters and histograms are sync/atomic float64s, gauges
// are callbacks, and Handler renders the v0.0.4 text format — no
// prometheus/client_golang, no go.mod change.
//
// Histograms (MON-CRIER-002, observability-spec §3.3) carry a FIXED,
// cumulative bucket set decided at construction and shared by every sample of
// a vec: a bucket boundary is never allocated per request, so Observe is a
// scan over the (twelve-entry) bounds plus three atomic adds. A histogram
// renders the full Prometheus histogram shape — one "<name>_bucket" line per
// bucket with an "le" label, a "+Inf" bucket, plus "<name>_sum" and
// "<name>_count" — so promtool and every Prometheus server read it as a
// real histogram rather than a pile of unknown series.
//
// Series ownership follows the code that increments them: registry, relay,
// webhook, guard and cmd/server each construct their own counters on Default
// at package init, while gauges whose objects live in the composition layer
// (federation hold depth, live WS subscribers, build identity, store health,
// guard circuit/inflight) are registered from cmd/server. Registration is
// idempotent: re-creating a counter name returns the existing series and
// re-registering a gauge replaces it, so a process that boots the server more
// than once (tests do) never renders duplicates.
//
// Gauges come in three shapes: RegisterGaugeFunc for a single unlabelled
// sample, RegisterGaugeFuncVec for a sample whose label VALUES are fixed for
// the process lifetime (a build identity, the serving backend), and
// RegisterGaugeFuncLabel for a sample whose label values are computed per
// scrape from a bounded domain (guard provider circuits) — all evaluate their
// callback at scrape time.
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
	addFloat(&c.val, v)
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

// ---------- histogram ----------

// DefaultBuckets is the fixed latency bucket set of observability-spec §3.3:
// a hand-rolled exporter has no use for finer than 5ms or coarser than 30s.
// It is a plain slice so a caller can pass it straight to NewHistogram or
// NewHistogramVec; both COPY and normalise it, so a live series never shares
// storage with this (mutable) package variable.
var DefaultBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

// Histogram is a cumulative-bucket distribution of one measured quantity
// (seconds, for every series this repo emits). Buckets are fixed at
// construction and never reallocated: Observe is a scan over the shared
// bounds plus three atomic adds, safe for concurrent writers and for a
// concurrent scrape.
//
// Bucket counters are stored PER BUCKET (not cumulatively) and summed at
// render time, which is what Prometheus means by "cumulative buckets": the
// sample for le=0.1 counts every observation <= 0.1. An observation above the
// last bound lands in no bucket and is counted only by _count — exactly the
// "+Inf" bucket.
type Histogram struct {
	name   string
	help   string
	bounds []float64 // sorted, strictly increasing, fixed for the series' life

	counts []atomic.Uint64 // per-bucket (NOT cumulative) counts, index-parallel to bounds
	sum    atomic.Uint64   // Float64bits of the running sum
	count  atomic.Uint64   // every observation, including those above the last bound
}

// newHistogram builds a histogram over a normalised COPY of bounds.
func newHistogram(name, help string, bounds []float64) *Histogram {
	b := normalizeBuckets(bounds)
	return &Histogram{name: name, help: help, bounds: b, counts: make([]atomic.Uint64, len(b))}
}

// Observe records one observation of v (v is seconds for latency series).
// Negative values are clamped to 0 and NaN is dropped: a clock anomaly must
// never render a distribution that cannot exist, and NaN would poison _sum
// for the lifetime of the process.
func (h *Histogram) Observe(v float64) {
	if math.IsNaN(v) {
		return
	}
	if v < 0 {
		v = 0
	}
	for i, ub := range h.bounds {
		if v <= ub {
			h.counts[i].Add(1)
			break
		}
	}
	addFloat(&h.sum, v)
	h.count.Add(1)
}

// count returns the number of observations.
func (h *Histogram) countValue() uint64 { return h.count.Load() }

// sumValue returns the running sum of observations.
func (h *Histogram) sumValue() float64 { return math.Float64frombits(h.sum.Load()) }

// writeSamples renders the Prometheus histogram shape: one _bucket line per
// bound (cumulative), the +Inf bucket, then _sum and _count. name is the family
// name to render under (a histogram may be exposed under a namespaced alias,
// MON-CRIER-005); labelNames and labelValues are the vec's own labels, in
// registration order; "le" is always appended last, as the reference client
// does.
func (h *Histogram) writeSamples(b *strings.Builder, name string, labelNames, labelValues []string) {
	var cumulative uint64
	for i, ub := range h.bounds {
		cumulative += h.counts[i].Load()
		h.writeBucket(b, name, labelNames, labelValues, formatBucket(ub), cumulative)
	}
	// +Inf: every observation, including those above the last bound.
	h.writeBucket(b, name, labelNames, labelValues, "+Inf", h.count.Load())

	h.writeScalar(b, name, labelNames, labelValues, "_sum", strconv.FormatFloat(h.sumValue(), 'g', -1, 64))
	h.writeScalar(b, name, labelNames, labelValues, "_count", strconv.FormatUint(h.count.Load(), 10))
}

func (h *Histogram) writeBucket(b *strings.Builder, name string, labelNames, labelValues []string, le string, v uint64) {
	b.WriteString(name)
	b.WriteString("_bucket")
	writeLabels(b, appendLabel(labelNames, "le"), appendLabel(labelValues, le))
	b.WriteByte(' ')
	b.WriteString(strconv.FormatUint(v, 10))
	b.WriteByte('\n')
}

func (h *Histogram) writeScalar(b *strings.Builder, name string, labelNames, labelValues []string, suffix, value string) {
	b.WriteString(name)
	b.WriteString(suffix)
	if len(labelNames) > 0 {
		writeLabels(b, labelNames, labelValues)
	}
	b.WriteByte(' ')
	b.WriteString(value)
	b.WriteByte('\n')
}

// HistogramVec is a histogram partitioned by label values (e.g.
// crier_http_request_duration_seconds{route,method,code}). Children are
// created on first use and every child shares the ONE bucket set fixed when
// the vec was constructed — the reason a hot path never allocates buckets.
type HistogramVec struct {
	name       string
	help       string
	bounds     []float64
	labelNames []string

	mu       sync.RWMutex // read-mostly: With takes the read lock on the hot path
	children map[string]*Histogram
	keys     map[string][]string // joined label values -> the values themselves
}

// With returns the child histogram for the given label values, creating it on
// first use. labelValues must match the label names' arity, in order; a
// mismatched call returns a discarded histogram (never panics on a hot path,
// never leaks an unlabelled sample into the exposition).
func (v *HistogramVec) With(labelValues ...string) *Histogram {
	if len(labelValues) != len(v.labelNames) {
		return discardedHistogram
	}
	key := strings.Join(labelValues, "\xff")
	v.mu.RLock()
	h, ok := v.children[key]
	v.mu.RUnlock()
	if ok {
		return h
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if h, ok := v.children[key]; ok { // lost the race: reuse the winner
		return h
	}
	if v.children == nil {
		v.children = make(map[string]*Histogram)
		v.keys = make(map[string][]string)
	}
	h = newHistogram(v.name, v.help, v.bounds)
	v.children[key] = h
	v.keys[key] = append([]string(nil), labelValues...)
	return h
}

// Observe records one observation under the given label values: the one-call
// form of With(values...).Observe(x) used on the delivery hot paths.
func (v *HistogramVec) Observe(x float64, labelValues ...string) {
	v.With(labelValues...).Observe(x)
}

// snapshot copies the child map and label values so rendering never holds the
// lock while writing samples (a slow scrape must not block a delivery path).
func (v *HistogramVec) snapshot() (map[string]*Histogram, map[string][]string, []string) {
	v.mu.RLock()
	children := make(map[string]*Histogram, len(v.children))
	values := make(map[string][]string, len(v.keys))
	keys := make([]string, 0, len(v.children))
	for k, h := range v.children {
		children[k] = h
		values[k] = v.keys[k]
		keys = append(keys, k)
	}
	v.mu.RUnlock()
	sort.Strings(keys) // deterministic exposition across scrapes
	return children, values, keys
}

// discardedHistogram is the sink for arity-mismatched With calls: a hot path
// must never panic over a metric. It has no bounds, so it only ever records
// into its +Inf bucket — and it is never registered, so nothing is exposed.
var discardedHistogram = newHistogram("", "", nil)

// normalizeBuckets returns a sorted, strictly increasing COPY of bounds with
// non-positive, NaN, infinite and duplicate entries dropped: a bucket set that
// cannot render monotonic cumulative counts would produce an exposition
// Prometheus rejects.
func normalizeBuckets(bounds []float64) []float64 {
	sorted := make([]float64, 0, len(bounds))
	for _, b := range bounds {
		if math.IsNaN(b) || math.IsInf(b, 0) || b <= 0 {
			continue
		}
		sorted = append(sorted, b)
	}
	sort.Float64s(sorted)
	out := sorted[:0]
	for i, b := range sorted {
		if i > 0 && b == sorted[i-1] {
			continue // duplicate bound: one bucket, not two
		}
		out = append(out, b)
	}
	return out
}

// formatBucket renders a bucket bound the way the reference client does: the
// shortest float that round-trips (0.005, 0.01, … 30).
func formatBucket(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

// appendLabel returns names with one more value appended, WITHOUT mutating the
// caller's slice (a child's label slice is shared by every scrape).
func appendLabel(values []string, extra string) []string {
	out := make([]string, len(values)+1)
	copy(out, values)
	out[len(values)] = extra
	return out
}

// addFloat atomically adds v to the float64 whose bits live at addr. Used by
// Counter.Add and Histogram.Observe: integer atomics are not enough for a
// float running total.
func addFloat(addr *atomic.Uint64, v float64) {
	for {
		old := addr.Load()
		next := math.Float64frombits(old) + v
		if addr.CompareAndSwap(old, math.Float64bits(next)) {
			return
		}
	}
}

// ---------- gauge ----------

// gaugeFunc renders a value computed at scrape time.
type gaugeFunc struct {
	name string
	help string
	fn   func() float64
}

// gaugeVecFunc renders a value computed at scrape time under a fixed set of
// label values. The label VALUES are captured at registration, not passed by
// the callback: a gauge whose identity is fixed for the process lifetime
// (build identity, serving backend) has nothing per-scrape to vary, and
// keeping the render path single-sample keeps the exposition deterministic.
type gaugeVecFunc struct {
	name        string
	help        string
	labelNames  []string
	labelValues []string
	fn          func() float64
}

// gaugeFuncLabel renders a labelled gauge computed at scrape time: fn
// returns label value -> sample value. The label domain is the caller's
// contract and must be bounded (a per-request value would grow the
// exposition without bound); MON-CRIER-003's only user labels by provider
// preset.
type gaugeFuncLabel struct {
	name      string
	help      string
	labelName string
	fn        func() map[string]float64
}

// ---------- registry ----------

// series is one exposable entity: metadata plus zero or more samples.
//
// writeWith renders the samples under the given family name: the series' own
// name for the legacy family, or its namespaced alias (MON-CRIER-005). The
// samples themselves are identical either way — only the name in front of them
// changes, which is what makes the dual-emit window a rename and not a copy.
type series interface {
	meta() (name, help, typ string)
	writeWith(b *strings.Builder, name string)
}

type counterSeries struct {
	c    *Counter
	name string
	help string
}

func (s *counterSeries) meta() (string, string, string) { return s.name, s.help, "counter" }
func (s *counterSeries) writeWith(b *strings.Builder, name string) {
	b.WriteString(name)
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

func (s *counterVecSeries) writeWith(b *strings.Builder, name string) {
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
		b.WriteString(name)
		writeLabels(b, s.v.labelNames, labelValues[k])
		b.WriteByte(' ')
		writeFloat(b, s.v.children[k].value())
		b.WriteByte('\n')
	}
}

// ---------- histogram series ----------

type histogramSeries struct {
	h *Histogram
}

func (s *histogramSeries) meta() (string, string, string) { return s.h.name, s.h.help, "histogram" }
func (s *histogramSeries) writeWith(b *strings.Builder, name string) {
	s.h.writeSamples(b, name, nil, nil)
}

type histogramVecSeries struct {
	v *HistogramVec
}

func (s *histogramVecSeries) meta() (string, string, string) {
	return s.v.name, s.v.help, "histogram"
}

func (s *histogramVecSeries) writeWith(b *strings.Builder, name string) {
	children, values, keys := s.v.snapshot()
	for _, k := range keys {
		children[k].writeSamples(b, name, s.v.labelNames, values[k])
	}
}

type gaugeSeries struct {
	g gaugeFunc
}

func (s *gaugeSeries) meta() (string, string, string) { return s.g.name, s.g.help, "gauge" }
func (s *gaugeSeries) writeWith(b *strings.Builder, name string) {
	b.WriteString(name)
	b.WriteByte(' ')
	writeFloat(b, s.g.fn())
	b.WriteByte('\n')
}

type gaugeVecSeries struct {
	g gaugeVecFunc
}

func (s *gaugeVecSeries) meta() (string, string, string) { return s.g.name, s.g.help, "gauge" }
func (s *gaugeVecSeries) writeWith(b *strings.Builder, name string) {
	b.WriteString(name)
	writeLabels(b, s.g.labelNames, s.g.labelValues)
	b.WriteByte(' ')
	writeFloat(b, s.g.fn())
	b.WriteByte('\n')
}

type gaugeFuncLabelSeries struct {
	g gaugeFuncLabel
}

func (s *gaugeFuncLabelSeries) meta() (string, string, string) {
	return s.g.name, s.g.help, "gauge"
}

func (s *gaugeFuncLabelSeries) writeWith(b *strings.Builder, name string) {
	samples := s.g.fn()
	values := make([]string, 0, len(samples))
	for v := range samples {
		values = append(values, v)
	}
	sort.Strings(values) // deterministic exposition across scrapes
	for _, v := range values {
		b.WriteString(name)
		writeLabels(b, []string{s.g.labelName}, []string{v})
		b.WriteByte(' ')
		writeFloat(b, samples[v])
		b.WriteByte('\n')
	}
}

// Registry holds an ordered set of series and renders them in the Prometheus
// text exposition format. Safe for concurrent use.
//
// # Namespacing aliases
//
// MON-CRIER-005 namespaces the exposition: every series must identify its
// product (observability-spec §2.4 G2 / §3.1). Live dashboards already key on
// the legacy names, so the migration is a DATED, DUAL-EMIT window rather than a
// break: Registry.Alias(legacy, namespaced) registers a second family name over
// the SAME series. The alias renders the identical samples, help and type at
// render() time — it is a name, not a copy, so the two names can never drift
// apart, no counter is incremented twice, and no increment site changes.
//
// The alias is resolved at RENDER time (not captured at Alias time), so a
// gauge the composition layer re-registers on a second in-process boot keeps
// rendering through its alias: the alias follows the live series, not the one
// that happened to exist when Alias was called.
type Registry struct {
	mu     sync.RWMutex
	series []series
	byName map[string]series
	// aliases are second names for already-registered series, in registration
	// order: render() writes a family's aliases directly after the family
	// itself. See Registry.Alias.
	aliases []aliasName
}

// aliasName is one dual-emit alias: the registered (legacy) series it mirrors
// and the namespaced name it renders under.
type aliasName struct {
	legacy string
	name   string
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

// NewHistogram registers (or returns the already-registered) unlabelled
// histogram under name, over a normalised copy of buckets. Re-registering the
// same name returns the existing series (a second boot in one process keeps
// accumulating — the Prometheus contract) with ITS original buckets: the
// bucket set is fixed by the first construction.
func (r *Registry) NewHistogram(name, help string, buckets []float64) *Histogram {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.byName[name]; ok {
		if hs, ok := s.(*histogramSeries); ok {
			return hs.h
		}
	}
	h := newHistogram(name, help, buckets)
	r.add(&histogramSeries{h: h})
	return h
}

// NewHistogramVec registers (or returns the already-registered) labelled
// histogram under name. labelNames are rendered in order on every sample, with
// "le" appended last per bucket line; all children share one bucket set.
func (r *Registry) NewHistogramVec(name, help string, buckets []float64, labelNames ...string) *HistogramVec {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.byName[name]; ok {
		if hs, ok := s.(*histogramVecSeries); ok {
			return hs.v
		}
	}
	v := &HistogramVec{
		name:       name,
		help:       help,
		bounds:     normalizeBuckets(buckets),
		labelNames: append([]string(nil), labelNames...),
	}
	r.add(&histogramVecSeries{v: v})
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

// RegisterGaugeFuncVec registers a gauge whose value is fn() evaluated at
// scrape time, rendered under the given label names/values — one sample per
// registration, for a gauge whose label values are fixed for the process
// lifetime (build identity, serving backend). Like RegisterGaugeFunc,
// re-registering the same name replaces the gauge, so a second boot in one
// process rebinds rather than duplicating.
//
// A mismatched name/value arity (all in-tree call sites pass literals) is
// registered UNLABELLED rather than panicking: a metric must never take the
// process down, and an unlabelled gauge still answers its question.
func (r *Registry) RegisterGaugeFuncVec(name, help string, labelNames, labelValues []string, fn func() float64) {
	if len(labelNames) != len(labelValues) {
		labelNames, labelValues = nil, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.add(&gaugeVecSeries{g: gaugeVecFunc{
		name:        name,
		help:        help,
		labelNames:  append([]string(nil), labelNames...),
		labelValues: append([]string(nil), labelValues...),
		fn:          fn,
	}})
}

// RegisterGaugeFuncLabel registers a single-label gauge whose samples are
// fn() evaluated at scrape time: the returned map is label value -> sample
// value. Re-registering an existing name replaces the previous fn, exactly
// like RegisterGaugeFunc, so repeated boots in one process never duplicate
// the series. The help text must state what a zero means, because this
// primitive can only render values, never "no data".
func (r *Registry) RegisterGaugeFuncLabel(name, help, labelName string, fn func() map[string]float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.add(&gaugeFuncLabelSeries{g: gaugeFuncLabel{name: name, help: help, labelName: labelName, fn: fn}})
}

// Alias registers namespacedName as a second exposition name for the series
// already registered as legacyName: the alias renders the SAME samples, help and
// type under the new name (MON-CRIER-005 dual-emit window, observability-spec
// §3.1). It is a NAME, not a copy — nothing is counted twice and no increment
// site is touched.
//
// Three properties the alias must inherit from registration, because the
// exposition is served by a process that may boot the server more than once:
//
//   - Idempotent: aliasing the same pair twice adds nothing, so repeated boots
//     never render the family twice.
//   - A no-op when the names are equal: a series whose legacy name already
//     matches its namespaced name is emitted ONCE (the brief's rule — never
//     duplicate identical lines).
//   - Resolved at render time: the alias follows whatever series currently
//     holds legacyName, so a gauge the composition layer re-registers per boot
//     keeps aliasing the LIVE callback.
//
// Resolution is lazy, so Alias may be called before the series is registered.
// If namespacedName is itself registered as a real series later, that real
// series wins and the alias is dropped (add does that), so no family is ever
// rendered twice.
func (r *Registry) Alias(legacyName, namespacedName string) {
	if legacyName == "" || namespacedName == "" || legacyName == namespacedName {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byName[namespacedName]; ok {
		return // a real series already owns the name; the alias would duplicate it
	}
	for _, a := range r.aliases {
		if a.legacy == legacyName && a.name == namespacedName {
			return
		}
	}
	r.aliases = append(r.aliases, aliasName{legacy: legacyName, name: namespacedName})
}

// add appends s, replacing any same-name series in place (locked caller). A
// real series registered under a name an alias had claimed evicts that alias:
// the exposition never renders one family twice.
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
	kept := r.aliases[:0]
	for _, a := range r.aliases {
		if a.name != name {
			kept = append(kept, a)
		}
	}
	r.aliases = kept
}

// render writes the whole exposition: every registered series' HELP/TYPE
// metadata (even with zero samples — the series list is the contract), then
// its samples in deterministic order, then any dual-emit alias of that family
// under its namespaced name (MON-CRIER-005).
//
// A family is rendered ONCE into a buffer and the alias re-emits those exact
// bytes under the other name. That is not a micro-optimisation: a gauge's
// callback is evaluated per render, and several of this process's gauges have
// observable side effects (the inbox queue gauges perform a store read and
// count a failed read once per scrape). Re-evaluating them for the alias would
// read the store twice per scrape and double the error counter — the exposition
// would report on itself. One evaluation, two names.
func (r *Registry) render() string {
	r.mu.RLock()
	series := make([]series, len(r.series))
	copy(series, r.series)
	aliasNames := make(map[string][]string, len(r.aliases))
	for _, a := range r.aliases {
		aliasNames[a.legacy] = append(aliasNames[a.legacy], a.name)
	}
	r.mu.RUnlock()

	var b strings.Builder
	for _, s := range series {
		name, help, typ := s.meta()
		writeFamily(&b, name, help, typ)
		var samples strings.Builder
		s.writeWith(&samples, name)
		rendered := samples.String()
		b.WriteString(rendered)
		for _, alias := range aliasNames[name] {
			writeFamily(&b, alias, help, typ)
			b.WriteString(renameFamily(rendered, name, alias))
		}
	}
	return b.String()
}

// renameFamily re-emits an already-rendered sample block under a different
// family name: every line of a family's block begins with that family's name
// (a counter with " ", a vec with "{", a histogram with "_"), so the swap is a
// prefix replacement on each line. It is never a search-and-replace over label
// values, and it never re-evaluates the series.
func renameFamily(rendered, from, to string) string {
	if rendered == "" || from == to {
		return rendered
	}
	var b strings.Builder
	b.Grow(len(rendered) + len(to)*4)
	for len(rendered) > 0 {
		nl := strings.IndexByte(rendered, '\n')
		var line string
		if nl < 0 {
			line, rendered = rendered, ""
		} else {
			line, rendered = rendered[:nl+1], rendered[nl+1:]
		}
		if strings.HasPrefix(line, from) {
			b.WriteString(to)
			b.WriteString(line[len(from):])
			continue
		}
		b.WriteString(line)
	}
	return b.String()
}

// writeFamily writes one family's HELP/TYPE metadata lines under name.
func writeFamily(b *strings.Builder, name, help, typ string) {
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
