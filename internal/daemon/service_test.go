package daemon

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mathomhaus/guild/internal/module"
)

// TestLoopService_StartStop verifies the loopService adapter: Start spawns
// the loop and returns promptly, the loop runs until its Start ctx is
// cancelled, and Stop blocks until the goroutine has returned.
func TestLoopService_StartStop(t *testing.T) {
	var started, returned atomic.Bool
	svc := newLoopService("test", func(ctx context.Context) {
		started.Store(true)
		<-ctx.Done()
		returned.Store(true)
	})

	if svc.Name() != "test" {
		t.Fatalf("Name: got %q want test", svc.Name())
	}

	ctx, cancel := context.WithCancel(context.Background())
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Start returns promptly; give the goroutine a moment to mark started.
	deadline := time.Now().Add(time.Second)
	for !started.Load() {
		if time.Now().After(deadline) {
			t.Fatal("loop did not start within 1s")
		}
		time.Sleep(time.Millisecond)
	}
	if returned.Load() {
		t.Fatal("loop returned before its ctx was cancelled")
	}

	cancel()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopCancel()
	if err := svc.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !returned.Load() {
		t.Fatal("Stop returned before the loop goroutine finished")
	}
}

// TestLoopService_StopBeforeStart is a no-op (the daemon may Stop a
// service it never Started if Start failed).
func TestLoopService_StopBeforeStart(t *testing.T) {
	svc := newLoopService("noop", func(context.Context) {})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := svc.Stop(ctx); err != nil {
		t.Fatalf("Stop before Start should be a no-op, got %v", err)
	}
}

// stubService is a module.Service for asserting the daemon appends and
// drives module-contributed loops through collectServices.
type stubService struct {
	name          string
	startN, stopN atomic.Int32
}

func (s *stubService) Name() string { return s.name }

func (s *stubService) Start(context.Context) error {
	s.startN.Add(1)
	return nil
}

func (s *stubService) Stop(context.Context) error {
	s.stopN.Add(1)
	return nil
}

// TestCollectServices_ModuleServicesAppended asserts collectServices
// appends module-contributed services (cfg.Services) after the built-in
// loops. The built-in loops are nil here (constructing real ones needs a
// host), so only the module service is present, which pins that
// cfg.Services is consumed by the registry.
func TestCollectServices_ModuleServicesAppended(t *testing.T) {
	stub := &stubService{name: "stub-loop"}
	s := &Server{cfg: Config{Services: []module.Service{stub}}}

	svcs := s.collectServices()
	if len(svcs) != 1 {
		t.Fatalf("expected 1 service (the module stub), got %d", len(svcs))
	}
	if svcs[0].Name() != "stub-loop" {
		t.Errorf("module service not in list: got %q", svcs[0].Name())
	}

	// Drive start/stop through the Server helpers to pin the lifecycle.
	ctx := context.Background()
	for _, sv := range svcs {
		_ = sv.Start(ctx)
	}
	s.stopServices(svcs)
	if stub.startN.Load() != 1 || stub.stopN.Load() != 1 {
		t.Errorf("module service lifecycle: start=%d stop=%d want 1/1",
			stub.startN.Load(), stub.stopN.Load())
	}
}
