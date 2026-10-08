# Retrieval policy and quality checks

Lore appraise returns evidence candidates. A nonempty result list does not prove that the requested fact exists. Its structured `Answerability` is `unknown` for candidates and `no_match` for an empty search. `no_match` means no eligible candidate was retrieved, not proof that the knowledge is absent. CLI and MCP output identify candidate relevance as unverified. BM25, fusion scores and approximate cosine similarities are ranking signals, not calibrated answer probabilities.

## Ranking and scope

The default lexical score uses `log1p(-BM25)` instead of a saturating sigmoid. Default recency weight is zero, so strong old evidence retains its relevance. Explicit scoring overrides still support recency; `since` supplies an explicit time constraint. Exact titles use the same whitespace and case normalization as title matching and are selected independently before bounded candidate limits.

Hybrid retrieval requires an enabled embedder state, a usable runtime and index, and at least one fresh compatible vector in the requested scope. Coverage is computed from source/vector rows with model, dimension, shape and content-hash checks. Missing or stale vectors do not disable the entire semantic arm. They remain reachable through the independent lexical arm. An explicit operator disable, no fresh vectors, invalid/zero query vectors, runtime errors or index failures use the lexical fallback.

Project, status and time constraints select eligible IDs before the vector top-60 limit. Freshness further restricts those IDs. Hydration checks the constraints again for concurrent changes. Both scoped and global searches use this policy. Time comparison uses SQLite date conversion so RFC3339 timestamps and SQLite timestamps share one ordering.

Fusion prioritizes the best rank in either arm, with a smaller agreement bonus:

```
one arm: 1 / rank
both arms: 1 / min(rankA, rankB) + 1 / (60 + max(rankA, rankB))
```

This protects strong single-arm results from weak agreement in both tails. Standard RRF remains available separately. Exact-title matches receive priority after the candidate union, and score ties use ascending entity IDs. A noisy arm can still contribute a top candidate, which is another reason to judge the returned evidence before using it.

## Reproducible judgments

`internal/eval/testdata/relevance_corpus.json` contains 18 invented engineering entries. The 25 held-out questions were authored independently of ranking implementation and frozen before results were inspected. Seventeen ask for recorded facts; eight ask for absent facts, including deployment counts, credentials, owners and backup locations whose subjects overlap the corpus. Labels explicitly cover project collisions, identifiers, superseded entries and recent time windows.

Run the deterministic lexical gate and mathematical/wiring regressions with:

```sh
go test ./internal/eval ./internal/lore ./internal/lore/embed
```

The held-out relevance gate requires hit@5 at least 85% and MRR at least 0.80, plus focused tests for exact identifiers, scopes and explicit temporal filters. The eval command also reports these metrics alongside the older three-query adversarial diagnostic and golden parity. That diagnostic still reports its known keyword-stuffing weakness; a strict run can fail on that weakness even when the new relevance gate passes. Its expected diagnostic outcomes are not the quality target.

Real-model validation is opt-in and explicitly skips if assets are unavailable:

```sh
export GUILD_EMBED_TEST_VOCAB=/path/to/vocab.txt
export GUILD_EMBED_TEST_LIB=/path/to/libonnxruntime.so # or .dylib
export GUILD_EMBED_TEST_MODEL=/path/to/model.onnx
go test ./internal/eval -run TestRelevanceRealModel -v
go test ./internal/lore/embed -run 'Test(TokenizerParity|EmbeddingParity|TailDetailTruncation)' -v
```

The model test runs genuine BGE inference for corpus text and queries, vector-only and production hybrid retrieval, and a query-prefix ablation. Deterministic hash embeddings only validate wiring and are never presented as semantic quality evidence. All databases are disposable; no private corpus or installed state is read or changed.

The October 2026 sanitized run with BGE-small-en-v1.5 measured:

| Mode | Hit / recall @1 | @5 | @10 | MRR | Negative queries with candidates |
|---|---:|---:|---:|---:|---:|
| Previous lexical scoring ablation | 47.1% | 94.1% | 94.1% | 0.629 | 75% |
| Current lexical | 82.4% | 94.1% | 94.1% | 0.873 | 75% |
| Real vector | 94.1% | 100% | 100% | 0.971 | 100% |
| Real hybrid | 88.2% | 100% | 100% | 0.931 | 100% |

The previous-scoring ablation restores only the old sigmoid and 0.3 recency weight; it keeps the new filtering and title protection. It isolates scoring effects and is not a complete old-binary benchmark. Each positive has one judged answer, so hit and recall coincide here. The report computes both independently and tests multi-answer judgments. Precision@5 averages relevant returned items divided by five over all 25 questions, including negatives and empty slots: 12.8% lexical and 13.6% vector/hybrid. With these single-answer labels, the attainable maximum is 13.6%; this metric must not be confused with answer accuracy. All nonempty outputs leave answerability unknown.

## Abstention and representation tradeoffs

The vector report sweeps similarity cutoffs as diagnostics without choosing a production threshold. The same run produced:

| Minimum approximate cosine | Positive hit@5 | Negative queries returning no candidates |
|---|---:|---:|
| 0.4 | 100% | 0% |
| 0.5 | 100% | 12.5% |
| 0.6 | 100% | 37.5% |
| 0.7 | 52.9% | 87.5% |
| 0.8 | 17.6% | 100% |
| 0.9 | 0% | 100% |

These small, invented examples do not calibrate a universal threshold. Raising a cutoff suppresses useful paraphrases as well as unsupported questions. The policy therefore abstains from asserting answerability, preserves useful semantic candidates, and reports negative candidate rates honestly. It does not promise automatic semantic no-answer detection.

The BGE query instruction prefix did not change the held-out hit-rate or reciprocal-rank metrics. Candidate ordering and similarity scores can still differ. Five pinned embedding-parity cases passed with worst cosine 1.000000. A controlled tail-detail probe gave two summaries identical first-512-token tensors and identical vectors despite different facts after that window. For a question about the omitted detail, approximate relevance was 0.595 for the truncated representation and 0.802 for a focused detail representation. This establishes the tail-loss mechanism, not a validated general chunking strategy. FTS still indexes summary tails. No default prefix, representation, model identity or reindex policy changes on this evidence; any such change needs broader held-out testing and explicit vector identity migration.

## Performance checks

`appraise_bench_test.go` measures a 1,000-entry eligible scan, sorting 120 candidates, and complete lexical appraise including access telemetry. Query normalization and query tokens are shared across each candidate batch; exact-title flags are prepared before sorting. Hydration does not compute scores that its callers immediately replace. The complete hybrid candidate union and ranking policy remain intact.

On one development machine, three-run medians before/after these allocation reductions were 2.97/0.60 ms for the eligible scan, 2.89/0.037 ms for sorting, and 6.38/3.71 ms for lexical appraise. These synthetic timings isolate implementation costs; they are not a model-inference or production latency claim. Reproduce with:

```sh
go test ./internal/lore -run '^$' -bench 'Benchmark(EligibleEntryIDs|SortAppraiseResults|AppraiseLexical)' -benchmem -count 3
```
