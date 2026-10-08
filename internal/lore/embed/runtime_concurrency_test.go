package embed

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestExtract_ConcurrentSameProcess(t *testing.T) {
	m := fakeManifest(bytes.Repeat([]byte("runtime"), 1<<17), bytes.Repeat([]byte("model"), 1<<19), bytes.Repeat([]byte("vocab"), 1<<12))
	for _, drift := range []bool{false, true} {
		name := "cold"
		if drift {
			name = "drift"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if drift {
				if _, err := Extract(m, dir); err != nil {
					t.Fatal(err)
				}
				for _, asset := range m.Assets {
					if err := os.WriteFile(filepath.Join(dir, asset.Name), []byte("corrupt"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			const workers = 24
			start := make(chan struct{})
			errors := make(chan error, workers)
			var wg sync.WaitGroup
			for i := 0; i < workers; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					_, err := Extract(m, dir)
					errors <- err
				}()
			}
			close(start)
			wg.Wait()
			close(errors)
			for err := range errors {
				if err != nil {
					t.Errorf("concurrent extraction failed: %v", err)
				}
			}
			for _, asset := range m.Assets {
				sha, err := fileSHA256Hex(filepath.Join(dir, asset.Name))
				if err != nil || sha != asset.SHA256 {
					t.Fatalf("asset %s SHA=%s err=%v", asset.Name, sha, err)
				}
			}
			leftovers, err := filepath.Glob(filepath.Join(dir, "*.tmp-*"))
			if err != nil || len(leftovers) != 0 {
				t.Fatalf("temporary files leaked: %v err=%v", leftovers, err)
			}
			warm, err := Extract(m, dir)
			if err != nil || warm.Extracted {
				t.Fatalf("warm cache was rewritten: result=%+v err=%v", warm, err)
			}
		})
	}
}
