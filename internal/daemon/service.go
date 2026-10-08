package daemon

import (
	"context"
	"sync"

	"github.com/mathomhaus/guild/internal/module"
)

// This file is the ADR-006 Phase 3 daemon service registry. It
// generalizes the daemon's per-loop background goroutines — the idle
// scheduler, the watch pipeline, the session/lease registry, the lease
// reaper, and any loop a capability module contributes via
// module.Service — into one uniform list the Run loop ranges over: Start
// each on boot, Stop each on shutdown. The previous fixed
// `if cfg.X != nil { go ... }` ladder collapses into a range over this
// list, and a future module's loop joins automatically because the kernel
// appends module.Enabled(...).Services() to it.

// loopService adapts one of the daemon's existing blocking Run(ctx) loops
// (Scheduler, Pipeline, Registry, Reaper) to the module.Service interface
// without changing the loop's behavior. Start spawns the loop on its own
// goroutine and returns promptly; the loop runs until the ctx it was
// Started with is cancelled (the daemon's connCtx). Stop blocks until that
// goroutine has returned, so the daemon's shutdown join is preserved
// exactly: cancel the shared ctx, then Stop every service.
//
// The adapter does NOT own cancellation: like the pre-Phase-3 ladder, the
// loops observe shutdown through the shared connCtx the daemon cancels on
// every Run exit path. Stop's ctx is a drain deadline only.
type loopService struct {
	name string
	run  func(ctx context.Context)

	mu   sync.Mutex
	done chan struct{}
}

// newLoopService wraps a named blocking Run(ctx) loop as a Service.
func newLoopService(name string, run func(ctx context.Context)) *loopService {
	return &loopService{name: name, run: run}
}

func (l *loopService) Name() string { return l.name }

// Start launches the loop on its own goroutine and returns immediately.
// The loop runs until ctx is cancelled. Idempotent guard: a second Start
// before Stop is a no-op (the daemon never double-starts a service, but
// the guard keeps the contract honest).
func (l *loopService) Start(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.done != nil {
		return nil
	}
	done := make(chan struct{})
	l.done = done
	go func() {
		defer close(done)
		l.run(ctx)
	}()
	return nil
}

// Stop blocks until the loop goroutine has returned or the drain ctx is
// cancelled. The loop is expected to be already winding down because the
// daemon cancelled the ctx it was Started with; Stop only joins.
func (l *loopService) Stop(ctx context.Context) error {
	l.mu.Lock()
	done := l.done
	l.mu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// collectServices assembles the ordered service list the Run loop drives.
// It adapts the daemon's four built-in loops (when wired) in their
// historical start order — scheduler, pipeline, registry, reaper — then
// appends any module-contributed services from cfg.Services. Keeping the
// built-in order byte-identical to the pre-Phase-3 ladder preserves
// daemon-startup parity; the module services run after them, which is the
// only place new behavior can appear and only when a module ships a loop.
func (s *Server) collectServices() []module.Service {
	var svcs []module.Service
	if s.cfg.Scheduler != nil {
		svcs = append(svcs, newLoopService("scheduler", s.cfg.Scheduler.Run))
	}
	if s.cfg.Pipeline != nil {
		svcs = append(svcs, newLoopService("pipeline", s.cfg.Pipeline.Run))
	}
	if s.cfg.Registry != nil {
		svcs = append(svcs, newLoopService("registry", s.cfg.Registry.Run))
	}
	if s.cfg.Reaper != nil {
		svcs = append(svcs, newLoopService("reaper", s.cfg.Reaper.Run))
	}
	svcs = append(svcs, s.cfg.Services...)
	return svcs
}
