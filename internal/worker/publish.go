package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/forge"
	"github.com/home-operations/kritik/internal/repoconfig"
	"github.com/home-operations/kritik/internal/review"
	"github.com/home-operations/kritik/internal/runner"
	"github.com/home-operations/kritik/internal/store"
)

// publishPhase writes a prepared review's answer back to the forge and the
// database. It returns the final review status and, for a failure the
// caller should surface, the error.
type publishPhase struct {
	w        *Review
	account  *configfile.Account
	settings configfile.Settings
	client   forge.Client
	pr       *pullRequest
	reviewID string
	runID    string
	logger   *slog.Logger
	// parse and templates are the repository's contract settings; the zero
	// values are kritik's defaults.
	parse     review.ParseOptions
	templates review.Templates
	// repoNotes are what the summary states about the repository's
	// configuration files.
	repoNotes []string
	// prior is the last completed review, whose inline comments are not
	// posted again; scope says whether this review builds on it.
	prior priorReview
	scope review.Scope
	// agent is the review's agent run, whose usage the gateway recorded
	// step by step.
	agent *store.AgentRunRow
}

// run publishes what the runner's agent submitted; the run's usage is
// already recorded. An agent that stopped without submitting fails the
// review, and the sticky comment says this head was not fully reviewed so
// an earlier verdict does not stand in for it.
func (p *publishPhase) run(ctx context.Context) (store.ReviewStatus, error) {
	// The agent has already answered, so publishing runs to the end even if
	// the job's ctx ends meanwhile.
	ctx, cancel := detach(ctx)
	defer cancel()
	if p.agent == nil {
		return store.ReviewFailed, errors.New("worker: the runner wrote no agent run")
	}
	run := *p.agent
	var diff string
	err := p.w.Store.WithAccount(ctx, p.account.ID(), func(tx pgx.Tx) error {
		var err error
		diff, _, _, err = store.ContextPackDiffs(ctx, tx, p.runID)
		return err
	})
	if err != nil {
		return store.ReviewFailed, fmt.Errorf("worker: read context pack: %w", err)
	}
	p.logger.Info("agent answered", "stop", run.StopReason, "steps", run.Steps, "model", run.Model, "input_tokens", run.Usage.Prompt(),
		"cached_tokens", run.Usage.CacheRead, "output_tokens", run.Usage.Output, "cost_usd", run.CostUSD)
	if stopErr := stopError(run); stopErr != nil {
		return store.ReviewFailed, errors.Join(stopErr, p.incomplete(ctx, "agent stopped: "+run.StopReason, run.Model))
	}
	res, dropped, err := review.Parse(string(run.Result), review.Anchors(diff), p.parse)
	if err != nil {
		return store.ReviewFailed, errors.Join(err, p.incomplete(ctx, "the submitted review was invalid", run.Model))
	}
	for _, d := range dropped {
		p.logger.Debug("finding dropped", "reason", d.Reason, "path", d.Finding.Path, "line", d.Finding.Line, "title", d.Finding.Title)
	}
	commentID, inline, err := p.writeBack(ctx, res, run.Model, append(reviewNotes(dropped), p.repoNotes...))
	if err != nil {
		return store.ReviewFailed, err
	}
	p.countFindings(res)
	if err := p.persist(ctx, res, inline, run.Model, commentID); err != nil {
		return store.ReviewFailed, err
	}
	return store.ReviewCompleted, nil
}

// skipDescription is how the commit status states a skip the runner
// decided: a repository's own reason, or a bot's unchanged patch.
func skipDescription(reason string) string {
	if reason == runner.SkipUnchangedPatch {
		return "patch unchanged since the last review"
	}
	return repoconfig.SkipReason(reason).Description()
}

// incomplete replaces the sticky comment with one saying why this head was
// not fully reviewed, in kritik's own template, and records the model.
func (p *publishPhase) incomplete(ctx context.Context, reason, modelName string) error {
	body, _ := review.RenderSummary(ctx, review.Templates{}, review.RenderData{
		Number: p.pr.number, HeadSHA: p.pr.headSHA, Model: modelName, Incomplete: reason, Notes: p.repoNotes,
	})
	commentID, err := p.upsertSticky(ctx, body)
	if err != nil {
		return err
	}
	owner, repo := p.pr.ownerRepo()
	if err := p.client.SetStatus(ctx, owner, repo, p.pr.headSHA, forge.StatusSuccess, "kritik: review incomplete ("+reason+")"); err != nil {
		p.logger.Warn("commit status not set", "error", err)
	}
	return p.persist(ctx, review.Result{}, nil, modelName, commentID)
}

func (p *publishPhase) countFindings(res review.Result) {
	bySeverity := map[string]int{}
	for _, f := range res.Findings {
		bySeverity[string(f.Severity)]++
	}
	for severity, n := range bySeverity {
		p.w.Metrics.Findings(p.account.Key(), severity, n)
	}
}

// reviewNotes are the caveats the sticky comment states about a review.
func reviewNotes(dropped []review.Dropped) []string {
	var notes []string
	if len(dropped) > 0 {
		byReason := map[review.DropReason]int{}
		for _, d := range dropped {
			byReason[d.Reason]++
		}
		reasons := make([]string, 0, len(byReason))
		for r, n := range byReason {
			reasons = append(reasons, fmt.Sprintf("%s: %d", r, n))
		}
		slices.Sort(reasons)
		notes = append(notes, fmt.Sprintf("%d finding(s) were dropped (%s)", len(dropped), strings.Join(reasons, ", ")))
	}
	return notes
}

// writeBack posts the sticky comment (created once, edited after), the
// inline review, and the commit status. Only the sticky comment is
// required: the other two are best effort and logged when they fail, so a
// forge quirk cannot turn a finished review into a retry storm. A finding
// the last review already posted inline, or one the settings keep out of
// inline comments, is listed in the summary only. The returned comments
// say, per finding, whether an inline comment for it is on the forge, and
// its id there.
func (p *publishPhase) writeBack(
	ctx context.Context, res review.Result, modelName string, notes []string,
) (int64, []store.InlinePosted, error) {
	owner, repo := p.pr.ownerRepo()
	onForge := alreadyInline(res.Findings, p.prior.findings)
	for i := range res.Findings {
		f := &res.Findings[i]
		f.URL = p.client.FileURL(owner, repo, p.pr.headSHA, f.Path, f.Line, f.EndLine)
	}
	// Inline comments render first so a failing inline template is noted
	// in the summary. After one failure the rest use the default, so a
	// template that times out costs one deadline, not one per finding.
	templates := p.templates
	inline := make([]forge.InlineComment, 0, len(res.Findings))
	var posted []int
	for i, f := range res.Findings {
		if onForge[i].Posted || !p.postsInline(f) {
			continue
		}
		body, inlineNotes := review.RenderInline(ctx, templates, f)
		if len(inlineNotes) > 0 {
			templates.Inline = ""
			notes = append(notes, inlineNotes...)
		}
		c := forge.InlineComment{Path: f.Path, Line: f.Line, Body: body}
		if f.EndLine > 0 {
			c.StartLine, c.Line = f.Line, f.EndLine
		}
		inline = append(inline, c)
		posted = append(posted, i)
	}
	var sources []string
	if p.agent != nil {
		sources = review.SourceLinks(p.agent.Sources)
	}
	body, renderNotes := review.RenderSummary(ctx, p.templates, review.RenderData{
		Number: p.pr.number, HeadSHA: p.pr.headSHA, Model: modelName, Result: res, Counts: res.Counts(), Notes: notes,
		Incremental: p.scope == review.ScopeIncremental, PriorHeadSHA: p.prior.headSHA, Sources: sources,
	})
	for _, n := range renderNotes {
		p.logger.Warn("template fell back to the default", "note", n)
	}

	commentID, err := p.upsertSticky(ctx, body)
	if err != nil {
		return 0, nil, err
	}

	ids, err := p.client.CreateReview(ctx, owner, repo, p.pr.number, p.pr.headSHA, inline)
	switch {
	case ids == nil:
		p.logger.Warn("inline review not posted", "error", err)
	case err != nil:
		p.logger.Warn("inline review posted, but its comments' ids not read back", "error", err)
	}
	for j, i := range posted {
		if ids != nil {
			onForge[i] = store.InlinePosted{Posted: true, ID: ids[j]}
		}
	}
	desc := "no findings"
	if n := len(res.Findings); n > 0 {
		desc = fmt.Sprintf("%d finding(s)", n)
	}
	if err := p.client.SetStatus(ctx, owner, repo, p.pr.headSHA, forge.StatusSuccess, "kritik: "+desc); err != nil {
		p.logger.Warn("commit status not set", "error", err)
	}
	if p.settings.Review.Approve {
		p.approve(ctx, res.Counts())
	}
	return commentID, onForge, nil
}

// approve approves the head when the review found nothing blocking or
// important, and otherwise dismisses the approval an earlier review gave,
// so an approval never outlives the verdict behind it. Both are best
// effort, like the commit status: the review is published either way.
func (p *publishPhase) approve(ctx context.Context, counts review.Counts) {
	owner, repo := p.pr.ownerRepo()
	if counts.Approvable() {
		posted, err := p.client.Approve(ctx, owner, repo, p.pr.number, p.pr.headSHA,
			fmt.Sprintf("kritik: nothing blocking or important found at %s.", review.ShortSHA(p.pr.headSHA)))
		if err != nil {
			p.logger.Warn("pull request not approved", "error", err)
		} else if posted {
			p.logger.Info("pull request approved")
		}
		return
	}
	n, err := p.client.DismissApprovals(ctx, owner, repo, p.pr.number,
		fmt.Sprintf("kritik: %d blocking and %d important finding(s) at %s.", counts.Blocking, counts.Important, review.ShortSHA(p.pr.headSHA)))
	if err != nil {
		p.logger.Warn("approval not dismissed", "error", err)
	} else if n > 0 {
		p.logger.Info("approval dismissed", "reviews", n)
	}
}

// postsInline reports whether the review's settings post f as an inline
// comment: none when inline comments are off, and otherwise every finding
// but a nit that the feedback level keeps to the summary.
func (p *publishPhase) postsInline(f review.Finding) bool {
	r := p.settings.Review
	return r.InlineComments && (f.Severity != review.SeverityNit || r.NitsInline())
}

// upsertSticky edits the pull request's sticky comment to body, creating
// it the first time, and returns its id.
func (p *publishPhase) upsertSticky(ctx context.Context, body string) (int64, error) {
	owner, repo := p.pr.ownerRepo()
	login, err := p.client.BotLogin(ctx)
	if err != nil {
		return 0, err
	}
	var commentID int64
	// The row is a shortcut: no row, or a read that failed, leaves the
	// comment to be found on the forge by its marker.
	_ = p.w.Store.WithAccount(ctx, p.account.ID(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT forge_comment_id FROM sticky_comments WHERE pull_request_id = $1`, p.pr.id).Scan(&commentID)
	})
	if commentID == 0 {
		if commentID, err = p.client.FindComment(ctx, owner, repo, p.pr.number, login, review.Marker(p.pr.number)); err != nil {
			return 0, err
		}
	}
	if commentID != 0 {
		err = p.client.UpdateComment(ctx, owner, repo, commentID, body)
	} else {
		commentID, err = p.client.CreateComment(ctx, owner, repo, p.pr.number, body)
	}
	if err != nil {
		return 0, err
	}
	return commentID, nil
}

func (p *publishPhase) persist(
	ctx context.Context, res review.Result, inline []store.InlinePosted, modelName string, commentID int64,
) error {
	return p.w.Store.WithAccount(ctx, p.account.ID(), func(tx pgx.Tx) error {
		return store.RecordReviewResult(ctx, tx, store.ReviewResult{
			AccountID: p.account.ID(), ReviewID: p.reviewID, PullRequestID: p.pr.id, Result: res, Inline: inline, Model: modelName,
			CommentID: commentID,
		})
	})
}

func callOutcome(err error) string {
	if err != nil {
		return "error"
	}
	return "ok"
}

// servedRef is the model that answered a call made to ref, as a model
// reference, for metrics: OpenRouter's server-side fallback may answer with
// a model other than the one asked for, and served names it, "" for none.
func servedRef(ref configfile.ModelRef, served string) string {
	if served == "" {
		return string(ref)
	}
	return ref.Provider() + "/" + served
}
