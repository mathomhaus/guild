package observability

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// This file is the metrics half of the observability triad (ADR-006 Phase 5,
// Headroom's prometheus.rs pattern ported to pure Go). It is a tiny, stdlib-
// only counter/gauge registry that renders the Prometheus text exposition
// format by hand. We deliberately do NOT pull in prometheus/client_golang:
// the text format is a simple, stable, well-documented line protocol and the
// daemon's metric set is small and label-free, so a few dozen lines of
// formatting beats a new module dependency (per the build constraint).
//
// Exposition format (one metric):
//
//	# HELP guild_daemon_lease_forfeits_total Zombie claims auto-forfeited.
//	# TYPE guild_daemon_lease_forfeits_total counter
//	guild_daemon_lease_forfeits_total 7
//
// All metrics here are unlabeled scalars (counters monotonically increase,
// gauges can go up or down), which keeps the renderer trivial and the
// cardinality bounded. Concurrency: counters/gauges are int64 atomics, so
// Inc/Add/Set are lock-free; the registry map is built once at construction
// and only read afterwards, so Render needs no lock.

// metricType is "counter" or "gauge" for the # TYPE line.
type metricType string

const (
	typeCounter metricType = "counter"
	typeGauge   metricType = "gauge"
)

// metric is one named scalar series. value is an int64 atomic so emit-site
// updates are lock-free.
type metric struct {
	name  string
	help  string
	typ   metricType
	value atomic.Int64
}

// Inc adds one. Counters and gauges both support it.
func (m *metric) Inc() { m.value.Add(1) }

// Add adds n (may be negative for a gauge).
func (m *metric) Add(n int64) { m.value.Add(n) }

// Set replaces the value (gauges only; calling on a counter is allowed but
// unconventional, used by the registry's restored-rollup seeding).
func (m *metric) Set(n int64) { m.value.Store(n) }

// Value reads the current value.
func (m *metric) Value() int64 { return m.value.Load() }

// Registry is a fixed set of named daemon metrics. Construct with
// NewRegistry; the set is closed (no dynamic registration) so Render is a
// stable, sorted walk and there is no register-time locking on the hot path.
type Registry struct {
	mu     sync.Mutex
	byName map[string]*metric
	order  []string
}

// NewRegistry returns a Registry preloaded with the daemon's metric set.
// Every counter starts at zero, so an empty scrape is well-formed.
func NewRegistry() *Registry {
	r := &Registry{byName: map[string]*metric{}}
	for _, d := range daemonMetrics {
		r.register(d.name, d.help, d.typ)
	}
	return r
}

// metricDef is a compile-time metric declaration.
type metricDef struct {
	name string
	help string
	typ  metricType
}

// Metric name constants. Prefixed guild_daemon_ per Prometheus naming
// convention (namespace_subsystem_unit). Counters carry the _total suffix.
const (
	MetricLeaseGrants    = "guild_daemon_lease_grants_total"
	MetricLeaseForfeits  = "guild_daemon_lease_forfeits_total"
	MetricReaperSweeps   = "guild_daemon_reaper_sweeps_total"
	MetricSessionsOpened = "guild_daemon_sessions_opened_total"
	//nolint:gosec // G101 false positive: a metric name, not a credential.
	MetricSleepPasses     = "guild_daemon_sleep_passes_total"
	MetricStaleSignals    = "guild_daemon_stale_signals_total"
	MetricRenewalQuests   = "guild_daemon_renewal_quests_total"
	MetricDecisionsTotal  = "guild_daemon_decisions_total"
	MetricEventsLogged    = "guild_daemon_events_logged_total"
	MetricSessionsActive  = "guild_daemon_sessions_active"
	MetricRecorderEnabled = "guild_daemon_decision_recorder_enabled"
)

// daemonMetrics is the closed set the daemon Service tracks. Counters first,
// then gauges; Render sorts by name regardless, so this order is only for
// reading the source.
var daemonMetrics = []metricDef{
	{MetricLeaseGrants, "Total leases granted (sessions taking a heartbeated claim).", typeCounter},
	{MetricLeaseForfeits, "Total zombie claims auto-forfeited by the lease reaper.", typeCounter},
	{MetricReaperSweeps, "Total lease-reaper sweeps that examined at least one expired lease.", typeCounter},
	{MetricSessionsOpened, "Total MCP sessions opened on the daemon.", typeCounter},
	{MetricSleepPasses, "Total idle dream passes the autopass gate fired.", typeCounter},
	{MetricStaleSignals, "Total lore staleness signals written by the watch pipeline.", typeCounter},
	{MetricRenewalQuests, "Total renewal quests posted by the watch pipeline.", typeCounter},
	{MetricDecisionsTotal, "Total daemon decisions recorded across all gates.", typeCounter},
	{MetricEventsLogged, "Total events appended to the durable JSONL event log.", typeCounter},
	{MetricSessionsActive, "Currently active MCP sessions on the daemon.", typeGauge},
	{MetricRecorderEnabled, "1 while the observability decision recorder is installed, else 0.", typeGauge},
}

// register adds a metric. Construction-time only; panics on a duplicate name
// (a programmer error in daemonMetrics).
func (r *Registry) register(name, help string, typ metricType) *metric {
	if _, dup := r.byName[name]; dup {
		panic("observability: duplicate metric " + name)
	}
	m := &metric{name: name, help: help, typ: typ}
	r.byName[name] = m
	r.order = append(r.order, name)
	return m
}

// Metric returns the named metric, or nil when unknown. Callers in this
// package use the name constants, so a nil return is a bug; external callers
// should nil-check.
func (r *Registry) Metric(name string) *metric {
	return r.byName[name]
}

// Inc increments the named counter/gauge by one. Unknown name is a no-op so
// a stray emit site can never panic the daemon.
func (r *Registry) Inc(name string) {
	if m := r.byName[name]; m != nil {
		m.Inc()
	}
}

// Add adds n to the named metric. Unknown name is a no-op.
func (r *Registry) Add(name string, n int64) {
	if m := r.byName[name]; m != nil {
		m.Add(n)
	}
}

// Set replaces the named gauge's value. Unknown name is a no-op.
func (r *Registry) Set(name string, n int64) {
	if m := r.byName[name]; m != nil {
		m.Set(n)
	}
}

// Render writes the full metric set in Prometheus text exposition format,
// metrics sorted by name for stable output. Each metric emits its HELP and
// TYPE comment lines followed by one sample line. The trailing newline after
// the last sample is required by the format.
func (r *Registry) Render() string {
	names := make([]string, len(r.order))
	copy(names, r.order)
	sort.Strings(names)

	var b strings.Builder
	for _, name := range names {
		m := r.byName[name]
		b.WriteString("# HELP ")
		b.WriteString(m.name)
		b.WriteByte(' ')
		b.WriteString(escapeHelp(m.help))
		b.WriteByte('\n')
		b.WriteString("# TYPE ")
		b.WriteString(m.name)
		b.WriteByte(' ')
		b.WriteString(string(m.typ))
		b.WriteByte('\n')
		b.WriteString(m.name)
		b.WriteByte(' ')
		b.WriteString(strconv.FormatInt(m.Value(), 10))
		b.WriteByte('\n')
	}
	return b.String()
}

// escapeHelp escapes a HELP string per the exposition format: backslash and
// newline are escaped; the help text is otherwise free-form. (Label values
// would also escape double-quote, but these metrics carry no labels.)
func escapeHelp(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}
