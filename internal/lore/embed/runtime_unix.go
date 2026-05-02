// Runtime loader: unix-only because shota3506/onnxruntime-purego's
// session loader calls purego.Dlopen + RTLD_NOW, which are not available
// on the purego Windows surface (F11). On windows the package compiles
// with only embedder.go + null.go + wordpiece.go + deterministic.go +
// runtime.go, and callers use NullEmbedder to deterministically fall
// through to BM25-only retrieval.

//go:build unix

package embed

import (
	"fmt"

	ort "github.com/shota3506/onnxruntime-purego/onnxruntime"
)

// ortRuntime is a thin wrapper over shota3506/onnxruntime-purego's
// Runtime + Env + Session triple. Lives inside the port so BGEEmbedder
// can depend on one construct rather than three, and so the Phase 2
// swap to mathomhaus/ortpipe only has to replace this file.
type ortRuntime struct {
	rt      *ort.Runtime
	env     *ort.Env
	session *ort.Session
}

// openORTRuntime loads libonnxruntime at cfg.LibraryPath, opens an ORT
// env + inference session against cfg.ModelPath, and returns the triple.
// Every failure path wraps the underlying error with context so callers
// can tell which stage failed without parsing strings.
//
// F1: "pure Go" is true at build time only; cfg.LibraryPath must point
// at an on-disk libonnxruntime.{dylib,so}. F3: leaving LibraryPath empty
// is unsupported by this wrapper even though the underlying library
// will try DYLD/LD resolution; callers must pass an absolute path.
func openORTRuntime(cfg RuntimeConfig) (*ortRuntime, error) {
	if cfg.LibraryPath == "" {
		return nil, fmt.Errorf("%w: RuntimeConfig.LibraryPath is empty", ErrEmbedderDisabled)
	}
	if cfg.ModelPath == "" {
		return nil, fmt.Errorf("%w: RuntimeConfig.ModelPath is empty", ErrEmbedderDisabled)
	}

	rt, err := ort.NewRuntime(cfg.LibraryPath, ORTAPIVersion)
	if err != nil {
		return nil, fmt.Errorf("embed: ort NewRuntime(%q, apiV=%d): %w", cfg.LibraryPath, ORTAPIVersion, err)
	}
	env, err := rt.NewEnv("guild-embed", ort.LoggingLevelWarning)
	if err != nil {
		_ = rt.Close()
		return nil, fmt.Errorf("embed: ort NewEnv: %w", err)
	}
	opts := &ort.SessionOptions{IntraOpNumThreads: cfg.NumThreads}
	sess, err := rt.NewSession(env, cfg.ModelPath, opts)
	if err != nil {
		env.Close()
		_ = rt.Close()
		return nil, fmt.Errorf("embed: ort NewSession(%q): %w", cfg.ModelPath, err)
	}
	return &ortRuntime{rt: rt, env: env, session: sess}, nil
}

// close releases the native resources in reverse construction order.
// Safe to call multiple times; subsequent calls no-op. F6: purego has
// no Dlclose so the shared-library handle leaks for the lifetime of the
// process. Fine for CLI; fine for MCP (one runtime per process).
func (r *ortRuntime) close() {
	if r == nil {
		return
	}
	if r.session != nil {
		r.session.Close()
		r.session = nil
	}
	if r.env != nil {
		r.env.Close()
		r.env = nil
	}
	if r.rt != nil {
		_ = r.rt.Close()
		r.rt = nil
	}
}
