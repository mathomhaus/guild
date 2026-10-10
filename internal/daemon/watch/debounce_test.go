package watch

import (
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Drive the real watch loop with raw events and virtual time. OS event
// delivery cannot promise that repeated writes arrive in one quiet window,
// especially while the race suite competes for a CI runner's CPU.
func TestDebounceCoalescesRepeatedWrites(t *testing.T) {
	root := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		const debounce = 120 * time.Millisecond
		w := &Watcher{
			roots:         []Root{{Project: "proj", Path: root}},
			debounce:      debounce,
			fs:            &fsnotify.Watcher{Events: make(chan fsnotify.Event), Errors: make(chan error)},
			out:           make(chan Event, eventBuffer),
			pendingEvents: make(map[Event]*pending),
			done:          make(chan struct{}),
			loopDone:      make(chan struct{}),
		}
		go w.run()
		defer func() {
			close(w.done)
			<-w.loopDone
		}()

		target := filepath.Join(root, "busy.txt")
		raw := fsnotify.Event{Name: target, Op: fsnotify.Write}
		want := Event{Project: "proj", Path: target, Kind: KindFile}
		assertQuiet := func() {
			t.Helper()
			select {
			case ev := <-w.Events():
				t.Fatalf("unexpected event before quiet window elapsed: %+v", ev)
			default:
			}
		}
		assertEvent := func() {
			t.Helper()
			select {
			case ev := <-w.Events():
				if ev != want {
					t.Fatalf("event = %+v, want %+v", ev, want)
				}
			default:
				t.Fatal("quiet window elapsed without a file event")
			}
		}

		for i := 0; i < 5; i++ {
			if i > 0 {
				time.Sleep(debounce / 4)
			}
			w.fs.Events <- raw
			synctest.Wait() // raw event folded into the pending set
			assertQuiet()
		}
		time.Sleep(debounce - time.Nanosecond)
		synctest.Wait()
		assertQuiet()
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		assertEvent()
		assertQuiet() // exactly one event for the entire burst

		// A later burst must still emit; coalescing must not suppress
		// subsequent changes to the same path.
		w.fs.Events <- raw
		synctest.Wait()
		time.Sleep(debounce)
		synctest.Wait()
		assertEvent()
		assertQuiet()
	})
}
