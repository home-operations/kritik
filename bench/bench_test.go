//go:build bench

package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/home-operations/kritik/internal/contextpack"
	"github.com/home-operations/kritik/internal/gitfetch"
	"github.com/home-operations/kritik/internal/model"
	"github.com/home-operations/kritik/internal/review"
)

// Modes are the ablations: what the prompt carries beyond the diff.
var modes = map[string]func([]contextpack.Chunk) []contextpack.Chunk{
	// diff: the diff alone.
	"diff": func([]contextpack.Chunk) []contextpack.Chunk { return nil },
	// overlay: whole declarations the diff touches.
	"overlay": func(cs []contextpack.Chunk) []contextpack.Chunk { return keep(cs, contextpack.StageOverlay) },
	// context: stages 1 to 3 as the runner builds them.
	"context": func(cs []contextpack.Chunk) []contextpack.Chunk { return cs },
}

func keep(cs []contextpack.Chunk, stage string) []contextpack.Chunk {
	var out []contextpack.Chunk
	for _, c := range cs {
		if c.Stage == stage {
			out = append(out, c)
		}
	}
	return out
}

// caseResult is one case under one mode.
type caseResult struct {
	ID          string           `json:"id"`
	Mode        string           `json:"mode"`
	Findings    []review.Finding `json:"findings"`
	Dropped     int              `json:"dropped"`
	MustTotal   int              `json:"must_total"`
	MustHit     int              `json:"must_hit"`
	NiceTotal   int              `json:"nice_total"`
	NiceHit     int              `json:"nice_hit"`
	Matched     int              `json:"matched_findings"`
	Context     map[string]int   `json:"context_chunks"`
	PromptChars int              `json:"prompt_chars"`
	Input       int64            `json:"input_tokens"`
	Cached      int64            `json:"cached_tokens"`
	Output      int64            `json:"output_tokens"`
	CostUSD     float64          `json:"cost_usd"`
	Latency     time.Duration    `json:"latency_ns"`
	Error       string           `json:"error,omitempty"`
}

// report is what a run writes.
type report struct {
	Model   string                `json:"model"`
	When    time.Time             `json:"when"`
	Dry     bool                  `json:"dry"`
	Cases   int                   `json:"cases"`
	Modes   map[string]modeTotals `json:"modes"`
	Results []caseResult          `json:"results"`
}

type modeTotals struct {
	Cases       int     `json:"cases"`
	Errors      int     `json:"errors"`
	MustRecall  float64 `json:"must_recall"`
	NiceRecall  float64 `json:"nice_recall"`
	Findings    int     `json:"findings"`
	Matched     int     `json:"matched_findings"`
	MatchRate   float64 `json:"match_rate"`
	CostUSD     float64 `json:"cost_usd"`
	MeanLatency float64 `json:"mean_latency_s"`
	InputTokens int64   `json:"input_tokens"`
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// TestBench runs the corpus. Environment:
//
//	OPENROUTER_API_KEY     model key (required unless KRITIK_BENCH_DRY=1)
//	KRITIK_BENCH_MODEL     model id on OpenRouter (default openai/gpt-6-sol)
//	KRITIK_BENCH_MODES     comma list of modes (default diff,context)
//	KRITIK_BENCH_CASES     corpus glob (default bench/cases/*.yaml)
//	KRITIK_BENCH_LIMIT     at most this many cases (default all)
//	KRITIK_BENCH_ONLY      run one case id
//	KRITIK_BENCH_DRY       1: fetch, build context and prompts, call no model
//	KRITIK_BENCH_OUT       results directory (default bench/results)
func TestBench(t *testing.T) {
	ctx := context.Background()
	dry := os.Getenv("KRITIK_BENCH_DRY") == "1"
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" && !dry {
		t.Skip("OPENROUTER_API_KEY not set (use KRITIK_BENCH_DRY=1 to run without a model)")
	}
	cases, err := Load(envOr("KRITIK_BENCH_CASES", "cases/*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if only := os.Getenv("KRITIK_BENCH_ONLY"); only != "" {
		var sel []Case
		for _, c := range cases {
			if c.ID == only {
				sel = append(sel, c)
			}
		}
		cases = sel
	}
	if lim, _ := strconv.Atoi(os.Getenv("KRITIK_BENCH_LIMIT")); lim > 0 && lim < len(cases) {
		cases = cases[:lim]
	}
	if len(cases) == 0 {
		t.Fatal("no cases selected")
	}
	modeNames := strings.Split(envOr("KRITIK_BENCH_MODES", "diff,context"), ",")
	for _, m := range modeNames {
		if _, ok := modes[m]; !ok {
			t.Fatalf("unknown mode %q; modes are diff, overlay, context", m)
		}
	}
	modelID := envOr("KRITIK_BENCH_MODEL", "openai/gpt-6-sol")
	var completer model.Completer
	if !dry {
		if completer, err = model.NewOpenRouter(key, nil); err != nil {
			t.Fatal(err)
		}
	}

	rep := report{Model: modelID, When: time.Now().UTC(), Dry: dry, Cases: len(cases), Modes: map[string]modeTotals{}}
	for _, c := range cases {
		t.Logf("%s: fetching %s..%s", c.ID, c.Base[:7], c.Head[:7])
		res, err := gitfetch.Run(ctx, gitfetch.Fetch{CloneURL: c.CloneURL, Token: os.Getenv("GITHUB_TOKEN"), Head: c.Head, Base: c.Base})
		if err != nil {
			t.Logf("%s: fetch failed: %v", c.ID, err)
			for _, m := range modeNames {
				rep.Results = append(rep.Results, caseResult{ID: c.ID, Mode: m, Error: "fetch: " + err.Error()})
			}
			continue
		}
		headTree, _ := res.Head.Tree()
		baseTree, _ := res.Base.Tree()
		chunks, stats, err := contextpack.Build(ctx, contextpack.Input{Head: headTree, Base: baseTree, Diff: res.Diff, Changed: res.Changed}, contextpack.DefaultOptions)
		if err != nil {
			t.Logf("%s: context failed: %v", c.ID, err)
		}
		anchors := review.Anchors(res.Diff)
		for _, m := range modeNames {
			cr := caseResult{ID: c.ID, Mode: m, Context: map[string]int{}}
			selected := modes[m](chunks)
			for _, ch := range selected {
				cr.Context[ch.Stage]++
			}
			msg, _, _ := review.Build(review.Input{
				Repository: c.Repository, Number: c.PR, Title: c.Title, Author: "author", BaseRef: "main",
				Changed: res.Changed, Diff: res.Diff, Context: selected,
			})
			cr.PromptChars = len(msg)
			for _, e := range c.Expected {
				if e.Must {
					cr.MustTotal++
				} else {
					cr.NiceTotal++
				}
			}
			if !dry {
				started := time.Now()
				resp, err := completer.Complete(ctx, model.CompletionRequest{
					System: review.System, User: msg, Model: modelID, Schema: review.Schema(), SchemaName: "findings", MaxTokens: 4096,
				})
				cr.Latency = time.Since(started)
				if err != nil {
					cr.Error = err.Error()
					t.Logf("%s [%s]: model error: %v", c.ID, m, err)
				} else {
					cr.Input, cr.Cached, cr.Output, cr.CostUSD = resp.InputTokens, resp.CachedTokens, resp.OutputTokens, resp.CostUSD
					parsed, dropped, err := review.Parse(resp.Raw, anchors)
					if err != nil {
						cr.Error = err.Error()
					} else {
						cr.Findings, cr.Dropped = parsed.Findings, len(dropped)
						score(&cr, c)
					}
				}
			}
			t.Logf("%s [%s]: must %d/%d nice %d/%d findings %d matched %d cost $%.4f context %v %s",
				c.ID, m, cr.MustHit, cr.MustTotal, cr.NiceHit, cr.NiceTotal, len(cr.Findings), cr.Matched, cr.CostUSD, cr.Context, errText(cr.Error))
			rep.Results = append(rep.Results, cr)
		}
		_ = res.Close()
		_ = stats
	}
	for _, m := range modeNames {
		rep.Modes[m] = totals(rep.Results, m)
	}
	printSummary(t, rep)
	writeReport(t, rep)
}

func score(cr *caseResult, c Case) {
	for _, e := range c.Expected {
		hit := false
		for _, f := range cr.Findings {
			if e.Matches(f.Path, f.Line) {
				hit = true
				break
			}
		}
		if hit && e.Must {
			cr.MustHit++
		} else if hit {
			cr.NiceHit++
		}
	}
	for _, f := range cr.Findings {
		for _, e := range c.Expected {
			if e.Matches(f.Path, f.Line) {
				cr.Matched++
				break
			}
		}
	}
}

func totals(results []caseResult, mode string) modeTotals {
	var tot modeTotals
	var must, mustHit, nice, niceHit int
	var latency time.Duration
	for _, r := range results {
		if r.Mode != mode {
			continue
		}
		tot.Cases++
		if r.Error != "" {
			tot.Errors++
			continue
		}
		must += r.MustTotal
		mustHit += r.MustHit
		nice += r.NiceTotal
		niceHit += r.NiceHit
		tot.Findings += len(r.Findings)
		tot.Matched += r.Matched
		tot.CostUSD += r.CostUSD
		tot.InputTokens += r.Input
		latency += r.Latency
	}
	if must > 0 {
		tot.MustRecall = float64(mustHit) / float64(must)
	}
	if nice > 0 {
		tot.NiceRecall = float64(niceHit) / float64(nice)
	}
	if tot.Findings > 0 {
		tot.MatchRate = float64(tot.Matched) / float64(tot.Findings)
	}
	if n := tot.Cases - tot.Errors; n > 0 {
		tot.MeanLatency = latency.Seconds() / float64(n)
	}
	return tot
}

func printSummary(t *testing.T, rep report) {
	t.Helper()
	names := make([]string, 0, len(rep.Modes))
	for m := range rep.Modes {
		names = append(names, m)
	}
	sort.Strings(names)
	var b strings.Builder
	fmt.Fprintf(&b, "\nmodel %s, %d cases%s\n", rep.Model, rep.Cases, map[bool]string{true: " (dry: no model calls)", false: ""}[rep.Dry])
	fmt.Fprintf(&b, "%-8s %6s %6s %11s %11s %9s %9s %9s %8s\n", "mode", "cases", "errors", "must recall", "nice recall", "findings", "matched", "cost usd", "mean s")
	for _, m := range names {
		x := rep.Modes[m]
		fmt.Fprintf(&b, "%-8s %6d %6d %10.0f%% %10.0f%% %9d %9d %9.4f %8.1f\n",
			m, x.Cases, x.Errors, x.MustRecall*100, x.NiceRecall*100, x.Findings, x.Matched, x.CostUSD, x.MeanLatency)
	}
	b.WriteString("matched = findings that land on an expected range; the rest need a human label for precision.\n")
	t.Log(b.String())
}

func writeReport(t *testing.T, rep report) {
	t.Helper()
	dir := envOr("KRITIK_BENCH_OUT", "results")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(dir, rep.When.Format("20060102-150405")+".json")
	raw, _ := json.MarshalIndent(rep, "", "  ")
	if err := os.WriteFile(name, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("report written to %s", name)
}

func errText(s string) string {
	if s == "" {
		return ""
	}
	return "error: " + s
}
