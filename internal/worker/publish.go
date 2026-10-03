package worker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritika/internal/configfile"
	"github.com/home-operations/kritika/internal/forge"
	"github.com/home-operations/kritika/internal/jobs"
	"github.com/home-operations/kritika/internal/repoconfig"
	"github.com/home-operations/kritika/internal/review"
	"github.com/home-operations/kritika/internal/runner"
	"github.com/home-operations/kritika/internal/store"
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
	// trigger is why the review ran; one a human asked for does not
	// count towards the pull request's automatic reviews.
	trigger string
	logger  *slog.Logger
	// parse and templates are the repository's contract settings; the zero
	// values are kritika's defaults.
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
	unanchored, notes := splitDropped(dropped)
	var dismissed int
	if res.Findings, dismissed = dropDismissed(res.Findings, p.prior.dismissed); dismissed > 0 {
		notes = append(notes, fmt.Sprintf("%d finding(s) a maintainer dismissed were left out", dismissed))
	}
	if note, err := p.countAutoReview(ctx); err != nil {
		return store.ReviewFailed, err
	} else if note != "" {
		notes = append(notes, note)
	}
	commentID, inline, err := p.writeBack(ctx, res, unanchored, run.Model, append(notes, p.repoNotes...))
	if err != nil {
		return store.ReviewFailed, err
	}
	p.countFindings(res)
	if err := p.persist(ctx, res, inline, run.Model, commentID); err != nil {
		return store.ReviewFailed, err
	}
	return store.ReviewCompleted, nil
}

// countAutoReview counts this review towards the pull request's automatic
// reviews, unless a human asked for it, and returns the note the summary
// states when it was the last the repository allows.
func (p *publishPhase) countAutoReview(ctx context.Context) (string, error) {
	if p.trigger == jobs.TriggerManual {
		return "", nil
	}
	var pausedNow bool
	err := p.w.Store.WithAccount(ctx, p.account.ID(), func(tx pgx.Tx) error {
		var err error
		pausedNow, err = store.CountAutoReview(ctx, tx, p.pr.id, p.settings.MaxAutoReviews)
		return err
	})
	if err != nil || !pausedNow {
		return "", err
	}
	login, err := p.client.BotLogin(ctx)
	if err != nil {
		return "", err
	}
	p.logger.Info("automatic reviews paused", "after", p.settings.MaxAutoReviews)
	return review.AutoPausedNote(strings.TrimSuffix(login, "[bot]"), p.settings.MaxAutoReviews), nil
}

// skipDescription is how the commit status states a skip the runner
// decided: a repository's own reason, a bot's unchanged patch, or a diff
// over the repository's maxChangedLines.
func skipDescription(reason string, maxChangedLines int) string {
	switch reason {
	case runner.SkipUnchangedPatch:
		return "patch unchanged since the last review"
	case runner.SkipTooLarge:
		return fmt.Sprintf("more than %d changed lines", maxChangedLines)
	}
	return repoconfig.SkipReason(reason).Description()
}

// incomplete replaces the sticky comment with one saying why this head was
// not fully reviewed, in kritika's own template, and records the model.
func (p *publishPhase) incomplete(ctx context.Context, reason, modelName string) error {
	owner, repo := p.pr.ownerRepo()
	body, _ := review.RenderSummary(ctx, review.Templates{}, review.RenderData{
		Number: p.pr.number, HeadSHA: p.pr.headSHA, HeadURL: p.client.CommitURL(owner, repo, p.pr.headSHA), Model: modelName,
		Incomplete: reason, Notes: p.repoNotes,
	})
	commentID, err := p.upsertSticky(ctx, body)
	if err != nil {
		return err
	}
	if err := p.client.SetStatus(ctx, owner, repo, p.pr.headSHA, forge.StatusError, "kritika: review incomplete ("+reason+")"); err != nil {
		p.logger.Warn("commit status not set", "error", err)
	}
	return p.persist(ctx, review.Result{}, nil, modelName, commentID)
}

func (p *publishPhase) countFindings(res review.Result) {
	byKind := map[[2]string]int{}
	for _, f := range res.Findings {
		byKind[[2]string{string(f.Severity), string(f.Category)}]++
	}
	for kind, n := range byKind {
		p.w.Metrics.Findings(p.account.Key(), kind[0], kind[1], n)
	}
}

// splitDropped sorts what Parse dropped into the findings on lines the
// diff does not show, which the summary lists since no inline comment can
// carry them, and a note counting the rest.
func splitDropped(dropped []review.Dropped) (unanchored []review.Finding, notes []string) {
	byReason := map[review.DropReason]int{}
	total := 0
	for _, d := range dropped {
		if d.Reason == review.DropUnanchored {
			unanchored = append(unanchored, d.Finding)
			continue
		}
		byReason[d.Reason]++
		total++
	}
	if total == 0 {
		return unanchored, nil
	}
	reasons := make([]string, 0, len(byReason))
	for r, n := range byReason {
		reasons = append(reasons, fmt.Sprintf("%s: %d", r, n))
	}
	slices.Sort(reasons)
	return unanchored, []string{fmt.Sprintf("%d finding(s) were dropped (%s)", total, strings.Join(reasons, ", "))}
}

// writeBack posts the sticky comment (created once, edited after), the
// inline review, and the commit status. Only the sticky comment is
// required: the other two are best effort and logged when they fail, so a
// forge quirk cannot turn a finished review into a retry storm. A finding
// the last review already posted inline, or one the settings keep out of
// inline comments, is listed in the summary only. Once the inline review
// is posted, the sticky comment is edited again to link each finding to
// its thread. The returned comments say, per finding, whether an inline
// comment for it is on the forge, and its id there.
func (p *publishPhase) writeBack(
	ctx context.Context, res review.Result, unanchored []review.Finding, modelName string, notes []string,
) (int64, []store.InlinePosted, error) {
	owner, repo := p.pr.ownerRepo()
	onForge := alreadyInline(res.Findings, p.prior.findings)
	if p.settings.Review.InlineComments && len(res.Findings) > 0 {
		p.markOnForge(ctx, res.Findings, onForge)
	}
	for i := range res.Findings {
		f := &res.Findings[i]
		f.URL = p.client.FileURL(owner, repo, p.pr.headSHA, f.Path, f.Line, f.EndLine)
		if onForge[i].ID != 0 {
			f.ThreadURL = p.client.ThreadURL(owner, repo, p.pr.number, onForge[i].ID)
		}
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
	data := review.RenderData{
		Number: p.pr.number, HeadSHA: p.pr.headSHA, HeadURL: p.client.CommitURL(owner, repo, p.pr.headSHA), Model: modelName,
		AuthorIsBot: p.pr.authorIsBot, Result: res, Counts: res.Counts(), Notes: notes, Unanchored: unanchored,
		Incremental: p.scope == review.ScopeIncremental, PriorHeadSHA: p.prior.headSHA, Sources: sources,
	}
	if data.Incremental {
		data.PriorHeadURL = p.client.CommitURL(owner, repo, p.prior.headSHA)
		data.Prior = p.priorFindings(res)
		p.resolveThreads(ctx, resolvedThreads(res, p.prior.findings))
	}
	body, renderNotes := review.RenderSummary(ctx, p.templates, data)
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
	linked := false
	for j, i := range posted {
		if ids != nil {
			onForge[i] = store.InlinePosted{Posted: true, ID: ids[j]}
		}
		if ids != nil && ids[j] != 0 {
			res.Findings[i].ThreadURL = p.client.ThreadURL(owner, repo, p.pr.number, ids[j])
			linked = true
		}
	}
	if linked {
		// The threads exist only now, so the summary is written a second
		// time with the links; losing them is not worth failing the review.
		data.Result = res
		body, _ = review.RenderSummary(ctx, p.templates, data)
		if err := p.client.UpdateComment(ctx, owner, repo, commentID, body); err != nil {
			p.logger.Warn("summary not linked to its inline comments", "error", err)
		}
	}
	desc := "no findings"
	if n := len(res.Findings); n > 0 {
		desc = fmt.Sprintf("%d finding(s)", n)
	}
	if err := p.client.SetStatus(ctx, owner, repo, p.pr.headSHA, forge.StatusSuccess, "kritika: "+desc); err != nil {
		p.logger.Warn("commit status not set", "error", err)
	}
	if p.settings.Review.Approve {
		p.approve(ctx, res.Counts())
	}
	return commentID, onForge, nil
}

// markOnForge marks the findings whose inline comment the bot has already
// posted on the pull request, by the FindingMarker each carries, which the
// database may not know: an attempt that posted and then died before
// recording them, or a review whose record failed to write. A forge that
// cannot be read leaves the database's answer to stand.
func (p *publishPhase) markOnForge(ctx context.Context, findings []review.Finding, onForge []store.InlinePosted) {
	owner, repo := p.pr.ownerRepo()
	login, err := p.client.BotLogin(ctx)
	if err != nil {
		p.logger.Warn("inline comments on the forge not checked", "error", err)
		return
	}
	comments, err := p.client.ListInline(ctx, owner, repo, p.pr.number)
	if err != nil {
		p.logger.Warn("inline comments on the forge not checked", "error", err)
		return
	}
	posted := markedInline(comments, login)
	for i, f := range findings {
		if id, ok := posted[review.Fingerprint(f)]; ok && !onForge[i].Posted {
			onForge[i] = store.InlinePosted{Posted: true, ID: id}
		}
	}
}

// markedInline maps the fingerprint each of the bot's root inline comments
// carries a FindingMarker for to the comment's id; a later comment for the
// same finding wins, as the one a reader sees as current.
func markedInline(comments []forge.Comment, login string) map[string]int64 {
	posted := map[string]int64{}
	for _, c := range comments {
		if c.InReplyTo != 0 || !strings.EqualFold(c.Author, login) {
			continue
		}
		if fp, ok := review.MarkedFinding(c.Body); ok {
			posted[fp] = c.ID
		}
	}
	return posted
}

// priorFindings is the last review's findings as this review's summary
// lists them: resolved when the model, asked to report each again only if
// still present, did not, and linked to their threads where they have one,
// then the findings maintainers dismissed, each linked to the comment that
// dismissed it.
func (p *publishPhase) priorFindings(res review.Result) []review.PriorFinding {
	reported := make(map[string]bool, len(res.Findings))
	for _, f := range res.Findings {
		reported[review.Fingerprint(f)] = true
	}
	owner, repo := p.pr.ownerRepo()
	out := make([]review.PriorFinding, 0, len(p.prior.findings))
	for _, pf := range p.prior.findings {
		f := pf.Finding
		f.URL = p.client.FileURL(owner, repo, p.prior.headSHA, f.Path, f.Line, f.EndLine)
		if pf.commentID != 0 {
			f.ThreadURL = p.client.ThreadURL(owner, repo, p.pr.number, pf.commentID)
		}
		out = append(out, review.PriorFinding{Finding: f, Resolved: !reported[review.Fingerprint(f)]})
	}
	for _, d := range p.prior.dismissed {
		f := d.Finding
		f.URL = p.client.FileURL(owner, repo, p.prior.headSHA, f.Path, f.Line, f.EndLine)
		f.ThreadURL = p.client.ThreadURL(owner, repo, p.pr.number, d.CommentID)
		out = append(out, review.PriorFinding{Finding: f, Dismissed: true, DismissReason: d.Reason})
	}
	return out
}

// resolvedThreads is the inline comment id of each of the last review's
// findings this review, asked to report each again only if still present,
// did not report: the threads the summary lists as resolved.
func resolvedThreads(res review.Result, prior []priorFinding) []int64 {
	reported := make(map[string]bool, len(res.Findings))
	for _, f := range res.Findings {
		reported[review.Fingerprint(f)] = true
	}
	var ids []int64
	for _, pf := range prior {
		if pf.commentID != 0 && !reported[review.Fingerprint(pf.Finding)] {
			ids = append(ids, pf.commentID)
		}
	}
	return ids
}

// resolveThreads resolves the threads of the findings this review found
// gone, so the forge agrees with the summary that lists them resolved. The
// forge client leaves a thread someone else wrote in open. Best effort,
// like the inline review: the summary is the record either way.
func (p *publishPhase) resolveThreads(ctx context.Context, ids []int64) {
	owner, repo := p.pr.ownerRepo()
	for _, id := range ids {
		resolved, err := p.client.ResolveThread(ctx, owner, repo, p.pr.number, id, true)
		switch {
		case err != nil:
			p.logger.Warn("finding's thread not resolved", "comment", id, "error", err)
		case resolved:
			p.logger.Info("finding's thread resolved", "comment", id)
		}
	}
}

// approve approves the head when the review found nothing blocking or
// important, and otherwise dismisses the approval an earlier review gave,
// so an approval never outlives the verdict behind it. Both are best
// effort, like the commit status: the review is published either way.
func (p *publishPhase) approve(ctx context.Context, counts review.Counts) {
	owner, repo := p.pr.ownerRepo()
	if counts.Approvable() {
		posted, err := p.client.Approve(ctx, owner, repo, p.pr.number, p.pr.headSHA,
			fmt.Sprintf("kritika: nothing blocking or important found at %s.", review.ShortSHA(p.pr.headSHA)))
		if err != nil {
			p.logger.Warn("pull request not approved", "error", err)
		} else if posted {
			p.logger.Info("pull request approved")
		}
		return
	}
	n, err := p.client.DismissApprovals(ctx, owner, repo, p.pr.number,
		fmt.Sprintf("kritika: %d blocking and %d important finding(s) at %s.", counts.Blocking, counts.Important, review.ShortSHA(p.pr.headSHA)))
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
	var stored int64
	// The row is a shortcut: no row, or a read that failed, leaves the
	// comment to be found on the forge by its marker.
	_ = p.w.Store.WithAccount(ctx, p.account.ID(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT forge_comment_id FROM sticky_comments WHERE pull_request_id = $1`, p.pr.id).Scan(&stored)
	})
	return p.writeSticky(ctx, stored, body)
}

// writeSticky edits the sticky comment stored, the one the database
// remembers, to body, and returns its id. With none stored, or one the
// forge no longer has, the comment is found by its marker instead and
// created when there is none: a sticky comment someone deleted must not
// fail every later review, and the id returned replaces the stored one.
func (p *publishPhase) writeSticky(ctx context.Context, stored int64, body string) (int64, error) {
	owner, repo := p.pr.ownerRepo()
	if stored != 0 {
		err := p.client.UpdateComment(ctx, owner, repo, stored, body)
		if err == nil {
			return stored, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return 0, err
		}
		p.logger.Warn("sticky comment no longer on the forge", "comment", stored)
	}
	login, err := p.client.BotLogin(ctx)
	if err != nil {
		return 0, err
	}
	commentID, err := p.client.FindComment(ctx, owner, repo, p.pr.number, login, review.Marker(p.pr.number))
	if err != nil {
		return 0, err
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
