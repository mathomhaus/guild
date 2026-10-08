package embed

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Coverage measures eligible source entities rather than cached meta counters.
// Valid vectors have the requested model and canonical shape; Fresh also
// requires a hash matching the source text. Stale and Invalid are disjoint.
type Coverage struct {
	Eligible, Valid, Fresh, Missing, Stale, Invalid int64
	FreshIDs                                        map[int64]bool
}

func (c Coverage) Ratio() float64 {
	if c.Eligible == 0 {
		return 1
	}
	return float64(c.Fresh) / float64(c.Eligible)
}

// ReadCoverage optionally restricts measurement to caller-selected entity IDs.
// A nil filter measures the whole corpus. Orphans and inactive rows never count.
func ReadCoverage(ctx context.Context, db *sql.DB, corpus VectorCorpus, modelID string, allowed func(int64) bool) (Coverage, error) {
	if corpus == nil {
		corpus = LoreCorpus{}
	}
	c := Coverage{FreshIDs: make(map[int64]bool)}
	// Built-in adapters expose the canonical stored source column so the
	// entity, vector and source hash are measured in one SQLite snapshot.
	projection := "NULL"
	if adapter, ok := corpus.(CorpusSourceProjection); ok {
		projection = "e." + adapter.SourceTextColumn()
	}
	query := fmt.Sprintf(`SELECT e.%s, v.entry_id, v.model_id, v.dim, length(v.vec), v.content_hash, %s FROM %s e LEFT JOIN %s v ON v.entry_id=e.%s WHERE e.%s ORDER BY e.%s`, corpus.EntityIDColumn(), projection, corpus.EntityTable(), corpus.VectorTable(), corpus.EntityIDColumn(), corpus.ActivePredicate(), corpus.EntityIDColumn()) //nolint:gosec // compile-time corpus accessors

	rows, err := db.QueryContext(ctx, query) //nolint:sqlcheck // compile-time corpus accessors
	if err != nil {
		return c, err
	}
	type row struct {
		id                  int64
		vectorID, dim, size sql.NullInt64
		model, hash, source sql.NullString
	}
	var candidates []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.vectorID, &r.model, &r.dim, &r.size, &r.hash, &r.source); err != nil {
			_ = rows.Close()
			return c, err
		}
		if allowed == nil || allowed(r.id) {
			candidates = append(candidates, r)
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return c, err
	}
	for index := range candidates {
		r := &candidates[index]
		c.Eligible++
		if !r.vectorID.Valid {
			c.Missing++
			continue
		}
		if r.model.String != modelID || r.dim.Int64 != VecDim || r.size.Int64 != VecDim {
			c.Invalid++
			continue
		}
		c.Valid++
		text := r.source.String
		if projection == "NULL" {
			text, err = corpus.SourceText(ctx, db, r.id)
			if err != nil {
				return c, err
			}
		}
		if ContentHash(text) != r.hash.String {
			c.Stale++
			continue
		}
		c.Fresh++
		c.FreshIDs[r.id] = true
	}
	return c, nil
}

// sourceTextAt reads canonical source text on the locked writer connection.
// Adapters with computed sources can implement SourceTextQuery to preserve their
// exact assembly inside Tx2. Unsupported adapters fail rather than assuming
// a source column that may differ from their canonical text.
func sourceTextAt(ctx context.Context, conn *sql.Conn, corpus VectorCorpus, id int64) (string, error) {
	query := ""
	if adapter, ok := corpus.(CorpusTransactionalSource); ok {
		query = adapter.SourceTextQuery()
	}
	if query == "" {
		return "", fmt.Errorf("embed: corpus %s has no transactional source lookup", corpus.Name())
	}

	var text string
	err := conn.QueryRowContext(ctx, query, id).Scan(&text) //nolint:sqlcheck // compile-time adapter query
	return text, err
}

// reconcileCoverageTx repairs diagnostic counters inside a writer transaction.
// Freshness is checked by ReadCoverage; cached counts describe valid shape only.
func reconcileCoverageTx(ctx context.Context, conn *sql.Conn, corpus VectorCorpus, modelID string) error {
	query := fmt.Sprintf(`SELECT COUNT(*), COALESCE(SUM(CASE WHEN v.model_id=? AND v.dim=? AND length(v.vec)=? THEN 1 ELSE 0 END),0) FROM %s e LEFT JOIN %s v ON v.entry_id=e.%s WHERE e.%s`, corpus.EntityTable(), corpus.VectorTable(), corpus.EntityIDColumn(), corpus.ActivePredicate()) //nolint:gosec // compile-time corpus accessors
	var den, num int64
	if err := conn.QueryRowContext(ctx, query, modelID, VecDim, VecDim).Scan(&den, &num); err != nil { //nolint:sqlcheck // compile-time corpus accessors
		return err
	}
	for _, kv := range []struct {
		key   string
		value int64
	}{{corpus.MetaKey(FieldVectorCoverageNum), num}, {corpus.MetaKey(FieldVectorCoverageDen), den}} {
		if _, err := conn.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, kv.key, fmt.Sprint(kv.value)); err != nil {
			return err
		}
	}
	return nil
}

// writeEncodedTx validates the current source under the writer lock before
// replacing a vector. Concurrent edits, deletes and inactivation are skips.
func writeEncodedTx(ctx context.Context, conn *sql.Conn, corpus VectorCorpus, id int64, text string, qvec []int8, modelID string) (WriteVectorResult, error) {
	epoch, err := readEpochTxKey(ctx, conn, corpus.MetaKey(FieldVectorEpoch))
	if err != nil {
		return WriteVectorResult{}, err
	}
	result := WriteVectorResult{Epoch: epoch}
	var currentModel string
	if err := conn.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, corpus.MetaKey(FieldEmbedderModelID)).Scan(&currentModel); err != nil {
		return result, err
	}
	if currentModel != modelID {
		return result, nil
	}
	query := fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE %s=? AND %s`, corpus.EntityTable(), corpus.EntityIDColumn(), corpus.ActivePredicate()) //nolint:gosec // compile-time corpus accessors
	var active int
	if err := conn.QueryRowContext(ctx, query, id).Scan(&active); err != nil { //nolint:sqlcheck // compile-time corpus accessors
		return result, err
	}
	if active == 0 {
		return result, nil
	}
	current, err := sourceTextAt(ctx, conn, corpus, id)
	if err != nil {
		return result, err
	}
	if current != text {
		return result, nil
	}
	hash := ContentHash(text)
	blob := make([]byte, len(qvec))
	for j, v := range qvec {
		blob[j] = byte(v)
	}
	query = fmt.Sprintf(`INSERT INTO %s(entry_id,model_id,dim,vec,encoded_at,content_hash) VALUES(?,?,?,?,?,?) ON CONFLICT(entry_id) DO UPDATE SET model_id=excluded.model_id,dim=excluded.dim,vec=excluded.vec,encoded_at=excluded.encoded_at,content_hash=excluded.content_hash WHERE model_id!=excluded.model_id OR dim!=excluded.dim OR length(vec)!=length(excluded.vec) OR content_hash!=excluded.content_hash`, corpus.VectorTable()) //nolint:gosec // compile-time corpus accessor
	res, err := conn.ExecContext(ctx, query, id, modelID, VecDim, blob, time.Now().Unix(), hash)                                                                                                                                                                                                                                                                                                                                             //nolint:sqlcheck // compile-time corpus accessors
	if err != nil {
		return result, err
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return result, err
	}
	if stateCol := corpus.VectorStateColumn(); stateCol != "" {
		query = fmt.Sprintf(`UPDATE %s SET %s='indexed' WHERE %s=?`, corpus.EntityTable(), stateCol, corpus.EntityIDColumn()) //nolint:gosec // compile-time corpus accessors
		if _, err := conn.ExecContext(ctx, query, id); err != nil {                                                           //nolint:sqlcheck // compile-time corpus accessors
			return result, err
		}
	}
	if err := reconcileCoverageTx(ctx, conn, corpus, modelID); err != nil {
		return result, err
	}
	if changed == 0 {
		return result, nil
	}
	epoch++
	if _, err := conn.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, corpus.MetaKey(FieldVectorEpoch), fmt.Sprint(epoch)); err != nil {
		return result, err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, corpus.MetaKey(FieldEmbedLastOKAt), time.Now().UTC().Format(time.RFC3339)); err != nil {
		return result, err
	}
	return WriteVectorResult{Written: true, Epoch: epoch, Vec: qvec, ContentHash: hash}, nil
}
