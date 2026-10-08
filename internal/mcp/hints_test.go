package mcp

import (
	"context"
	"testing"
)

func TestRegisterClosesPreviousHintsEngineDB(t *testing.T) {
	isolateHome(t)
	t.Cleanup(func() {
		if processProviders != nil {
			processProviders.closeHintsEngine()
		}
	})

	if _, err := build(); err != nil {
		t.Fatalf("first build: %v", err)
	}
	firstBundle := processProviders
	if firstBundle == nil {
		t.Fatal("first build did not record a process-default bundle")
	}
	first := firstBundle.hintsEngine
	if first == nil || first.Store == nil || first.Store.DB == nil {
		t.Fatal("first build did not initialize hints engine DB")
	}
	firstDB := first.Store.DB
	if err := firstDB.PingContext(context.Background()); err != nil {
		t.Fatalf("first hints DB should be open before rebuild: %v", err)
	}

	if _, err := build(); err != nil {
		t.Fatalf("second build: %v", err)
	}
	secondBundle := processProviders
	if secondBundle == nil || secondBundle == firstBundle {
		t.Fatal("second build did not construct a fresh process-default bundle")
	}
	second := secondBundle.hintsEngine
	if second == nil || second.Store == nil || second.Store.DB == nil {
		t.Fatal("second build did not initialize replacement hints engine DB")
	}
	if second == first {
		t.Fatal("second build reused the previous hints engine")
	}
	if second.Store.DB == firstDB {
		t.Fatal("second build reused the previous hints DB handle")
	}
	if err := firstDB.PingContext(context.Background()); err == nil {
		t.Fatal("previous hints DB handle is still open after rebuild")
	}
}
