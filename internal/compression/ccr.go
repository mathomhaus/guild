// Package compression is the ADR-006 Phase 7 opt-in capability module.
//
// It ports Headroom's pure-algorithmic context compressors (no ML, no
// provider proxy) to Go and exposes them behind a config toggle that is
// OFF by default. With the default config the module contributes nothing:
// no CLI verb, no MCP tool, no INSTRUCTIONS fragment, and the lore_dossier
// path stays byte-identical. The capability only engages when the operator
// sets [modules].compression = true.
//
// Two operating modes mirror Headroom:
//
//   - Lossless compaction (JSON/SmartCrusher table render): the original is
//     fully recoverable from the compact form alone; no retrieval needed.
//   - Lossy-with-CCR (diff, log, search): a compact view ships inline and
//     the full original is stashed in the CCR store keyed by a hash; a
//     retrieval marker embedded in the output lets a later retrieve(hash)
//     call expand it back to the original. "Lossy on the wire, lossless
//     end to end."
//
// This file is the CCR (Compress-Cache-Retrieve) reversible store: a keyed
// store with a TTL holding originals, plus the marker format the compressors
// embed and the retrieve verb expands. It mirrors Headroom's
// crates/headroom-core/src/ccr semantics (keyed put/get, lazy TTL purge,
// fixed marker format) using only the Go standard library.
package compression

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sync"
	"time"
)

const (
	// DefaultCapacity bounds the in-memory store; matches Headroom's
	// CompressionStore default. When a put would exceed capacity the
	// oldest entry (by insertion order) is evicted.
	DefaultCapacity = 1000

	// DefaultTTL is the entry lifetime; matches Headroom's 5-minute
	// default. Entries past their TTL are dropped lazily on the next get.
	DefaultTTL = 5 * time.Minute

	// KeyHexLen is the number of hex chars in a CCR key. Headroom's Rust
	// path uses a 24-char BLAKE3 prefix; the diff/log/search compressors
	// use a 24-char MD5 prefix. We standardize on a 24-char SHA-256 prefix
	// (pure stdlib, collision-resistant for a bounded LRU population) and
	// keep the marker grammar identical so a marker is recognized the same
	// way regardless of which compressor minted it.
	KeyHexLen = 24
)

// ComputeKey returns the canonical CCR key for payload: the first KeyHexLen
// hex chars of its SHA-256 digest. Deterministic, so re-storing identical
// content is idempotent (same key, same bytes).
func ComputeKey(payload string) string {
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])[:KeyHexLen]
}

// MarkerFor returns the standard <<ccr:HASH>> marker. This format is fixed
// across compressors and tests, matching Headroom's marker_for so a
// downstream consumer can pattern-match a marker no matter which compressor
// produced it.
func MarkerFor(hash string) string { return "<<ccr:" + hash + ">>" }

// Two marker grammars resolve to the same content-addressed store:
//
//   - The structured table marker <<ccr:HASH>> (and its richer
//     <<ccr:HASH,kind,size>> form) minted by the JSON/table compactor for
//     opaque cells, matching Headroom's marker_for / format_ccr_marker.
//   - The human-readable "Retrieve ... hash=HASH" footer minted by the
//     diff/log/search compressors, matching Headroom's Python footer.
//
// retrieve(hash) accepts a bare hash, a whole compressed block containing
// either marker, or the marker itself; ExtractMarkerHash finds the first
// hash present so a caller can paste any of them. The hash is a lowercase
// hex run of 8..64 chars (24 for both the SHA-256 and MD5-24 paths; the
// wider range tolerates a future prefix length).
var (
	ccrMarkerRE = regexp.MustCompile(`<<ccr:([a-f0-9]{8,64})\b`)
	hashEqRE    = regexp.MustCompile(`hash=([a-f0-9]{8,64})`)
)

// ExtractMarkerHash returns the first CCR marker hash found in text, or ""
// when none is present. Recognizes both the <<ccr:HASH...>> grammar and the
// trailing "hash=HASH" footer.
func ExtractMarkerHash(text string) string {
	if m := ccrMarkerRE.FindStringSubmatch(text); m != nil {
		return m[1]
	}
	if m := hashEqRE.FindStringSubmatch(text); m != nil {
		return m[1]
	}
	return ""
}

// Store is the reversible CCR backend: stash an original under its key, look
// it up later to expand a marker. Implementations are safe for concurrent
// use so one store can be shared across the CLI surface, the MCP surface,
// and the compressors.
type Store interface {
	// Put stashes payload under hash. Re-storing the same hash overwrites
	// in place (same hash means same content, so this is idempotent).
	Put(hash, payload string)
	// Get returns the payload for hash and whether it was present and live
	// (not expired).
	Get(hash string) (string, bool)
	// Len reports the number of live entries. Informational; used by tests
	// and status output.
	Len() int
}

// MemStore is the process-local CCR store: a map guarded by a mutex, with
// lazy TTL expiry on read and FIFO capacity eviction on write. It is the
// default store for the module (the daemon could later swap in a SQLite-
// backed one without changing the marker grammar). now is overridable so
// tests can drive TTL deterministically.
type MemStore struct {
	mu       sync.Mutex
	entries  map[string]memEntry
	order    []string // insertion order, for capacity eviction
	ttl      time.Duration
	capacity int
	now      func() time.Time
}

type memEntry struct {
	payload  string
	inserted time.Time
}

// NewMemStore returns a store with the default capacity and TTL.
func NewMemStore() *MemStore {
	return NewMemStoreWith(DefaultCapacity, DefaultTTL, time.Now)
}

// NewMemStoreWith returns a store with explicit capacity, TTL, and clock.
// A non-positive capacity falls back to DefaultCapacity; a non-positive TTL
// disables expiry (entries live until evicted by capacity). A nil clock uses
// time.Now.
func NewMemStoreWith(capacity int, ttl time.Duration, now func() time.Time) *MemStore {
	if capacity <= 0 {
		capacity = DefaultCapacity
	}
	if now == nil {
		now = time.Now
	}
	return &MemStore{
		entries:  make(map[string]memEntry, capacity),
		ttl:      ttl,
		capacity: capacity,
		now:      now,
	}
}

// Put stashes payload under hash, evicting the oldest entries if needed to
// stay under capacity. An empty hash is ignored (never minted by ComputeKey).
func (s *MemStore) Put(hash, payload string) {
	if hash == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[hash]; ok {
		// Idempotent overwrite: keep the original insertion position so
		// the FIFO order stays stable.
		s.entries[hash] = memEntry{payload: payload, inserted: s.now()}
		return
	}
	for len(s.entries) >= s.capacity && len(s.order) > 0 {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.entries, oldest)
	}
	s.entries[hash] = memEntry{payload: payload, inserted: s.now()}
	s.order = append(s.order, hash)
}

// Get returns the payload for hash if present and not past its TTL. An
// expired entry is dropped on access (lazy expiry) and reported absent.
func (s *MemStore) Get(hash string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[hash]
	if !ok {
		return "", false
	}
	if s.ttl > 0 && s.now().Sub(e.inserted) >= s.ttl {
		delete(s.entries, hash)
		return "", false
	}
	return e.payload, true
}

// Len returns the number of entries currently held (including any that are
// past their TTL but not yet lazily purged). Cheap; takes the lock briefly.
func (s *MemStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}
