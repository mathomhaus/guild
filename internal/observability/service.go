package observability

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/mathomhaus/guild/internal/daemon"
	"github.com/mathomhaus/guild/internal/module"
)

// This file is the observability module's daemon Service (ADR-006 Phase 5).
// It is the runtime that ties the triad together: it installs a
// daemon.DecisionRecorder so the daemon's autopass / lease-reap /
// staleness-renewal gates feed the metrics registry, the JSONL event log,
// and the time-bucketed rollups; and it serves the Prometheus /metrics HTTP
// endpoint. It runs ONLY when the observability module is enabled AND the
// daemon is up: a disabled module's Services() are never collected, so this
// Service is never constructed, the recorder is never installed (the
// daemon's sink stays nil), no HTTP port is opened, and daemon behavior is
// byte-identical to a build without this module.
//
// The Service satisfies module.Service (Name/Start/Stop) structurally; it is
// returned from the module's Services() in module.go. The daemon's uniform
// service registry (internal/daemon/service.go) starts it on boot and stops
// it on shutdown alongside the built-in loops.

// flushInterval is how often the Service persists the rollups sidecar while
// running, so a crash loses at most this much un-persisted rollup state (the
// next boot replays the log to recover it anyway; the sidecar is the
// fast-path).
const flushInterval = 30 * time.Second

// metricsReadTimeout bounds a scrape client; a slow/stuck scraper must not
// pin a daemon goroutine.
const (
	metricsReadTimeout  = 5 * time.Second
	metricsWriteTimeout = 10 * time.Second
)

// Service is the observability daemon loop + metrics endpoint. Construct with
// NewService; the daemon drives Start/Stop.
type Service struct {
	settings Settings
	log      *slog.Logger

	registry   *Registry
	eventLog   *EventLog
	rollups    *Rollups
	rollupPath string

	mu        sync.Mutex
	srv       *http.Server
	boundAddr string
	flushCtx  context.CancelFunc
	flushWG   sync.WaitGroup
	started   bool
}

// compile-time assertions: *Service satisfies the daemon's recorder seam and
// the module.Service shape (Name/Start/Stop) the daemon's registry consumes.
var (
	_ daemon.DecisionRecorder = (*Service)(nil)
	_ module.Service          = (*Service)(nil)
)

// NewService builds the observability Service from resolved settings. logPath
// and rollupPath default to the canonical guild-home locations when empty;
// passing explicit paths is for tests. A nil logger falls back to the
// default. The rollups are reconstructed (sidecar + log replay) so a restart
// resumes its buckets.
func NewService(s Settings, logPath, rollupPath string, logger *slog.Logger) (*Service, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if logPath == "" {
		p, err := DefaultEventLogPath()
		if err != nil {
			return nil, err
		}
		logPath = p
	}
	if rollupPath == "" {
		p, err := DefaultRollupPath()
		if err != nil {
			return nil, err
		}
		rollupPath = p
	}
	rollups, err := LoadRollups(rollupPath, logPath)
	if err != nil {
		// A corrupt sidecar should not stop the daemon; rebuild from log only.
		logger.Warn("observability: could not load rollups; starting fresh", "err", err.Error())
		rollups = NewRollups()
	}
	svc := &Service{
		settings:   s,
		log:        logger,
		registry:   NewRegistry(),
		eventLog:   NewEventLog(logPath),
		rollups:    rollups,
		rollupPath: rollupPath,
	}
	return svc, nil
}

// Name identifies the loop in daemon logs and status.
func (s *Service) Name() string { return "observability" }

// Registry exposes the metric registry for tests and any in-process reader.
func (s *Service) Registry() *Registry { return s.registry }

// Rollups exposes the rollups for tests and in-process readers.
func (s *Service) Rollups() *Rollups { return s.rollups }

// metricsAddr returns the actual bound metrics address (resolving an
// ephemeral :0 port), or "" when the endpoint is not listening. Used by tests
// to scrape the endpoint.
func (s *Service) metricsAddr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.boundAddr
}

// Start installs the decision recorder and, when a metrics address is
// configured, starts the /metrics HTTP server. It returns promptly: the HTTP
// server and the rollup-flush loop run on their own goroutines and honor the
// ctx the daemon cancels on shutdown. Installing the recorder is the moment
// the daemon's gates begin feeding observability; before Start (and after
// Stop) the daemon's sink is nil and behavior is byte-identical.
func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return nil
	}

	// Install the recorder first so the gates that fire during startup are
	// captured. The daemon sink is process-global; SetDecisionRecorder(nil)
	// in Stop removes us.
	daemon.SetDecisionRecorder(s)
	s.registry.Set(MetricRecorderEnabled, 1)

	// Record a lifecycle event so the log shows when observability came up.
	// Fold it into the rollups too (with the same UTC stamp Append uses) so
	// the live rollup matches the one a restart rebuilds from the log.
	startEv := Event{Time: time.Now().UTC(), Kind: "service_start", Reason: "observability service started"}
	s.rollups.Fold(startEv)
	if s.settings.EventLog {
		if err := s.eventLog.Append(startEv); err != nil {
			s.log.Warn("observability: could not append start event", "err", err.Error())
		} else {
			s.registry.Inc(MetricEventsLogged)
		}
	}

	// Metrics HTTP endpoint (optional: empty addr disables it).
	if s.settings.MetricsAddr != "" {
		ln, err := net.Listen("tcp", s.settings.MetricsAddr)
		if err != nil {
			// A bind failure must not crash the daemon: log and degrade to
			// "recording without an HTTP endpoint", still serving.
			s.log.Warn("observability: could not bind metrics address; metrics endpoint disabled",
				"addr", s.settings.MetricsAddr, "err", err.Error())
		} else {
			mux := http.NewServeMux()
			mux.HandleFunc("/metrics", s.handleMetrics)
			s.srv = &http.Server{
				Handler:      mux,
				ReadTimeout:  metricsReadTimeout,
				WriteTimeout: metricsWriteTimeout,
			}
			s.boundAddr = ln.Addr().String()
			go func() {
				if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
					s.log.Warn("observability: metrics server stopped", "err", err.Error())
				}
			}()
			s.log.Info("observability: metrics endpoint listening", "addr", s.boundAddr)
		}
	}

	// Rollup flush loop: persist the sidecar periodically and on ctx cancel.
	flushCtx, cancel := context.WithCancel(ctx)
	s.flushCtx = cancel
	s.flushWG.Add(1)
	go s.flushLoop(flushCtx)

	s.started = true
	return nil
}

// flushLoop persists the rollups every flushInterval and once more on exit.
func (s *Service) flushLoop(ctx context.Context) {
	defer s.flushWG.Done()
	tk := time.NewTicker(flushInterval)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			s.persistRollups()
			return
		case <-tk.C:
			s.persistRollups()
		}
	}
}

// persistRollups writes the sidecar, logging a failure without crashing.
func (s *Service) persistRollups() {
	if err := s.rollups.Persist(s.rollupPath); err != nil {
		s.log.Warn("observability: could not persist rollups", "err", err.Error())
	}
}

// Stop removes the decision recorder, shuts the HTTP server, and joins the
// flush loop (which persists a final time). After Stop the daemon's sink is
// nil again, so any further daemon decision is unrecorded and byte-identical
// to the no-module path. Bounded by the ctx the daemon passes (a drain
// deadline).
func (s *Service) Stop(ctx context.Context) error {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return nil
	}
	srv := s.srv
	cancel := s.flushCtx
	s.started = false
	s.mu.Unlock()

	// Remove ourselves from the daemon sink first so no further decision is
	// recorded while we tear down.
	daemon.SetDecisionRecorder(nil)
	s.registry.Set(MetricRecorderEnabled, 0)

	if cancel != nil {
		cancel() // triggers the final flush in flushLoop
	}
	s.flushWG.Wait()

	if srv != nil {
		if err := srv.Shutdown(ctx); err != nil {
			s.log.Warn("observability: metrics server shutdown", "err", err.Error())
		}
	}
	return nil
}

// handleMetrics serves the Prometheus text exposition. It is read-only and
// allocation-light: one Render call, one Write.
func (s *Service) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = fmt.Fprint(w, s.registry.Render())
}

// Record implements daemon.DecisionRecorder. It is called by the daemon's
// gates AFTER each decision's outcome is computed, so it can never change an
// outcome. It folds the decision into the metrics, the event log, and the
// rollups. Errors are logged and swallowed: observability must never break
// the daemon decision path it observes.
func (s *Service) Record(d daemon.Decision) {
	s.registry.Inc(MetricDecisionsTotal)
	s.recordMetrics(d)

	allow := d.Allow
	ev := Event{
		Time:    d.At,
		Kind:    string(d.Kind),
		Allow:   &allow,
		Reason:  d.Reason,
		Inputs:  d.Inputs,
		Metrics: d.Metrics,
	}
	s.rollups.Fold(ev)

	if s.settings.EventLog {
		if err := s.eventLog.Append(ev); err != nil {
			s.log.Warn("observability: could not append decision event", "kind", string(d.Kind), "err", err.Error())
			return
		}
		s.registry.Inc(MetricEventsLogged)
	}
}

// recordMetrics maps a decision onto the daemon counter/gauge set. It reads
// only d (already computed) so it cannot affect the daemon.
func (s *Service) recordMetrics(d daemon.Decision) {
	switch d.Kind {
	case daemon.DecisionAutopass:
		if d.Allow {
			s.registry.Inc(MetricSleepPasses)
		}
	case daemon.DecisionLeaseReap:
		// A sweep that examined leases counts as a sweep; forfeits add to the
		// forfeit counter.
		if errored := d.Inputs["errored"]; !errored {
			s.registry.Inc(MetricReaperSweeps)
		}
		if n := d.Metrics["forfeited"]; n > 0 {
			s.registry.Add(MetricLeaseForfeits, int64(n))
		}
	case daemon.DecisionStaleRenew:
		if n := d.Metrics["signals"]; n > 0 {
			s.registry.Add(MetricStaleSignals, int64(n))
		}
		if n := d.Metrics["quests"]; n > 0 {
			s.registry.Add(MetricRenewalQuests, int64(n))
		}
	}
}
