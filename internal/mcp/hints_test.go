package mcp

import (
	"context"
	"testing"
)

func TestRegisterClosesPreviousHintsEngineDB(t *testing.T) {
	isolateHome(t)
	t.Cleanup(closeCurrentHintsEngine)

	if _, err := build(); err != nil {
		t.Fatalf("first build: %v", err)
	}
	first := currentHintsEngine
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
	if currentHintsEngine == nil || currentHintsEngine.Store == nil || currentHintsEngine.Store.DB == nil {
		t.Fatal("second build did not initialize replacement hints engine DB")
	}
	if currentHintsEngine == first {
		t.Fatal("second build reused the previous hints engine")
	}
	if currentHintsEngine.Store.DB == firstDB {
		t.Fatal("second build reused the previous hints DB handle")
	}
	if err := firstDB.PingContext(context.Background()); err == nil {
		t.Fatal("previous hints DB handle is still open after rebuild")
	}
}
