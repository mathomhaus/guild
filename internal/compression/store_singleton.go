package compression

import "sync"

// Process-wide CCR store singleton. The compressors stash originals into it
// and the retrieve verb reads from it; sharing one store per process means a
// marker minted by a compress call earlier in the session resolves on a later
// retrieve call. The store honors the configured CCR TTL.
//
// This is deliberately process-local (in-memory). A daemon-backed or SQLite-
// backed store that survives restarts is a clean future swap behind the same
// Store interface and marker grammar; this phase ships the in-memory default,
// matching Headroom's test-default backend.

var (
	storeOnce sync.Once
	procStore Store
)

// SharedStore returns the process-wide CCR store, constructing it on first use
// from the resolved [compression] TTL (falling back to DefaultTTL).
func SharedStore() Store {
	storeOnce.Do(func() {
		ttl := CurrentSettings().CCRTTL
		if ttl <= 0 {
			ttl = DefaultTTL
		}
		procStore = NewMemStoreWith(DefaultCapacity, ttl, nil)
	})
	return procStore
}
