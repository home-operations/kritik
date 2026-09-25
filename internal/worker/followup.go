package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/contextpack"
	"github.com/home-operations/kritik/internal/forge"
	"github.com/home-operations/kritik/internal/jobs"
	"github.com/home-operations/kritik/internal/metrics"
	"github.com/home-operations/kritik/internal/model"
	"github.com/home-operations/kritik/internal/review"
	"github.com/home-operations/kritik/internal/store"
)

// Follow-up bounds: mentions answered per pull request per hour before a
// single "limit reached" reply, and thread messages kept in the prompt.
const (
	followUpsPerHour = 5
	threadMessages   = 20
)

// Follow-up outcomes, as the followups table spells them.
const (
	followUpAnswered = "answered"
	followUpLimited  = "limited"
	followUpIgnored  = "ignored"
	followUpFailed   = "failed"
)

// FollowUp works the followup queue: one job answers one comment that
// @-mentioned the bot, scoped to its thread.
type FollowUp struct {
	river.WorkerDefaults[jobs.FollowUpArgs]
	Store      *store.Store
	Current    *configfile.Current
	Forges     Forges
	Completers CompleterSource
	Logger     *slog.Logger
	// Metrics may be nil.
	Metrics *metrics.Metrics
}

type followUpPR struct {
	id, repositoryID, installation, repository, title, author, baseRef string
	externalID                                                         int64
	number                                                             int
}

// Work implements river.Worker.
func (w *FollowUp) Work(ctx context.Context, job *river.Job[jobs.FollowUpArgs]) error {
	args := job.Args
	file := w.Current.Get()
	tenant := tenantByID(file, args.TenantID)
	if tenant == nil {
		return river.JobCancel(fmt.Errorf("worker: tenant %s is not in the configuration", args.TenantID))
	}
	logger := w.Logger.With("tenant", tenant.Slug, "pr", args.Number, "comment", args.CommentID)
	pr, err := w.loadPR(ctx, args)
	if err != nil {
		return err
	}
	pr.number = args.Number
	in, _, ok := file.Installation(pr.installation)
	if !ok {
		return river.JobCancel(fmt.Errorf("worker: installation %s is not in the configuration", pr.installation))
	}
	client, err := w.Forges.For(ctx, in, pr.externalID, pr.repository)
	if err != nil {
		return err
	}
	owner, repo, _ := strings.Cut(pr.repository, "/")
	comment, err := client.GetComment(ctx, owner, repo, args.CommentID, args.Inline)
	if err != nil {
		return err
	}
	login, err := client.BotLogin(ctx)
	if err != nil {
		return err
	}
	f := &followUp{w: w, file: file, tenant: tenant, settings: file.Settings(tenant, pr.repository), client: client, pr: pr,
		comment: comment, owner: owner, repo: repo, botLogin: login, jobID: job.ID, logger: logger}
	if done, err := f.alreadyAnswered(ctx); err != nil || done {
		return err
	}
	outcome, err := f.run(ctx)
	w.Metrics.FollowUp(tenant.Slug, outcome)
	if err != nil {
		logger.Error("follow-up failed", "error", err)
		_ = f.record(ctx, followUpFailed, err.Error(), 0, "")
		return err
	}
	logger.Info("follow-up " + outcome)
	return nil
}

func (w *FollowUp) loadPR(ctx context.Context, args jobs.FollowUpArgs) (*followUpPR, error) {
	var pr followUpPR
	err := w.Store.WithTenant(ctx, args.TenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT p.id, p.repository_id, i.name, r.name, p.title, p.author, p.base_ref, coalesce(i.external_id, 0)
			FROM pull_requests p JOIN repositories r ON r.id = p.repository_id JOIN installations i ON i.id = r.installation_id
			WHERE p.repository_id = $1 AND p.number = $2`, args.RepositoryID, args.Number).
			Scan(&pr.id, &pr.repositoryID, &pr.installation, &pr.repository, &pr.title, &pr.author, &pr.baseRef, &pr.externalID)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, river.JobCancel(fmt.Errorf("worker: pull request %d of %s is unknown", args.Number, args.RepositoryID))
	}
	if err != nil {
		return nil, fmt.Errorf("worker: load pull request: %w", err)
	}
	return &pr, nil
}

type followUp struct {
	w        *FollowUp
	file     *configfile.File
	tenant   *configfile.Tenant
	settings configfile.Settings
	client   forge.Client
	pr       *followUpPR
	comment  forge.Comment
	owner    string
	repo     string
	botLogin string
	jobID    int64
	logger   *slog.Logger
}

// alreadyAnswered guards a retried job: once a reply is on the forge the
// mention is done, whatever happened after posting.
func (f *followUp) alreadyAnswered(ctx context.Context) (bool, error) {
	var status string
	var replyID *int64
	err := f.w.Store.WithTenant(ctx, f.tenant.ID(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status, reply_comment_id FROM followups WHERE pull_request_id = $1 AND comment_id = $2`,
			f.pr.id, f.comment.ID).Scan(&status, &replyID)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("worker: read follow-up: %w", err)
	}
	if status == followUpAnswered || status == followUpLimited || replyID != nil {
		f.logger.Info("follow-up already handled", "status", status)
		return true, nil
	}
	return false, nil
}

// run qualifies the mention (§2.7), gathers the thread and the review's
// record, asks the model, and posts the reply. Nothing after the reply is
// posted may fail the job: a retry would answer twice.
func (f *followUp) run(ctx context.Context) (string, error) {
	if reason := f.disqualified(ctx); reason != "" {
		f.logger.Info("follow-up ignored", "reason", reason)
		return followUpIgnored, f.record(ctx, followUpIgnored, reason, 0, "")
	}
	limited, err := f.rateLimited(ctx)
	if err != nil {
		return followUpFailed, err
	}
	if limited {
		return followUpLimited, nil
	}
	thread, root, err := f.thread(ctx)
	if err != nil {
		return followUpFailed, err
	}
	rec, err := f.reviewRecord(ctx)
	if err != nil {
		return followUpFailed, err
	}
	msg := review.BuildFollowUp(review.Input{
		Repository: f.pr.repository, Number: f.pr.number, Title: f.pr.title, Author: f.pr.author, BaseRef: f.pr.baseRef,
		Changed: rec.changed, Diff: rec.diff, Context: rec.context,
	}, rec.findings, thread)
	resp, err := f.complete(ctx, msg)
	if err != nil {
		return followUpFailed, err
	}
	reply, err := review.ParseFollowUp(resp.Raw)
	if err != nil {
		return followUpFailed, err
	}
	body := review.FollowUpBody(reply, resp.Model)
	var replyID int64
	if f.comment.Inline {
		replyID, err = f.client.ReplyInline(ctx, f.owner, f.repo, f.number(), root, body)
	} else {
		replyID, err = f.client.CreateComment(ctx, f.owner, f.repo, f.number(), body)
	}
	if err != nil {
		return followUpFailed, err
	}
	f.logger.Info("follow-up answered", "model", resp.Model, "reply", replyID, "input_tokens", resp.InputTokens,
		"output_tokens", resp.OutputTokens, "cost_usd", resp.CostUSD)
	err = f.w.Store.WithTenant(ctx, f.tenant.ID(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO usage (tenant_id, repository_id, role, model, upstream, input_tokens, output_tokens, cost_usd)
			VALUES ($1, $2, 'followup', $3, $4, $5, $6, $7)`,
			f.tenant.ID(), f.pr.repositoryID, resp.Model, resp.Upstream, resp.InputTokens, resp.OutputTokens, resp.CostUSD); err != nil {
			return fmt.Errorf("worker: record follow-up usage: %w", err)
		}
		return nil
	})
	if err != nil {
		f.logger.Error("follow-up usage not recorded", "error", err)
	}
	if err := f.record(ctx, followUpAnswered, "", replyID, resp.Model); err != nil {
		f.logger.Error("follow-up not recorded", "error", err, "reply", replyID)
	}
	return followUpAnswered, nil
}

func (f *followUp) number() int { return f.pr.number }

var mentionPattern = regexp.MustCompile(`(?i)(^|[^\w@])@([\w-]+)`)

// disqualified returns why the mention is not answered, or "".
func (f *followUp) disqualified(ctx context.Context) string {
	if f.comment.AuthorIsBot || strings.EqualFold(f.comment.Author, f.botLogin) {
		return "author is a bot"
	}
	slug := strings.TrimSuffix(f.botLogin, "[bot]")
	mentioned := false
	for _, m := range mentionPattern.FindAllStringSubmatch(f.comment.Body, -1) {
		if strings.EqualFold(m[2], slug) {
			mentioned = true
			break
		}
	}
	if !mentioned {
		return "does not mention @" + slug
	}
	perm, err := f.client.Permission(ctx, f.owner, f.repo, f.comment.Author)
	if err != nil {
		f.logger.Warn("permission lookup failed", "error", err)
		return "permission unknown"
	}
	if !forge.CanWrite(perm) {
		return "author has " + string(perm) + " access, write is required"
	}
	return ""
}

// rateLimited posts the limit notice once per hour when the pull request
// has had its share of answers, and records it.
func (f *followUp) rateLimited(ctx context.Context) (bool, error) {
	var answered, notices int
	err := f.w.Store.WithTenant(ctx, f.tenant.ID(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status = 'answered'), count(*) FILTER (WHERE status = 'limited')
			FROM followups WHERE pull_request_id = $1 AND created_at > now() - interval '1 hour'`, f.pr.id).Scan(&answered, &notices)
	})
	if err != nil {
		return false, fmt.Errorf("worker: count follow-ups: %w", err)
	}
	if answered < followUpsPerHour {
		return false, nil
	}
	var replyID int64
	if notices == 0 {
		if replyID, err = f.client.CreateComment(ctx, f.owner, f.repo, f.number(), review.LimitBody); err != nil {
			return true, err
		}
	}
	f.logger.Info("follow-up rate limited", "answered_last_hour", answered, "notice_posted", notices == 0)
	return true, f.record(ctx, followUpLimited, "", replyID, "")
}

// thread returns the messages the model sees and, for an inline comment,
// the root comment replies attach to. The asking comment is always last.
func (f *followUp) thread(ctx context.Context) ([]review.Message, int64, error) {
	var comments []forge.Comment
	var err error
	root := f.comment.ID
	if f.comment.Inline {
		if f.comment.InReplyTo != 0 {
			root = f.comment.InReplyTo
		}
		all, err := f.client.ListInline(ctx, f.owner, f.repo, f.number())
		if err != nil {
			return nil, 0, err
		}
		for _, c := range all {
			if c.ID == root || c.InReplyTo == root {
				comments = append(comments, c)
			}
		}
	} else {
		if comments, err = f.client.ListConversation(ctx, f.owner, f.repo, f.number()); err != nil {
			return nil, 0, err
		}
	}
	found := false
	for _, c := range comments {
		if c.ID == f.comment.ID {
			found = true
		}
	}
	if !found {
		comments = append(comments, f.comment)
	}
	sort.SliceStable(comments, func(i, j int) bool { return comments[i].CreatedAt.Before(comments[j].CreatedAt) })
	// The asking comment closes the thread whatever the timestamps say.
	for i, c := range comments {
		if c.ID == f.comment.ID && i != len(comments)-1 {
			comments = append(append(comments[:i:i], comments[i+1:]...), c)
			break
		}
	}
	if len(comments) > threadMessages {
		comments = comments[len(comments)-threadMessages:]
	}
	msgs := make([]review.Message, 0, len(comments))
	for _, c := range comments {
		msgs = append(msgs, review.Message{Author: c.Author, Body: c.Body, When: c.CreatedAt})
	}
	return msgs, root, nil
}

type reviewRecord struct {
	diff     string
	changed  []string
	context  []contextpack.Chunk
	findings []review.Finding
}

// reviewRecord loads the latest completed review's diff, context pack and
// findings; without one the thread stands alone.
func (f *followUp) reviewRecord(ctx context.Context) (reviewRecord, error) {
	var rec reviewRecord
	err := f.w.Store.WithTenant(ctx, f.tenant.ID(), func(tx pgx.Tx) error {
		var reviewID string
		var stages []byte
		err := tx.QueryRow(ctx, `SELECT r.id, c.diff, c.changed_paths, c.stages FROM reviews r
			JOIN runner_runs rr ON rr.review_id = r.id JOIN context_packs c ON c.runner_run_id = rr.id
			WHERE r.pull_request_id = $1 AND r.status = 'completed' ORDER BY r.created_at DESC LIMIT 1`, f.pr.id).
			Scan(&reviewID, &rec.diff, &rec.changed, &stages)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("worker: load review record: %w", err)
		}
		if len(stages) > 0 && stages[0] == '[' {
			if err := json.Unmarshal(stages, &rec.context); err != nil {
				return fmt.Errorf("worker: decode context pack: %w", err)
			}
		}
		rows, err := tx.Query(ctx, `SELECT path, line, severity, title, body FROM findings WHERE review_id = $1 ORDER BY path, line`, reviewID)
		if err != nil {
			return fmt.Errorf("worker: load findings: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var fd review.Finding
			var sev string
			if err := rows.Scan(&fd.Path, &fd.Line, &sev, &fd.Title, &fd.Body); err != nil {
				return err
			}
			fd.Severity = review.Severity(sev)
			rec.findings = append(rec.findings, fd)
		}
		return rows.Err()
	})
	return rec, err
}

func (f *followUp) complete(ctx context.Context, msg string) (model.CompletionResponse, error) {
	ref := f.settings.Models.Review
	if ref == "" {
		return model.CompletionResponse{}, errors.New("worker: no review model is configured for this repository")
	}
	slots := f.settings.Limits.Concurrency
	if slots <= 0 {
		slots = configfile.DefaultConcurrency
	}
	waited := time.Now()
	l, err := acquireLease(ctx, f.w.Store, f.tenant.ID(), string(ref), slots, f.jobID)
	if err != nil {
		return model.CompletionResponse{}, err
	}
	f.w.Metrics.LeaseWait(f.tenant.Slug, string(ref), time.Since(waited))
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := l.release(rctx); err != nil {
			f.logger.Warn("lease not released", "error", err)
		}
	}()
	completer, err := f.w.Completers.For(f.file, ref.Provider())
	if err != nil {
		return model.CompletionResponse{}, err
	}
	req := model.CompletionRequest{
		System: review.FollowUpSystem, User: msg, Model: ref.Model(),
		Schema: review.FollowUpSchema(), SchemaName: "reply", MaxTokens: maxOutputTokens,
	}
	if fb := f.settings.Models.Fallback; fb != "" && fb.Provider() == ref.Provider() {
		req.Fallbacks = []string{fb.Model()}
	}
	resp, err := completer.Complete(ctx, req)
	f.w.Metrics.ModelCall(f.tenant.Slug, string(ref), "followup", callOutcome(err),
		resp.InputTokens, resp.CachedTokens, resp.OutputTokens, resp.CostUSD)
	return resp, err
}

func (f *followUp) record(ctx context.Context, status, reason string, replyID int64, modelName string) error {
	return f.w.Store.WithTenant(ctx, f.tenant.ID(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO followups
			(tenant_id, pull_request_id, comment_id, author, inline, path, line, status, reason, reply_comment_id, model)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, left($9, 500), nullif($10::bigint, 0), $11)
			ON CONFLICT (pull_request_id, comment_id) DO UPDATE SET status = excluded.status, reason = excluded.reason,
				reply_comment_id = coalesce(excluded.reply_comment_id, followups.reply_comment_id), model = excluded.model`,
			f.tenant.ID(), f.pr.id, f.comment.ID, f.comment.Author, f.comment.Inline, f.comment.Path, f.comment.Line,
			status, reason, replyID, modelName)
		if err != nil {
			return fmt.Errorf("worker: record follow-up: %w", err)
		}
		return nil
	})
}
