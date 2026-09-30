package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/contextpack"
	"github.com/home-operations/kritik/internal/forge"
	"github.com/home-operations/kritik/internal/jobs"
	"github.com/home-operations/kritik/internal/model"
	"github.com/home-operations/kritik/internal/repoconfig"
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
	Base
	Completers CompleterSource
}

// Work implements river.Worker.
func (w *FollowUp) Work(ctx context.Context, job *river.Job[jobs.FollowUpArgs]) error {
	args := job.Args
	file := w.Current.Get()
	account, err := w.account(file, args.AccountID)
	if err != nil {
		return err
	}
	logger := w.Logger.With("account", account.Key(), "pr", args.Number, "comment", args.CommentID)
	pr, err := loadPullRequest(ctx, w.Store, args.AccountID, args.RepositoryID, args.Number)
	if err != nil {
		return err
	}
	client, err := w.client(ctx, file, account, pr.repository)
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
	f := &followUp{w: w, file: file, account: account, settings: file.Settings(account, pr.repository), client: client, pr: pr,
		comment: comment, owner: owner, repo: repo, botLogin: login, jobID: job.ID, logger: logger}
	if done, err := f.alreadyAnswered(ctx); err != nil || done {
		return err
	}
	outcome, err := f.run(ctx)
	w.Metrics.FollowUp(account.Key(), outcome)
	if err != nil {
		logger.Error("follow-up failed", "error", err)
		_ = f.record(ctx, followUpFailed, err.Error(), 0, "")
		return err
	}
	logger.Info("follow-up " + outcome)
	return nil
}

type followUp struct {
	w        *FollowUp
	file     *configfile.File
	account  *configfile.Account
	settings configfile.Settings
	// ruleFiles are the files settings' rules name, as repoConfig read
	// them.
	ruleFiles repoconfig.Files
	client    forge.Client
	pr        *pullRequest
	comment   forge.Comment
	owner     string
	repo      string
	botLogin  string
	jobID     int64
	logger    *slog.Logger
}

// alreadyAnswered guards a retried job: once a reply is on the forge the
// mention is done, whatever happened after posting.
func (f *followUp) alreadyAnswered(ctx context.Context) (bool, error) {
	var status string
	var replyID *int64
	err := f.w.Store.WithAccount(ctx, f.account.ID(), func(tx pgx.Tx) error {
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

// run qualifies the mention (ADR-0002 §2.7), gathers the thread and the review's
// record, asks the model, and posts the reply. Nothing after the reply is
// posted may fail the job: a retry would answer twice.
func (f *followUp) run(ctx context.Context) (string, error) {
	if reason := f.disqualified(ctx); reason != "" {
		f.logger.Info("follow-up ignored", "reason", reason)
		return followUpIgnored, f.record(ctx, followUpIgnored, reason, 0, "")
	}
	if requestsReview(f.comment.Body, strings.TrimSuffix(f.botLogin, "[bot]")) {
		return f.requestReview(ctx)
	}
	reason, err := f.repoConfig(ctx)
	if err != nil {
		return followUpFailed, err
	}
	if reason != "" {
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
	thread, err := f.thread(ctx)
	if err != nil {
		return followUpFailed, err
	}
	rec, err := f.reviewRecord(ctx)
	if err != nil {
		return followUpFailed, err
	}
	// The review's runner read the agent files; the rules' files are read
	// again at the merge base, and win.
	files := maps.Clone(rec.files)
	if files == nil {
		files = repoconfig.Files{}
	}
	maps.Copy(files, f.ruleFiles)
	var agent []string
	if f.settings.Review.AgentFiles {
		agent = repoconfig.AgentFiles(files, rec.changed)
	}
	instructions, _ := repoconfig.Instructions(files, agent)
	rules, _ := repoconfig.ActiveRules(repoconfig.RulesFor(f.settings.Review.Rules, rec.vars), files, rec.changed)
	system := review.FollowUpSystemPrompt(rules, instructions)
	msg := review.BuildFollowUp(review.Input{
		Repository: f.pr.repository, Number: f.pr.number, Title: f.pr.title, Author: f.pr.author, BaseRef: f.pr.baseRef,
		Body: rec.body, Changed: rec.changed, Diff: rec.diff, Context: rec.context, BudgetTokens: review.UserBudget(system),
	}, rec.findings, thread)
	resp, err := f.complete(ctx, system, msg, rec.id)
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
		replyID, err = f.client.ReplyInline(ctx, f.owner, f.repo, f.pr.number, f.comment, body)
	} else {
		replyID, err = f.client.CreateComment(ctx, f.owner, f.repo, f.pr.number, body)
	}
	if err != nil {
		return followUpFailed, err
	}
	f.logger.Info("follow-up answered", "model", resp.Model, "reply", replyID, "input_tokens", resp.InputTokens,
		"output_tokens", resp.OutputTokens, "cost_usd", resp.CostUSD)
	err = f.w.Store.WithAccount(ctx, f.account.ID(), func(tx pgx.Tx) error {
		return insertUsage(ctx, tx, usageRow{
			accountID: f.account.ID(), repositoryID: f.pr.repositoryID, role: roleFollowUp, model: resp.Model, upstream: resp.Upstream,
			input: resp.InputTokens, output: resp.OutputTokens, costUSD: resp.CostUSD,
		})
	})
	if err != nil {
		f.logger.Error("follow-up usage not recorded", "error", err)
	}
	if err := f.record(ctx, followUpAnswered, "", replyID, resp.Model); err != nil {
		f.logger.Error("follow-up not recorded", "error", err, "reply", replyID)
	}
	return followUpAnswered, nil
}

var (
	mentionPattern = regexp.MustCompile(`(?i)(^|[^\w@])@([\w-]+)`)
	reviewPattern  = regexp.MustCompile(`(?i)(^|[^\w@])@([\w-]+)\s+review\b`)
)

// requestsReview reports whether body asks slug for a review: "@<slug>
// review", the word review right after the mention.
func requestsReview(body, slug string) bool {
	for _, m := range reviewPattern.FindAllStringSubmatch(body, -1) {
		if strings.EqualFold(m[2], slug) {
			return true
		}
	}
	return false
}

// requestReview queues a review of the pull request's head, as the
// dashboard's re-run does, for someone with write access who asked with
// "@<bot> review": it is how a pull request from a fork, which is not
// reviewed on its own, gets one. It replies that it did, and counts
// against the hourly follow-up limit.
func (f *followUp) requestReview(ctx context.Context) (string, error) {
	limited, err := f.rateLimited(ctx)
	if err != nil {
		return followUpFailed, err
	}
	if limited {
		return followUpLimited, nil
	}
	queue := river.ClientFromContext[pgx.Tx](ctx)
	already := false
	err = f.w.Store.WithAccount(ctx, f.account.ID(), func(tx pgx.Tx) error {
		_, err := jobs.EnqueueRerun(ctx, tx, queue, f.account.ID(), f.pr.repositoryID, f.pr.number)
		if errors.Is(err, jobs.ErrRerunQueued) {
			already, err = true, nil
		}
		return err
	})
	if errors.Is(err, jobs.ErrNoHead) {
		return followUpIgnored, f.record(ctx, followUpIgnored, "the pull request is closed", 0, "")
	}
	if err != nil {
		return followUpFailed, fmt.Errorf("worker: queue the requested review: %w", err)
	}
	body := review.ReviewQueuedBody(f.pr.headSHA, already)
	var replyID int64
	if f.comment.Inline {
		replyID, err = f.client.ReplyInline(ctx, f.owner, f.repo, f.pr.number, f.comment, body)
	} else {
		replyID, err = f.client.CreateComment(ctx, f.owner, f.repo, f.pr.number, body)
	}
	if err != nil {
		return followUpFailed, err
	}
	f.logger.Info("review requested", "already_queued", already, "reply", replyID)
	if err := f.record(ctx, followUpAnswered, "review requested", replyID, ""); err != nil {
		f.logger.Error("follow-up not recorded", "error", err, "reply", replyID)
	}
	return followUpAnswered, nil
}

// mentioned reports whether body @-mentions slug as a whole word.
func mentioned(body, slug string) bool {
	for _, m := range mentionPattern.FindAllStringSubmatch(body, -1) {
		if strings.EqualFold(m[2], slug) {
			return true
		}
	}
	return false
}

// disqualified returns why the mention is not answered, or "".
func (f *followUp) disqualified(ctx context.Context) string {
	if f.comment.AuthorIsBot || strings.EqualFold(f.comment.Author, f.botLogin) {
		return "author is a bot"
	}
	slug := strings.TrimSuffix(f.botLogin, "[bot]")
	if !mentioned(f.comment.Body, slug) {
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
	err := f.w.Store.WithAccount(ctx, f.account.ID(), func(tx pgx.Tx) error {
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
		if replyID, err = f.client.CreateComment(ctx, f.owner, f.repo, f.pr.number, review.LimitBody); err != nil {
			return true, err
		}
	}
	f.logger.Info("follow-up rate limited", "answered_last_hour", answered, "notice_posted", notices == 0)
	return true, f.record(ctx, followUpLimited, "", replyID, "")
}

// thread returns the messages the model sees. The asking comment is always
// last.
func (f *followUp) thread(ctx context.Context) ([]review.Message, error) {
	var comments []forge.Comment
	var err error
	switch {
	case !f.comment.Inline:
		if comments, err = f.client.ListConversation(ctx, f.owner, f.repo, f.pr.number); err != nil {
			return nil, err
		}
	// Only a reply has a thread to gather: a comment that replies to none
	// starts its thread.
	case f.comment.InReplyTo != 0:
		root := f.comment.InReplyTo
		all, err := f.client.ListInline(ctx, f.owner, f.repo, f.pr.number)
		if err != nil {
			return nil, err
		}
		for _, c := range all {
			if c.ID == root || c.InReplyTo == root {
				comments = append(comments, c)
			}
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
	slices.SortStableFunc(comments, func(a, b forge.Comment) int { return a.CreatedAt.Compare(b.CreatedAt) })
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
	return msgs, nil
}

type reviewRecord struct {
	// id is the review's, empty without one.
	id       string
	body     string
	diff     string
	changed  []string
	context  []contextpack.Chunk
	findings []review.Finding
	// files are the repository files the review's runner read, its agent
	// files among them.
	files repoconfig.Files
	// vars is the pull request as a filter sees it, with the event of the
	// review, which the rules' whenExpr are judged against.
	vars map[string]any
}

// reviewRecord loads the pull request description, its filter variables,
// and the latest completed review's diff, context pack, repository files
// and findings; without a review the thread stands alone.
func (f *followUp) reviewRecord(ctx context.Context) (reviewRecord, error) {
	var rec reviewRecord
	err := f.w.Store.WithAccount(ctx, f.account.ID(), func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT body FROM pull_requests WHERE id = $1`, f.pr.id).Scan(&rec.body); err != nil {
			return fmt.Errorf("worker: load pull request body: %w", err)
		}
		last, err := lastCompleted(ctx, tx, f.pr.id)
		if err != nil {
			return err
		}
		var trigger string
		if last.id != "" {
			if err := tx.QueryRow(ctx, `SELECT trigger FROM reviews WHERE id = $1`, last.id).Scan(&trigger); err != nil {
				return fmt.Errorf("worker: load review trigger: %w", err)
			}
		}
		if rec.vars, err = filterVars(ctx, tx, f.pr.id, trigger); err != nil || last.id == "" {
			return err
		}
		rec.id, rec.findings = last.id, reviewFindings(last.findings)
		var stages, files []byte
		err = tx.QueryRow(ctx, `SELECT c.diff, c.changed_paths, c.stages, c.repo_files FROM runner_runs rr
			JOIN context_packs c ON c.runner_run_id = rr.id WHERE rr.review_id = $1`, last.id).
			Scan(&rec.diff, &rec.changed, &stages, &files)
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
		if err := json.Unmarshal(files, &rec.files); err != nil {
			return fmt.Errorf("worker: decode repository files: %w", err)
		}
		return nil
	})
	return rec, err
}

// repoConfig applies the .kritik.yaml at the pull request's merge base to
// the follow-up's settings, so it answers with the repository's model and
// instructions, and reads the instruction files from the same commit. It
// returns why the file stops the follow-up, or "".
func (f *followUp) repoConfig(ctx context.Context) (string, error) {
	base, err := f.client.MergeBase(ctx, f.owner, f.repo, f.pr.baseRef, f.pr.headSHA)
	if err != nil {
		return "", err
	}
	doc, _, err := readRepoConfig(ctx, f.client, f.owner, f.repo, base)
	if err != nil {
		return "", err
	}
	eff, _ := effective(f.settings, doc)
	if !eff.Enabled {
		return repoconfig.SkipDisabled.Description(), nil
	}
	f.settings = eff.Settings
	// A follow-up has no summary to note a file it could not use in, so
	// one over the forge's size limit is left out like a missing one.
	files, _, err := repoconfig.Collect(func(p string) ([]byte, error) {
		b, err := f.client.FileAt(ctx, f.owner, f.repo, base, p)
		if errors.Is(err, forge.ErrFileTooLarge) {
			return nil, fmt.Errorf("worker: %s: %w", p, fs.ErrNotExist)
		}
		return b, err
	}, ruleFiles(eff.Review.Rules)...)
	if err != nil {
		return "", err
	}
	f.ruleFiles = files
	return "", nil
}

// ruleFiles is the files rules name, in order.
func ruleFiles(rules []configfile.Rule) []string {
	var out []string
	for _, r := range rules {
		if r.File != "" {
			out = append(out, r.File)
		}
	}
	return out
}

// complete asks the review model for the reply, with the repository's
// instructions, recording the call against the comment and, when there is
// one, reviewID.
func (f *followUp) complete(ctx context.Context, system, msg, reviewID string) (model.CompletionResponse, error) {
	ref := f.settings.Models.Review
	if ref == "" {
		return model.CompletionResponse{}, errors.New("worker: no review model is configured for this repository")
	}
	stepper, err := f.w.Completers.Stepper(f.file, f.account, ref.Provider())
	if err != nil {
		return model.CompletionResponse{}, err
	}
	spec, _ := f.file.Provider(f.account, ref.Provider())
	completer := model.Structured{Stepper: stepper, OnStep: f.w.onStep(ctx, f.logger, store.ModelCall{
		AccountID: f.account.ID(), ReviewID: reviewID, FollowupCommentID: f.comment.ID, Kind: store.ModelCallFollowUp,
	}, transcriptMask(f.file, spec))}
	req := model.CompletionRequest{
		System: system, User: msg, Model: ref.Model(),
		Schema: review.FollowUpSchema(), SchemaName: "reply", MaxTokens: maxOutputTokens,
	}
	if fb := f.settings.Models.Fallback; fb != "" && fb.Provider() == ref.Provider() {
		req.Fallbacks = []string{fb.Model()}
	}
	var resp model.CompletionResponse
	err = f.w.withLease(ctx, f.account, string(ref), f.settings.Limits.Concurrency, f.jobID, func(ctx context.Context) error {
		var err error
		resp, err = completer.Complete(ctx, req)
		f.w.Metrics.ModelCall(f.account.Key(), servedRef(ref, resp.Model), "followup", callOutcome(err),
			resp.InputTokens, resp.CachedTokens, resp.OutputTokens, resp.CostUSD)
		return err
	})
	return resp, err
}

func (f *followUp) record(ctx context.Context, status, reason string, replyID int64, modelName string) error {
	return f.w.Store.WithAccount(ctx, f.account.ID(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO followups
			(account_id, pull_request_id, comment_id, author, inline, path, line, status, reason, reply_comment_id, model)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, left($9, 500), nullif($10::bigint, 0), $11)
			ON CONFLICT (pull_request_id, comment_id) DO UPDATE SET status = excluded.status, reason = excluded.reason,
				reply_comment_id = coalesce(excluded.reply_comment_id, followups.reply_comment_id), model = excluded.model`,
			f.account.ID(), f.pr.id, f.comment.ID, f.comment.Author, f.comment.Inline, f.comment.Path, f.comment.Line,
			status, reason, replyID, modelName)
		if err != nil {
			return fmt.Errorf("worker: record follow-up: %w", err)
		}
		return nil
	})
}
