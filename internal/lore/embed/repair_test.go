package embed

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"testing"
)

type repairEmbedder struct {
	fn func(context.Context, string) ([]float32, error)
}

func (e repairEmbedder) Dimension() int { return Dim }
func (e repairEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	return e.fn(ctx, text)
}

func TestRepairCoverageAndBackfill(t *testing.T) {
	for _, scenario := range []string{"missing", "stale", "wrong-model", "wrong-dim", "wrong-blob", "fresh-with-pending-state"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			db, id := hotTestDB(t)
			text := mustSourceText(t, db, LoreCorpus{}, id)
			model, dim, blob, hash := canonModelID, Dim, make([]byte, Dim), ContentHash(text)
			switch scenario {
			case "stale":
				hash = "old"
			case "wrong-model":
				model = "old-model"
			case "wrong-dim":
				dim = 5
			case "wrong-blob":
				blob = []byte{1}
			}
			if scenario != "missing" {
				if _, err := db.Exec(`INSERT INTO lore_vectors VALUES(?,?,?,?,1,?)`, id, model, dim, blob, hash); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.Exec(`UPDATE meta SET value='999' WHERE key IN ('vector_coverage_num','vector_coverage_den')`); err != nil {
				t.Fatal(err)
			}
			measured, err := ReadCoverage(ctx, db, LoreCorpus{}, canonModelID, nil)
			if err != nil {
				t.Fatal(err)
			}
			wantFresh := int64(0)
			if scenario == "fresh-with-pending-state" {
				wantFresh = 1
			}
			if measured.Eligible != 1 || measured.Fresh != wantFresh {
				t.Fatalf("coverage=%+v", measured)
			}
			res, err := Backfill(ctx, BackfillOptions{DB: db, Embedder: NewDeterministicEmbedder(), ModelID: canonModelID})
			if err != nil {
				t.Fatal(err)
			}
			if res.Embedded != int(1-wantFresh) || res.Failed != 0 {
				t.Fatalf("repair=%+v", res)
			}
			after, err := ReadHealthReport(ctx, db, LoreCorpus{})
			if err != nil {
				t.Fatal(err)
			}
			if after.FreshCount != 1 || after.CoverageNum != 1 || after.CoverageDen != 1 || after.StaleCount != 0 || after.PendingCount != 0 || after.InvalidCount != 0 {
				t.Fatalf("health=%+v", after)
			}
			if readMetaInt(t, db, "vector_coverage_num") != 1 || readMetaInt(t, db, "vector_coverage_den") != 1 {
				t.Fatal("counter drift survived")
			}
			epoch := res.Epoch
			repeat, err := Backfill(ctx, BackfillOptions{DB: db, Embedder: NewDeterministicEmbedder(), ModelID: canonModelID})
			if err != nil {
				t.Fatal(err)
			}
			if repeat.Total != 0 || repeat.Embedded != 0 || repeat.Epoch != epoch {
				t.Fatalf("no-op changed epoch: %+v", repeat)
			}
		})
	}
}

func TestRepairPreservesExistingVectorOnFailure(t *testing.T) {
	ctx := context.Background()
	db, id := hotTestDB(t)
	if _, err := db.Exec(`INSERT INTO lore_vectors VALUES(?,'old-model',384,zeroblob(384),123,'old-hash')`, id); err != nil {
		t.Fatal(err)
	}
	encoder := repairEmbedder{fn: func(context.Context, string) ([]float32, error) { return nil, errors.New("temporary encode failure") }}
	res, err := Backfill(ctx, BackfillOptions{DB: db, Embedder: encoder, ModelID: canonModelID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Failed != 1 || res.Embedded != 0 {
		t.Fatalf("result=%+v", res)
	}
	var hash string
	var encoded int
	if err := db.QueryRow(`SELECT content_hash,encoded_at FROM lore_vectors WHERE entry_id=?`, id).Scan(&hash, &encoded); err != nil {
		t.Fatal(err)
	}
	if hash != "old-hash" || encoded != 123 {
		t.Fatal("failure replaced existing vector")
	}
	repaired, err := Backfill(ctx, BackfillOptions{DB: db, Embedder: NewDeterministicEmbedder(), ModelID: canonModelID})
	if err != nil {
		t.Fatal(err)
	}
	if repaired.Embedded != 1 {
		t.Fatalf("restart repair=%+v", repaired)
	}
}

func TestRepairDoesNotOverwriteConcurrentSource(t *testing.T) {
	for _, hot := range []bool{false, true} {
		t.Run(map[bool]string{false: "backfill", true: "hot"}[hot], func(t *testing.T) {
			ctx := context.Background()
			db, id := hotTestDB(t)
			source := mustSourceText(t, db, LoreCorpus{}, id)
			started, release := make(chan struct{}), make(chan struct{})
			encoder := repairEmbedder{fn: func(ctx context.Context, text string) ([]float32, error) {
				close(started)
				<-release
				return NewDeterministicEmbedder().Embed(ctx, text)
			}}
			done := make(chan error, 1)
			go func() {
				if hot {
					res, err := WriteVector(ctx, db, HotDeps{Embedder: encoder, ModelID: canonModelID}, id, source)
					if res.Written {
						err = errors.New("stale hot write succeeded")
					}
					done <- err
				} else {
					res, err := Backfill(ctx, BackfillOptions{DB: db, Embedder: encoder, ModelID: canonModelID})
					if res.Embedded != 0 || res.Skipped != 1 {
						err = errors.New("stale backfill write succeeded")
					}
					done <- err
				}
			}()
			<-started
			if _, err := db.Exec(`UPDATE entries SET summary='new source' WHERE id=?`, id); err != nil {
				t.Fatal(err)
			}
			// A successful writer for the edit wins before the old encoder finishes.
			fresh, err := WriteVector(ctx, db, HotDeps{Embedder: NewDeterministicEmbedder(), ModelID: canonModelID}, id, "new source")
			if err != nil || !fresh.Written {
				t.Fatalf("new writer=%+v err=%v", fresh, err)
			}
			close(release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			var hash string
			if err := db.QueryRow(`SELECT content_hash FROM lore_vectors WHERE entry_id=?`, id).Scan(&hash); err != nil {
				t.Fatal(err)
			}
			if hash != ContentHash("new source") {
				t.Fatal("concurrent edit overwritten")
			}
			if got := readMetaInt(t, db, "vector_epoch"); got != fresh.Epoch {
				t.Fatalf("stale attempt changed epoch: %d != %d", got, fresh.Epoch)
			}
		})
	}
}

type projectedLore struct{ LoreCorpus }

func (projectedLore) SourceText(context.Context, *sql.DB, int64) (string, error) {
	return "", errors.New("bulk coverage must not read sources individually")
}

func TestCoverageBulkProjectionAndScopedCandidates(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	ids := seedEntries(t, db, 4)
	if _, err := Backfill(ctx, BackfillOptions{DB: db, Embedder: NewDeterministicEmbedder(), ModelID: canonModelID}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE entries SET status='archived' WHERE id=?`, ids[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE lore_vectors SET content_hash='old' WHERE entry_id=?`, ids[1]); err != nil {
		t.Fatal(err)
	}
	allowed := func(id int64) bool { return id != ids[3] }
	c, err := ReadCoverage(ctx, db, projectedLore{}, canonModelID, allowed)
	if err != nil {
		t.Fatal(err)
	}
	if c.Eligible != 2 || c.Valid != 2 || c.Fresh != 1 || !c.FreshIDs[ids[2]] || c.Stale != 1 {
		t.Fatalf("scope=%+v", c)
	}
	idx := NewIndex(LoreCorpus{}, canonModelID)
	if _, err := idx.LoadFromDB(ctx, db); err != nil {
		t.Fatal(err)
	}
	hits, err := idx.TopKFiltered(make([]int8, Dim), 1, func(id int64) bool { return c.FreshIDs[id] })
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].EntryID != ids[2] {
		t.Fatalf("filtered candidates=%+v", hits)
	}
}

func TestOrphanVectorExcludedAndRepaired(t *testing.T) {
	ctx := context.Background()
	db, id := hotTestDB(t)
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO lore_vectors VALUES(999,'bge-small-en-v1.5-int8-cls',384,zeroblob(384),1,'orphan')`); err != nil {
		t.Fatal(err)
	}
	_, _ = conn.ExecContext(ctx, `PRAGMA foreign_keys=ON`)
	_ = conn.Close()
	idx := NewIndex(LoreCorpus{}, canonModelID)
	if n, err := idx.LoadFromDB(ctx, db); err != nil || n != 0 {
		t.Fatalf("orphan loaded: %d %v", n, err)
	}
	c, err := ReadCoverage(ctx, db, LoreCorpus{}, canonModelID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Eligible != 1 || c.Valid != 0 {
		t.Fatalf("orphan counted: %+v", c)
	}
	res, err := Backfill(ctx, BackfillOptions{DB: db, Embedder: NewDeterministicEmbedder(), ModelID: canonModelID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Embedded != 1 || res.Epoch != 2 {
		t.Fatalf("repair=%+v", res)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM lore_vectors WHERE entry_id!=?`, id).Scan(&count); err != nil || count != 0 {
		t.Fatalf("orphan survives: %d %v", count, err)
	}
}

func BenchmarkReadCoverageBulk(b *testing.B) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE entries(id INTEGER PRIMARY KEY,summary TEXT,status TEXT); CREATE TABLE lore_vectors(entry_id INTEGER PRIMARY KEY,model_id TEXT,dim INTEGER,vec BLOB,content_hash TEXT)`); err != nil {
		b.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		b.Fatal(err)
	}
	for id := range 1000 {
		if _, err := tx.Exec(`INSERT INTO entries VALUES(?,'source','current')`, id); err != nil {
			b.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO lore_vectors VALUES(?,'model',384,zeroblob(384),?)`, id, ContentHash("source")); err != nil {
			b.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for range b.N {
		c, err := ReadCoverage(ctx, db, projectedLore{}, "model", nil)
		if err != nil || c.Fresh != 1000 {
			b.Fatalf("coverage=%+v err=%v", c, err)
		}
	}
}

func TestVectorWritersRejectUnusableEncoderResults(t *testing.T) {
	for _, path := range []string{"hot", "backfill"} {
		for _, name := range []string{"zero", "rounds-to-zero", "nan", "infinity"} {
			t.Run(path+"/"+name, func(t *testing.T) {
				ctx := context.Background()
				db, id := hotTestDB(t)
				text := mustSourceText(t, db, LoreCorpus{}, id)
				if _, err := db.Exec(`INSERT INTO lore_vectors VALUES(?,'old-model',384,zeroblob(384),123,'old-hash')`, id); err != nil {
					t.Fatal(err)
				}
				encoder := repairEmbedder{fn: func(context.Context, string) ([]float32, error) {
					vec := make([]float32, VecDim)
					switch name {
					case "rounds-to-zero":
						vec[0] = 0.001
					case "nan":
						vec[0] = float32(math.NaN())
					case "infinity":
						vec[0] = float32(math.Inf(1))
					}
					return vec, nil
				}}
				if path == "hot" {
					result, err := WriteVector(ctx, db, HotDeps{Embedder: encoder, ModelID: canonModelID}, id, text)
					if err == nil || result.Written {
						t.Fatalf("invalid hot result=%+v err=%v", result, err)
					}
				} else {
					result, err := Backfill(ctx, BackfillOptions{DB: db, Embedder: encoder, ModelID: canonModelID})
					if err != nil || result.Embedded != 0 || result.Failed != 1 {
						t.Fatalf("invalid backfill result=%+v err=%v", result, err)
					}
				}
				var hash string
				var timestamp int
				if err := db.QueryRow(`SELECT content_hash,encoded_at FROM lore_vectors WHERE entry_id=?`, id).Scan(&hash, &timestamp); err != nil {
					t.Fatal(err)
				}
				if hash != "old-hash" || timestamp != 123 {
					t.Fatal("invalid output replaced existing vector")
				}
				if got := readMetaInt(t, db, "vector_epoch"); got != 0 {
					t.Fatalf("invalid output advanced epoch: %d", got)
				}
			})
		}
	}
}
