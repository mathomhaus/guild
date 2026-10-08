package eval

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/mathomhaus/guild/internal/lore"
	"github.com/mathomhaus/guild/internal/lore/embed"
)

// Corpus text and held-out questions are authored independently. The held-out
// labels include missing specifics in otherwise familiar domains: a related
// subject is not sufficient to answer those questions.
//
//go:embed testdata/relevance_corpus.json
var relevanceCorpusJSON []byte

//go:embed testdata/relevance_heldout.json
var relevanceQueriesJSON []byte

type relevanceEntry struct {
	Key     string `json:"key"`
	Project string `json:"project"`
	Title   string `json:"title"`
	Summary string `json:"summary"`
	AgeDays int    `json:"age_days"`
	Status  string `json:"status"`
}

type RelevanceQuery struct {
	Name      string   `json:"name"`
	Query     string   `json:"query"`
	Project   string   `json:"project"`
	SinceDays int      `json:"since_days"`
	Relevant  []string `json:"relevant"`
}

// RelevanceOptions selects genuine lexical, vector or hybrid retrieval.
// A deterministic embedder can exercise wiring but is not semantic validation.
// QueryPrefix is an explicit experimental query-only representation, never
// applied to persisted source text or promoted to the production default.
type RelevanceOptions struct {
	Mode        string
	Embedder    embed.Embedder
	ModelID     string
	QueryPrefix string
}

type RelevanceJudgment struct {
	Name           string    `json:"name"`
	Relevant       []string  `json:"relevant"`
	Retrieved      []string  `json:"retrieved"`
	ReciprocalRank float64   `json:"reciprocal_rank"`
	Answerability  string    `json:"answerability"`
	Similarities   []float64 `json:"similarities,omitempty"`
}

type RelevanceMetrics struct {
	Positives    int     `json:"positives"`
	Negatives    int     `json:"negatives"`
	HitAt1       float64 `json:"hit_at_1"`
	HitAt5       float64 `json:"hit_at_5"`
	HitAt10      float64 `json:"hit_at_10"`
	RecallAt1    float64 `json:"recall_at_1"`
	RecallAt5    float64 `json:"recall_at_5"`
	RecallAt10   float64 `json:"recall_at_10"`
	MRR          float64 `json:"mrr"`
	PrecisionAt5 float64 `json:"precision_at_5"`
	// NegativeCandidateRate counts nonempty lists on no-answer questions.
	// It is not accuracy: current retrieval explicitly leaves answerability
	// unknown, and must not assert that candidate existence proves an answer.
	NegativeCandidateRate    float64 `json:"negative_candidate_rate"`
	NegativeEmptyRate        float64 `json:"negative_empty_rate"`
	UnknownAnswerabilityRate float64 `json:"unknown_answerability_rate"`
}

type ThresholdTrial struct {
	MinimumSimilarity float64 `json:"minimum_similarity"`
	PositiveHitAt5    float64 `json:"positive_hit_at_5"`
	NegativeEmptyRate float64 `json:"negative_empty_rate"`
}

type RelevanceReport struct {
	Mode            string              `json:"mode"`
	QueryPrefix     string              `json:"query_prefix,omitempty"`
	Metrics         RelevanceMetrics    `json:"metrics"`
	Judgments       []RelevanceJudgment `json:"judgments"`
	ThresholdTrials []ThresholdTrial    `json:"threshold_trials,omitempty"`
}

// RelevancePassed is a normative recall/ranking gate, separate from the
// diagnostic adversarial grid. It fails meaningful degradation rather than
// pinning a known retrieval failure as an expected outcome.
func (r RelevanceReport) RelevancePassed() bool {
	return r.Metrics.HitAt5 >= 0.85 && r.Metrics.MRR >= 0.80
}

func RunRelevance(ctx context.Context, opts RelevanceOptions) (RelevanceReport, error) {
	if opts.Mode == "" {
		opts.Mode = "lexical"
	}
	if opts.Mode != "lexical" && opts.Mode != "vector" && opts.Mode != "hybrid" {
		return RelevanceReport{}, fmt.Errorf("eval: unsupported relevance mode %q", opts.Mode)
	}
	if opts.Mode != "lexical" && (opts.Embedder == nil || opts.ModelID == "") {
		return RelevanceReport{}, fmt.Errorf("eval: %s requires an explicit embedder and model identity", opts.Mode)
	}
	var corpus []relevanceEntry
	var queries []RelevanceQuery
	if err := json.Unmarshal(relevanceCorpusJSON, &corpus); err != nil {
		return RelevanceReport{}, err
	}
	if err := json.Unmarshal(relevanceQueriesJSON, &queries); err != nil {
		return RelevanceReport{}, err
	}
	db, err := openScratchDB(ctx)
	if err != nil {
		return RelevanceReport{}, err
	}
	defer func() { _ = db.Close() }()
	keys, err := seedRelevance(ctx, db, corpus, opts)
	if err != nil {
		return RelevanceReport{}, err
	}
	var deps *lore.EmbedDeps
	if opts.Mode != "lexical" {
		idx := embed.NewIndex(embed.LoreCorpus{}, opts.ModelID)
		if _, err := idx.LoadFromDB(ctx, db); err != nil {
			return RelevanceReport{}, err
		}
		deps = &lore.EmbedDeps{Embedder: queryPrefixEmbedder{Embedder: opts.Embedder, prefix: opts.QueryPrefix}, Index: idx, ModelID: opts.ModelID}
	}
	report := RelevanceReport{Mode: opts.Mode, QueryPrefix: opts.QueryPrefix}
	for _, q := range queries {
		params := lore.AppraiseParams{Query: q.Query, Project: q.Project, AllProjects: q.Project == "", Since: time.Duration(q.SinceDays) * 24 * time.Hour, Now: referenceNow, Limit: 10, Embed: deps}
		var out *lore.AppraiseOutput
		if opts.Mode == "vector" {
			out, err = rankVectorOnly(ctx, db, params, deps)
		} else {
			out, err = lore.Appraise(ctx, db, params)
		}
		if err != nil {
			return RelevanceReport{}, fmt.Errorf("eval: relevance %q: %w", q.Name, err)
		}
		j := RelevanceJudgment{Name: q.Name, Relevant: q.Relevant, Retrieved: []string{}, Answerability: out.Answerability}
		for _, r := range out.Results {
			j.Retrieved = append(j.Retrieved, keys[r.Entry.ID])
			j.Similarities = append(j.Similarities, r.VectorSimilarity)
		}
		report.Judgments = append(report.Judgments, j)
	}
	report.Metrics = measureRelevance(report.Judgments)
	for i := range report.Judgments {
		report.Judgments[i].ReciprocalRank = reciprocalRank(report.Judgments[i])
	}
	if opts.Mode == "vector" {
		report.ThresholdTrials = trialThresholds(report.Judgments)
	}
	return report, nil
}

func seedRelevance(ctx context.Context, db *sql.DB, corpus []relevanceEntry, opts RelevanceOptions) (map[int64]string, error) {
	keys := map[int64]string{}
	if opts.Mode != "lexical" {
		for _, kv := range [][2]string{{"embedder_state", "enabled"}, {"embedder_model_id", opts.ModelID}} {
			if _, err := db.ExecContext(ctx, `INSERT OR REPLACE INTO meta(key,value)VALUES(?,?)`, kv[0], kv[1]); err != nil {
				return nil, err
			}
		}
	}
	for _, e := range corpus {
		if _, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO projects(id,path)VALUES(?,?)`, e.Project, "/eval/"+e.Project); err != nil {
			return nil, err
		}
		status := e.Status
		if status == "" {
			status = "current"
		}
		created := referenceNow.AddDate(0, 0, -e.AgeDays).Format(time.RFC3339)
		r, err := db.ExecContext(ctx, `INSERT INTO entries(project_id,topic,kind,title,summary,status,created_at,updated_at)VALUES(?,'relevance','decision',?,?,?,?,?)`, e.Project, e.Title, e.Summary, status, created, created)
		if err != nil {
			return nil, err
		}
		id, err := r.LastInsertId()
		if err != nil {
			return nil, err
		}
		keys[id] = e.Key
		if opts.Mode != "lexical" && status != "parked" {
			if _, err := embed.WriteVector(ctx, db, embed.HotDeps{Embedder: opts.Embedder, ModelID: opts.ModelID, Corpus: embed.LoreCorpus{}}, id, e.Summary); err != nil {
				return nil, err
			}
		}
	}
	return keys, nil
}

type queryPrefixEmbedder struct {
	embed.Embedder
	prefix string
}

func (e queryPrefixEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	return e.Embedder.Embed(ctx, e.prefix+text)
}

// The pure vector ablation uses the same eligible scope and fresh-vector
// gate as production, but never adds lexical evidence or title protection.
func rankVectorOnly(ctx context.Context, db *sql.DB, p lore.AppraiseParams, deps *lore.EmbedDeps) (*lore.AppraiseOutput, error) {
	rows, err := db.QueryContext(ctx, `SELECT id,project_id,title,status,created_at FROM entries`)
	if err != nil {
		return nil, err
	}
	entries := map[int64]*lore.Entry{}
	for rows.Next() {
		e := &lore.Entry{}
		var created string
		if err := rows.Scan(&e.ID, &e.ProjectID, &e.Title, &e.Status, &created); err != nil {
			_ = rows.Close()
			return nil, err
		}
		e.CreatedAt, err = time.Parse(time.RFC3339, created)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		if e.Status != "current" && e.Status != "seed" && e.Status != "exploring" && e.Status != "imported" {
			continue
		}
		if p.Project != "" && e.ProjectID != p.Project {
			continue
		}
		if p.Since > 0 && e.CreatedAt.Before(p.Now.Add(-p.Since)) {
			continue
		}
		entries[e.ID] = e
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	coverage, err := embed.ReadCoverage(ctx, db, embed.LoreCorpus{}, deps.ModelID, func(id int64) bool { return entries[id] != nil })
	if err != nil {
		return nil, err
	}
	v, err := deps.Embedder.Embed(ctx, p.Query)
	if err != nil {
		return nil, err
	}
	ranked, err := deps.Index.TopKFiltered(embed.Quantize(v), p.Limit, func(id int64) bool { return coverage.FreshIDs[id] })
	if err != nil {
		return nil, err
	}
	out := &lore.AppraiseOutput{RetrievalMode: "vector", Answerability: lore.AnswerabilityUnknown}
	for _, r := range ranked {
		out.Results = append(out.Results, lore.AppraiseResult{Entry: entries[r.EntryID], Score: float64(r.Score), SemanticMatch: true, VectorSimilarity: float64(r.Score) / (127 * 127)})
	}
	if len(out.Results) == 0 {
		out.Answerability = lore.AnswerabilityNoMatch
	}
	return out, nil
}

func reciprocalRank(j RelevanceJudgment) float64 {
	for i, key := range j.Retrieved {
		for _, want := range j.Relevant {
			if key == want {
				return 1 / float64(i+1)
			}
		}
	}
	return 0
}

func measureRelevance(js []RelevanceJudgment) RelevanceMetrics {
	m := RelevanceMetrics{}
	var precision, unknown float64
	for _, j := range js {
		if j.Answerability == lore.AnswerabilityUnknown {
			unknown++
		}
		if len(j.Relevant) == 0 {
			m.Negatives++
			if len(j.Retrieved) > 0 {
				m.NegativeCandidateRate++
			}
			continue
		}
		m.Positives++
		m.MRR += reciprocalRank(j)
		counts := [3]float64{}
		for i, key := range j.Retrieved {
			for _, want := range j.Relevant {
				if key == want {
					for n, k := range []int{1, 5, 10} {
						if i < k {
							counts[n]++
						}
					}
					break
				}
			}
		}
		if counts[0] > 0 {
			m.HitAt1++
		}
		if counts[1] > 0 {
			m.HitAt5++
		}
		if counts[2] > 0 {
			m.HitAt10++
		}
		m.RecallAt1 += counts[0] / float64(len(j.Relevant))
		m.RecallAt5 += counts[1] / float64(len(j.Relevant))
		m.RecallAt10 += counts[2] / float64(len(j.Relevant))
		precision += counts[1] / 5
	}
	if m.Positives > 0 {
		n := float64(m.Positives)
		m.HitAt1 /= n
		m.HitAt5 /= n
		m.HitAt10 /= n
		m.RecallAt1 /= n
		m.RecallAt5 /= n
		m.RecallAt10 /= n
		m.MRR /= n
	}
	// Precision includes negative questions, whose retrieved candidates are
	// irrelevant by definition. Empty slots count as zero at fixed cutoff 5.
	if len(js) > 0 {
		m.PrecisionAt5 = precision / float64(len(js))
		m.UnknownAnswerabilityRate = unknown / float64(len(js))
	}
	if m.Negatives > 0 {
		m.NegativeCandidateRate /= float64(m.Negatives)
		m.NegativeEmptyRate = 1 - m.NegativeCandidateRate
	}
	return m
}

func trialThresholds(js []RelevanceJudgment) []ThresholdTrial {
	var trials []ThresholdTrial
	// This sweep describes tradeoffs; it does not select a production cutoff.
	for _, threshold := range []float64{0.4, 0.5, 0.6, 0.7, 0.8, 0.9} {
		filtered := make([]RelevanceJudgment, 0, len(js))
		for _, j := range js {
			f := j
			f.Retrieved = nil
			for i, key := range j.Retrieved {
				if i < len(j.Similarities) && j.Similarities[i] >= threshold {
					f.Retrieved = append(f.Retrieved, key)
				}
			}
			filtered = append(filtered, f)
		}
		m := measureRelevance(filtered)
		trials = append(trials, ThresholdTrial{threshold, m.HitAt5, m.NegativeEmptyRate})
	}
	sort.Slice(trials, func(i, j int) bool { return trials[i].MinimumSimilarity < trials[j].MinimumSimilarity })
	return trials
}
