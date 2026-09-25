package review

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// MaxRenderBytes bounds a rendered comment, marker included. GitHub rejects
// comment bodies over 65,536 characters, and a character is at least a byte.
const MaxRenderBytes = 64 << 10

// renderTimeout bounds one render, parse included.
var renderTimeout = 2 * time.Second

//go:embed templates/summary.md.j2
var defaultSummary string

//go:embed templates/inline.md.j2
var defaultInline string

// Templates are repository-supplied template sources. An empty field means
// the embedded default.
type Templates struct {
	Summary, Inline string
}

// RenderData is what the summary template sees, under the keys number,
// head_sha, model, summary.take, summary.praise, findings (each with path,
// line, severity, title, explanation and suggested_fix), counts.blocking,
// counts.important, counts.nit, notes, incremental, prior_head_sha and
// incomplete. The inline template sees one finding's keys at the top level.
type RenderData struct {
	Number       int
	HeadSHA      string
	Model        string
	Result       Result
	Counts       Counts
	Notes        []string
	Incremental  bool
	PriorHeadSHA string
	// Incomplete, when set, says why the head was not fully reviewed; the
	// default template then states that instead of a verdict.
	Incomplete string
}

var (
	errTooLarge = errors.New("review: rendered output exceeds the size limit")
	errSandbox  = errors.New("review: template exceeded a sandbox limit")
)

// RenderSummary renders the sticky comment. kritik's marker always leads,
// whatever the template does, so sticky discovery cannot be defeated by a
// template. When the repository's template fails, the default is used and
// the returned note, also shown in the comment, says why.
func RenderSummary(ctx context.Context, t Templates, d RenderData) (body string, notes []string) {
	marker := Marker(d.Number) + "\n"
	limit := MaxRenderBytes - len(marker)
	if t.Summary != "" {
		out, err := render(ctx, t.Summary, summaryContext(d), limit)
		if err == nil {
			return marker + out, nil
		}
		note := fallbackNote("summary", err)
		notes = append(notes, note)
		d.Notes = append(slices.Clone(d.Notes), note)
	}
	out, err := render(context.WithoutCancel(ctx), defaultSummary, summaryContext(d), limit)
	if err != nil {
		// The default renders data kritik bounds itself; failing here is a
		// bug, but the comment must still carry the marker and the take.
		out = truncateUTF8(fmt.Sprintf("### kritik review\n\n%s\n", d.Result.Summary.Take), limit)
	}
	return marker + out, notes
}

// RenderInline renders one finding as an inline review comment, falling
// back to the default template as RenderSummary does.
func RenderInline(ctx context.Context, t Templates, f Finding) (string, []string) {
	var notes []string
	if t.Inline != "" {
		out, err := render(ctx, t.Inline, findingContext(f), MaxRenderBytes)
		if err == nil {
			return out, nil
		}
		notes = append(notes, fallbackNote("inline", err))
	}
	out, err := render(context.WithoutCancel(ctx), defaultInline, findingContext(f), MaxRenderBytes)
	if err != nil {
		out = truncateUTF8(fmt.Sprintf("**[%s]** **%s**\n\n%s\n", f.Severity, f.Title, f.Explanation), MaxRenderBytes)
	}
	return out, notes
}

func fallbackNote(which string, err error) string {
	why := "failed to render"
	switch {
	case errors.Is(err, errTooLarge):
		why = fmt.Sprintf("produced more than %d KiB", MaxRenderBytes>>10)
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		why = "ran out of time"
	}
	return fmt.Sprintf("The repository's %s template %s, so kritik's default was used", which, why)
}

func summaryContext(d RenderData) map[string]any {
	praise := make([]any, len(d.Result.Summary.Praise))
	for i, p := range d.Result.Summary.Praise {
		praise[i] = p
	}
	findings := make([]any, len(d.Result.Findings))
	for i, f := range d.Result.Findings {
		findings[i] = findingContext(f)
	}
	notes := make([]any, len(d.Notes))
	for i, n := range d.Notes {
		notes[i] = n
	}
	return map[string]any{
		"number":    d.Number,
		"head_sha":  d.HeadSHA,
		"model":     d.Model,
		keySummary:  map[string]any{keyTake: d.Result.Summary.Take, keyPraise: praise},
		keyFindings: findings,
		"counts": map[string]any{
			"blocking": d.Counts.Blocking, "important": d.Counts.Important, "nit": d.Counts.Nit,
		},
		"notes":          notes,
		"incremental":    d.Incremental,
		"prior_head_sha": d.PriorHeadSHA,
		"incomplete":     d.Incomplete,
	}
}

func findingContext(f Finding) map[string]any {
	return map[string]any{
		keyPath:         f.Path,
		keyLine:         f.Line,
		keySeverity:     string(f.Severity),
		keyTitle:        f.Title,
		keyExplanation:  f.Explanation,
		keySuggestedFix: f.SuggestedFix,
	}
}

// render parses and executes src in a sandbox, bounded by ctx and
// renderTimeout. A render that outlives its deadline is abandoned; the
// guards make it stop at its next control structure, call or write.
func render(ctx context.Context, src string, data map[string]any, limit int) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, renderTimeout)
	defer cancel()
	type result struct {
		out string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				ch <- result{err: fmt.Errorf("%w: panic: %v", errSandbox, r)}
			}
		}()
		out, err := execute(ctx, src, data, limit)
		ch <- result{out, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil && ctx.Err() != nil {
			return "", fmt.Errorf("review: render: %w", ctx.Err())
		}
		return r.out, r.err
	case <-ctx.Done():
		return "", fmt.Errorf("review: render: %w", ctx.Err())
	}
}

func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}
