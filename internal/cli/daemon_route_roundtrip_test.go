package cli

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/mathomhaus/guild/internal/command"
	"github.com/mathomhaus/guild/internal/lore"
	"github.com/mathomhaus/guild/internal/quest"
)

// This file pins the lossless-round-trip requirement behind daemon
// routing: for every routed verb, rendering the domain output AFTER an
// O → JSON → O round trip (plus the production InputRestorer step) must
// produce the same bytes as rendering the original. A verb that fails
// here either needs an InputRestorer hook (state the wire drops) or an
// entry in command's exec exemption list.

// fuzzValue builds a pseudo-random value of type T. Pointers are always
// allocated (handlers return non-nil aggregates on success; nil-safety
// is not what this test targets), numbers stay small and non-negative
// (renderers feed them to strings.Repeat-style arithmetic), strings mix
// ASCII, JSON-escaped characters, and multibyte runes.
func fuzzValue[T any](t *testing.T, rng *rand.Rand) T {
	t.Helper()
	var v T
	rv := reflect.ValueOf(&v).Elem()
	fuzzInto(t, rng, rv, 0)
	return v
}

var fuzzWords = []string{
	"alpha", "beta-2", "quoted \"text\"", "newline\nline", "tab\tcol",
	"emoji ⚔️ path", "ünïcode", "trailing space ", "<TS>", "a/b/c.go",
}

func fuzzString(rng *rand.Rand) string {
	return fuzzWords[rng.Intn(len(fuzzWords))]
}

var timeType = reflect.TypeOf(time.Time{})

func fuzzInto(t *testing.T, rng *rand.Rand, v reflect.Value, depth int) {
	t.Helper()
	if depth > 8 {
		return
	}
	if v.Type() == timeType {
		// Random instants with sub-second precision; RFC 3339 nano
		// round-trips them exactly.
		sec := rng.Int63n(2_000_000_000)
		v.Set(reflect.ValueOf(time.Unix(sec, int64(rng.Intn(1_000_000_000))).UTC()))
		return
	}
	switch v.Kind() {
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fuzzInto(t, rng, v.Elem(), depth+1)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			f := v.Field(i)
			if !f.CanSet() { // unexported: stays zero, never crosses the wire
				continue
			}
			fuzzInto(t, rng, f, depth+1)
		}
	case reflect.Slice:
		n := rng.Intn(4) // 0 keeps the nil-slice arm exercised too
		if n == 0 {
			return
		}
		s := reflect.MakeSlice(v.Type(), n, n)
		for i := 0; i < n; i++ {
			fuzzInto(t, rng, s.Index(i), depth+1)
		}
		v.Set(s)
	case reflect.Map:
		n := rng.Intn(3)
		if n == 0 {
			return
		}
		m := reflect.MakeMapWithSize(v.Type(), n)
		for i := 0; i < n; i++ {
			k := reflect.New(v.Type().Key()).Elem()
			fuzzInto(t, rng, k, depth+1)
			val := reflect.New(v.Type().Elem()).Elem()
			fuzzInto(t, rng, val, depth+1)
			m.SetMapIndex(k, val)
		}
		v.Set(m)
	case reflect.String:
		v.SetString(fuzzString(rng))
	case reflect.Bool:
		v.SetBool(rng.Intn(2) == 0)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(int64(rng.Intn(40)))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(rng.Uint64() % 40)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(float64(rng.Intn(4000)) / 100) // finite, short decimal
	case reflect.Interface, reflect.Chan, reflect.Func:
		t.Fatalf("routed output carries a non-serializable %s field (%s)", v.Kind(), v.Type())
	default:
		t.Fatalf("fuzzInto: unhandled kind %s (%s)", v.Kind(), v.Type())
	}
}

// roundTripCase builds the subtest for one routed verb.
func roundTripCase[I, O any](c *command.Command[I, O]) (name string, run func(t *testing.T)) {
	return c.Name, func(t *testing.T) {
		if reason, exempt := command.ExecExemptionReason(c.Name); exempt {
			t.Skipf("exec-exempt: %s", reason)
		}
		rng := rand.New(rand.NewSource(0x5eed)) //nolint:gosec // deterministic fuzz, not crypto
		sinks := []command.CLISink{{NoEmoji: false}, {NoEmoji: true}}
		for i := 0; i < 25; i++ {
			o := fuzzValue[O](t, rng)
			in := fuzzValue[I](t, rng)
			// Baseline mirrors the handler contract for verbs that carry
			// their input in the output (quest list): output.In == input.
			if r, ok := any(&o).(command.InputRestorer[I]); ok {
				r.RestoreInput(in)
			}

			buf, err := json.Marshal(o)
			if err != nil {
				t.Fatalf("iter %d: marshal: %v", i, err)
			}
			var o2 O
			if err := json.Unmarshal(buf, &o2); err != nil {
				t.Fatalf("iter %d: unmarshal: %v", i, err)
			}
			// The production decode path (command.dispatchHandler) applies
			// the same restore on the wire value.
			if r, ok := any(&o2).(command.InputRestorer[I]); ok {
				r.RestoreInput(in)
			}

			for _, sink := range sinks {
				before := c.CLIFormat(sink, o)
				after := c.CLIFormat(sink, o2)
				if before != after {
					t.Fatalf("iter %d (noEmoji=%v): CLIFormat diverged after JSON round trip\n--- before ---\n%s\n--- after ---\n%s",
						i, sink.NoEmoji, before, after)
				}
				if c.CLIWarnings != nil {
					wb := c.CLIWarnings(sink, o)
					wa := c.CLIWarnings(sink, o2)
					if wb != wa {
						t.Fatalf("iter %d (noEmoji=%v): CLIWarnings diverged after JSON round trip\n--- before ---\n%s\n--- after ---\n%s",
							i, sink.NoEmoji, wb, wa)
					}
				}
			}

			// --json and the agent envelope emit json.Marshal(O) directly:
			// re-marshalling the decoded value must be byte-stable too.
			buf2, err := json.Marshal(o2)
			if err != nil {
				t.Fatalf("iter %d: re-marshal: %v", i, err)
			}
			if !bytes.Equal(buf, buf2) {
				t.Fatalf("iter %d: JSON re-marshal diverged\n--- first ---\n%s\n--- second ---\n%s", i, buf, buf2)
			}
		}
	}
}

// TestDaemonRoute_OutputsSurviveJSONRoundTrip runs the render-stability
// fuzz for EVERY verb the CLI routes: the same set NewDaemonExecHandler
// registers. The case list below is diffed against the verbs the CLI
// actually bound (cliRegistryBoundVerbs) before any subtest runs, so
// adding a bind without extending this list fails right here.
func TestDaemonRoute_OutputsSurviveJSONRoundTrip(t *testing.T) {
	type namedCase struct {
		name string
		run  func(t *testing.T)
	}
	var cases []namedCase
	add := func(name string, run func(t *testing.T)) {
		cases = append(cases, namedCase{name: name, run: run})
	}

	add(roundTripCase(quest.AcceptCommand))
	add(roundTripCase(quest.FulfillCommand))
	add(roundTripCase(quest.ForfeitCommand))
	add(roundTripCase(quest.JournalCommand))
	add(roundTripCase(quest.BriefCommand))
	add(roundTripCase(quest.ActiveCommand))
	add(roundTripCase(quest.SummonCommand))
	add(roundTripCase(quest.OrdersCommand))
	add(roundTripCase(quest.CampfireCommand))
	add(roundTripCase(quest.EpicCommand))
	add(roundTripCase(quest.PostCommand))
	add(roundTripCase(quest.UpdateCommand))
	add(roundTripCase(quest.ScrollCommand))
	add(roundTripCase(quest.ListCommand))
	add(roundTripCase(quest.GuildCommand))
	add(roundTripCase(quest.PulseCommand))
	add(roundTripCase(quest.SearchCommand))

	add(roundTripCase(lore.OathCommand))
	add(roundTripCase(lore.DossierCommand))
	add(roundTripCase(lore.EchoesCommand))
	add(roundTripCase(lore.WhispersCommand))
	add(roundTripCase(lore.ListCommand))
	add(roundTripCase(lore.RipplesCommand))
	add(roundTripCase(lore.InscribeCommand))
	add(roundTripCase(lore.UpdateCommand))
	add(roundTripCase(lore.CatalogCommand))
	add(roundTripCase(lore.SealCommand))
	add(roundTripCase(lore.LinkCommand))
	add(roundTripCase(lore.UnlinkCommand))
	add(roundTripCase(lore.ReforgeCommand))
	add(roundTripCase(lore.InquestCommand))
	add(roundTripCase(lore.MeldCommand))
	add(roundTripCase(lore.CommuneCommand))
	add(roundTripCase(lore.EmbedderHealthCommand))
	add(roundTripCase(lore.EmbedRebuildCommand))
	add(roundTripCase(lore.CoverageReconcileCommand))

	// Guard before fuzzing: the hand-maintained case list above must
	// stay in lockstep with the verbs the CLI bound through the
	// registry (exec-exempt verbs included; their subtests self-skip).
	caseNames := make([]string, 0, len(cases))
	for _, c := range cases {
		caseNames = append(caseNames, c.name)
	}
	if missing, extra := diffVerbSets(cliRegistryBoundVerbs, caseNames); len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("round-trip case list drifted from CLI-bound registry verbs\nbound but not fuzzed: %v\nfuzzed but not bound: %v", missing, extra)
	}

	for _, c := range cases {
		t.Run(c.name, c.run)
	}
}

// diffVerbSets reports the names in want missing from got and the
// names in got that are not in want, both sorted. Duplicates collapse.
func diffVerbSets(want, got []string) (missing, extra []string) {
	wantSet := make(map[string]bool, len(want))
	for _, n := range want {
		wantSet[n] = true
	}
	gotSet := make(map[string]bool, len(got))
	for _, n := range got {
		gotSet[n] = true
	}
	for n := range wantSet {
		if !gotSet[n] {
			missing = append(missing, n)
		}
	}
	for n := range gotSet {
		if !wantSet[n] {
			extra = append(extra, n)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	return missing, extra
}

// TestDaemonRoute_RegistryCoversCLIRoutedVerbs pins the daemon-side
// dispatch table against drift: every verb the CLI binds through the
// registry (minus exec exemptions) must be executable in the daemon,
// the daemon must not register verbs the CLI never bound, and the
// exemption list must stay exactly as documented.
func TestDaemonRoute_RegistryCoversCLIRoutedVerbs(t *testing.T) {
	if len(cliRegistryBoundVerbs) == 0 {
		t.Fatal("no CLI-bound registry verbs recorded; bind helpers lost their tracking append")
	}

	wantExempt := map[string]bool{"quest_orders": true}

	// The daemon-side table must cover exactly the bound, non-exempt set.
	var want []string
	for _, name := range cliRegistryBoundVerbs {
		if _, exempt := command.ExecExemptionReason(name); exempt {
			continue
		}
		want = append(want, name)
	}
	got := newDaemonExecRegistry(nil, nil).Names()
	if missing, extra := diffVerbSets(want, got); len(missing) > 0 || len(extra) > 0 {
		t.Errorf("daemon exec registry drifted from CLI-bound registry verbs\nbound but not registered: %v\nregistered but not bound: %v", missing, extra)
	}

	// The exemption list must stay exactly as documented: every
	// documented exemption is live, and no bound verb is exempt without
	// being documented here.
	for name := range wantExempt {
		if _, ok := command.ExecExemptionReason(name); !ok {
			t.Errorf("expected %s to be exec-exempt", name)
		}
	}
	for _, name := range cliRegistryBoundVerbs {
		if _, exempt := command.ExecExemptionReason(name); exempt && !wantExempt[name] {
			t.Errorf("bound verb %s is exec-exempt but undocumented in this test's wantExempt", name)
		}
	}
}
