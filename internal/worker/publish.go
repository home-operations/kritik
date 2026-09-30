package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/forge"
	"github.com/home-operations/kritik/internal/model"
	"github.com/home-operations/kritik/internal/review"
	"github.com/home-operations/kritik/internal/store"
)

// CompleterSource resolves a provider name, the account's own or the
// file's, to its model adapter; each call wraps it in a model.Structured
// that records its steps.
type CompleterSource interface {
	Stepper(f *configfile.File, t *configfile.Account, name string) (model.Stepper, error)
}

// maxOutputTokens bounds one review answer. Findings are short by
// instruction; this is a guard against a runaway model, not a target.
const maxOutputTokens = 4096

// Usage roles, matching the usage table's CHECK.
const (
	roleReview    = "review"
	roleEmbedding = "embedding"
	roleFollowUp  = "followup"
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
	agent *agentRun
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

// capReached returns a description of the account's cap that is exhausted,
// or "".
func capReached(ctx context.Context, st *store.Store, accountID string, limits configfile.Limits) (string, error) {
	if limits.ReviewsPerDay <= 0 && limits.TokensPerMonth <= 0 {
		return "", nil
	}
	u, err := readUsage(ctx, st, accountID)
	if err != nil {
		return "", err
	}
	return reached(u, limits), nil
}

// readUsage is what an account's caps count: completed reviews today and
// tokens this month.
func readUsage(ctx context.Context, st *store.Store, accountID string) (store.MonthUsage, error) {
	var m store.MonthUsage
	err := st.WithAccount(ctx, accountID, func(tx pgx.Tx) error {
		var err error
		m, err = store.ReadMonthUsage(ctx, tx)
		return err
	})
	if err != nil {
		return store.MonthUsage{}, fmt.Errorf("worker: read caps: %w", err)
	}
	return m, nil
}

// usageRow is one model call charged to an account, and to the review it
// served when there is one: its whole prompt, cached part included, as
// input.
type usageRow struct {
	accountID, repositoryID, reviewID, role, model, upstream string
	input, output                                            int64
	costUSD                                                  float64
}

// insertUsage records u, where the caps count it.
func insertUsage(ctx context.Context, tx pgx.Tx, u usageRow) error {
	if _, err := tx.Exec(ctx, `INSERT INTO usage
		(account_id, repository_id, review_id, role, model, upstream, input_tokens, output_tokens, cost_usd)
		VALUES ($1, $2, nullif($3, '')::uuid, $4, $5, $6, $7, $8, $9)`,
		u.accountID, u.repositoryID, u.reviewID, u.role, u.model, u.upstream, u.input, u.output, u.costUSD); err != nil {
		return fmt.Errorf("worker: insert usage: %w", err)
	}
	return nil
}

// reached says which cap u has reached, or "".
func reached(u store.MonthUsage, limits configfile.Limits) string {
	if limits.ReviewsPerDay > 0 && u.ReviewsToday >= int64(limits.ReviewsPerDay) {
		return fmt.Sprintf("reviewsPerDay (%d) reached", limits.ReviewsPerDay)
	}
	if limits.TokensPerMonth > 0 && u.Tokens >= limits.TokensPerMonth {
		return fmt.Sprintf("tokensPerMonth (%d) reached", limits.TokensPerMonth)
	}
	return ""
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
func (p *publishPhase) writeBack(ctx context.Context, res review.Result, modelName string, notes []string) (int64, []inlineComment, error) {
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
		if onForge[i].posted || !p.postsInline(f) {
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
		sources = review.SourceLinks(p.agent.sources)
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
			onForge[i] = inlineComment{posted: true, id: ids[j]}
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

func (p *publishPhase) persist(ctx context.Context, res review.Result, inline []inlineComment, modelName string, commentID int64) error {
	return p.w.Store.WithAccount(ctx, p.account.ID(), func(tx pgx.Tx) error {
		for i, f := range res.Findings {
			if _, err := tx.Exec(ctx, `INSERT INTO findings
				(account_id, review_id, path, line, severity, title, explanation, suggested_fix, fingerprint, posted_inline,
				 end_line, replacement, agent_prompt, forge_comment_id, rules)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, nullif($14::bigint, 0), coalesce($15::text[], '{}'))`,
				p.account.ID(), p.reviewID, f.Path, f.Line, string(f.Severity), f.Title, f.Explanation, f.SuggestedFix,
				review.Fingerprint(f), inline[i].posted, f.EndLine, f.Replacement, f.AgentPrompt, inline[i].id, f.Rules); err != nil {
				return fmt.Errorf("worker: insert finding: %w", err)
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO sticky_comments (pull_request_id, account_id, forge_comment_id) VALUES ($1, $2, $3)
			ON CONFLICT (pull_request_id) DO UPDATE SET forge_comment_id = excluded.forge_comment_id, updated_at = now()`,
			p.pr.id, p.account.ID(), commentID); err != nil {
			return fmt.Errorf("worker: upsert sticky comment: %w", err)
		}
		summary, err := json.Marshal(res.Summary)
		if err != nil {
			return fmt.Errorf("worker: encode summary: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE reviews SET model = $2, summary = $3 WHERE id = $1`, p.reviewID, modelName, summary); err != nil {
			return fmt.Errorf("worker: record model: %w", err)
		}
		return nil
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
