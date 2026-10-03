package review

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/home-operations/kritika/internal/textcut"
)

// MaxRenderBytes bounds a rendered comment, marker included. GitHub rejects
// comment bodies over 65,536 characters, and a character is at least a byte.
const MaxRenderBytes = 64 << 10

// renderTimeout bounds one render, parse included.
const renderTimeout = 2 * time.Second

//go:embed templates/summary.md.tmpl
var defaultSummary string

//go:embed templates/inline.md.tmpl
var defaultInline string

// Templates are repository-supplied template sources. An empty field means
// the embedded default.
type Templates struct {
	Summary, Inline string
}

// RenderData is what the summary template sees as its dot; the inline
// template's dot is one Finding.
type RenderData struct {
	Number  int
	HeadSHA string
	// HeadURL links the head commit on the forge, "" when unknown.
	HeadURL string
	Model   string
	// AuthorIsBot is whether a bot opened the pull request; the default
	// template then leaves out the praise, which a mechanical change
	// earns nothing by.
	AuthorIsBot  bool
	Result       Result
	Counts       Counts
	Notes        []string
	Incremental  bool
	PriorHeadSHA string
	// PriorHeadURL links the last review's head on the forge, "" when
	// unknown.
	PriorHeadURL string
	// Prior are the last review's findings, each marked resolved or not,
	// when this review builds on it.
	Prior []PriorFinding
	// Unanchored are findings on lines the diff does not show, which
	// have no inline comment and are listed in the summary only.
	Unanchored []Finding
	// Incomplete, when set, says why the head was not fully reviewed; the
	// default template then states that instead of a verdict.
	Incomplete string
	// Sources are links to what the review's commands fetched, from
	// the runner's record rather than the model's answer, as SourceLinks
	// gives them.
	Sources []string
	// WebURL is the dashboard's origin, without a trailing slash, and
	// PullURL the pull request's page on it, where an admin can re-run
	// the review; both "" when the dashboard has no public URL. The
	// default template then leaves the re-run badge out.
	WebURL, PullURL string
}

// PullPageURL is the dashboard page of pull request number of owner/repo
// under the account name on forge, for a dashboard served at web: "" when
// web is nil.
func PullPageURL(web *url.URL, forge, name, owner, repo string, number int) string {
	if web == nil {
		return ""
	}
	return web.String() + "/#/a/" + url.PathEscape(forge) + "/" + url.PathEscape(name) +
		"/pulls/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/" + strconv.Itoa(number)
}

var (
	errTooLarge = errors.New("review: rendered output exceeds the size limit")
	errSandbox  = errors.New("review: template exceeded a sandbox limit")
)

// RenderSummary renders the sticky comment. kritika's marker always leads,
// whatever the template does, so sticky discovery cannot be defeated by a
// template. When the repository's template fails, the default is used and
// the returned note, also shown in the comment, says why.
func RenderSummary(ctx context.Context, t Templates, d RenderData) (body string, notes []string) {
	marker := Marker(d.Number) + "\n"
	limit := MaxRenderBytes - len(marker)
	if t.Summary != "" {
		out, err := render(ctx, t.Summary, d, limit)
		if err == nil {
			return marker + out, nil
		}
		note := fallbackNote("summary", err)
		notes = append(notes, note)
		d.Notes = append(slices.Clone(d.Notes), note)
	}
	out, err := render(context.WithoutCancel(ctx), defaultSummary, d, limit)
	if err != nil {
		// The default renders data kritika bounds itself; failing here is a
		// bug, but the comment must still carry the marker and the take.
		out = textcut.Prefix(fmt.Sprintf("## Kritika Review\n\n%s\n", d.Result.Summary.Take), limit)
	}
	return marker + out, notes
}

// RenderInline renders one finding as an inline review comment, led by its
// FindingMarker, falling back to the default template as RenderSummary
// does.
func RenderInline(ctx context.Context, t Templates, f Finding) (string, []string) {
	marker := FindingMarker(Fingerprint(f)) + "\n"
	limit := MaxRenderBytes - len(marker)
	var notes []string
	if t.Inline != "" {
		out, err := render(ctx, t.Inline, f, limit)
		if err == nil {
			return marker + out, nil
		}
		notes = append(notes, fallbackNote("inline", err))
	}
	out, err := render(context.WithoutCancel(ctx), defaultInline, f, limit)
	if err != nil {
		out = textcut.Prefix(fmt.Sprintf("**[%s]** **%s**\n\n%s\n", f.Severity, f.Title, f.Explanation), limit)
	}
	return marker + out, notes
}

func fallbackNote(which string, err error) string {
	why := "failed to render"
	switch {
	case errors.Is(err, errTooLarge):
		why = fmt.Sprintf("produced more than %d KiB", MaxRenderBytes>>10)
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		why = "ran out of time"
	}
	return fmt.Sprintf("The repository's %s template %s, so kritika's default was used", which, why)
}

// render parses and executes src in a sandbox, bounded by ctx and
// renderTimeout. A render that outlives its deadline is abandoned; the
// guards make it stop at its next loop, call or write.
func render(ctx context.Context, src string, data any, limit int) (string, error) {
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
