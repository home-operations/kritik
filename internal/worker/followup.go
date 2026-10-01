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
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/home-operations/kritika/internal/adapter"
	"github.com/home-operations/kritika/internal/configfile"
	"github.com/home-operations/kritika/internal/contextpack"
	"github.com/home-operations/kritika/internal/forge"
	"github.com/home-operations/kritika/internal/jobs"
	"github.com/home-operations/kritika/internal/model"
	"github.com/home-operations/kritika/internal/repoconfig"
	"github.com/home-operations/kritika/internal/review"
	"github.com/home-operations/kritika/internal/store"
)

// Follow-up bounds: mentions answered per pull request per hour before a
// single "limit reached" reply, and thread messages kept in the prompt.
const (
	followUpsPerHour = 5
	threadMessages   = 20
)

// followUpMaxOutputTokens bounds one reply. Replies are short by
// instruction; this is a guard against a runaway model, not a target.
const followUpMaxOutputTokens = 4096

// FollowUp works the followup queue: one job answers one comment that
// @-mentioned the bot, scoped to its thread.
type FollowUp struct {
	river.WorkerDefaults[jobs.FollowUpArgs]
	Base
	Steppers *adapter.Steppers
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
	owner, repo := pr.ownerRepo()
	comment, err := client.GetComment(ctx, owner, repo, args.CommentID, args.Inline)
	if err != nil {
		return err
	}
	login, err := client.BotLogin(ctx)
	if err != nil {
		return err
	}
	f := &followUp{w: w, file: file, account: account, settings: file.Settings(account, pr.repository), client: client, pr: pr,
		comment: comment, owner: owner, repo: repo, botLogin: login, jobID: job.ID, attempt: job.Attempt, logger: logger}
	if done, err := f.alreadyAnswered(ctx); err != nil || done {
		return err
	}
	outcome, err := f.run(ctx)
	w.Metrics.FollowUp(account.Key(), string(outcome))
	if err != nil {
		logger.Error("follow-up failed", "error", err)
		// The failure itself is what goes back to River; a record of it that
		// could not be written is lost with it, and the retry records anew.
		_ = f.record(ctx, store.FollowupFailed, err.Error(), 0, "")
		return err
	}
	logger.Info("follow-up " + string(outcome))
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
	// attempt is the job's, 1 the first time.
	attempt int
	logger  *slog.Logger
}

// alreadyAnswered guards a retried job: once a reply is on the forge the
// mention is done, whatever happened after posting. The record says so
// first; on a retry, the forge is asked too, by the FollowUpMarker the
// reply carries, since an attempt killed between posting and recording
// left none.
func (f *followUp) alreadyAnswered(ctx context.Context) (bool, error) {
	var status store.FollowupStatus
	var replyID *int64
	err := f.w.Store.WithAccount(ctx, f.account.ID(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status, reply_comment_id FROM followups WHERE pull_request_id = $1 AND comment_id = $2`,
			f.pr.id, f.comment.ID).Scan(&status, &replyID)
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("worker: read follow-up: %w", err)
	}
	if status == store.FollowupAnswered || status == store.FollowupLimited || replyID != nil {
		f.logger.Info("follow-up already handled", "status", status)
		return true, nil
	}
	if f.attempt <= 1 {
		return false, nil
	}
	id, err := f.repliedOnForge(ctx)
	if err != nil || id == 0 {
		return false, err
	}
	f.logger.Info("follow-up already answered on the forge", "reply", id)
	return true, f.record(ctx, store.FollowupAnswered, "", id, "")
}

// repliedOnForge returns the id of the bot's reply to the comment, by its
// FollowUpMarker, where the reply would be posted, or 0.
func (f *followUp) repliedOnForge(ctx context.Context) (int64, error) {
	var comments []forge.Comment
	var err error
	if f.comment.Inline {
		comments, err = f.client.ListInline(ctx, f.owner, f.repo, f.pr.number)
	} else {
		comments, err = f.client.ListConversation(ctx, f.owner, f.repo, f.pr.number)
	}
	if err != nil {
		return 0, err
	}
	return markedReply(comments, f.botLogin, f.comment.ID), nil
}

// markedReply is the id of the comment by login whose FollowUpMarker names
// commentID, or 0.
func markedReply(comments []forge.Comment, login string, commentID int64) int64 {
	want := strconv.FormatInt(commentID, 10)
	for _, c := range comments {
		if !strings.EqualFold(c.Author, login) {
			continue
		}
		if id, ok := review.MarkedFollowUp(c.Body); ok && id == want {
			return c.ID
		}
	}
	return 0
}

// run qualifies the mention, gathers the thread and the review's record,
// asks the model, and posts the reply. Nothing after the reply is posted
// may fail the job: a retry would answer twice.
func (f *followUp) run(ctx context.Context) (store.FollowupStatus, error) {
	if reason := f.disqualified(ctx); reason != "" {
		f.logger.Info("follow-up ignored", "reason", reason)
		return store.FollowupIgnored, f.record(ctx, store.FollowupIgnored, reason, 0, "")
	}
	slug := strings.TrimSuffix(f.botLogin, "[bot]")
	if requestsReview(f.comment.Body, slug) {
		return f.requestReview(ctx)
	}
	if reason, ok := requestsDismiss(f.comment.Body, slug); ok {
		return f.dismiss(ctx, reason)
	}
	if paused, ok := requestsPause(f.comment.Body, slug); ok {
		return f.pause(ctx, slug, paused)
	}
	reason, err := f.repoConfig(ctx)
	if err != nil {
		return store.FollowupFailed, err
	}
	if reason != "" {
		f.logger.Info("follow-up ignored", "reason", reason)
		return store.FollowupIgnored, f.record(ctx, store.FollowupIgnored, reason, 0, "")
	}
	limited, err := f.rateLimited(ctx)
	if err != nil {
		return store.FollowupFailed, err
	}
	if limited {
		return store.FollowupLimited, nil
	}
	thread, err := f.thread(ctx)
	if err != nil {
		return store.FollowupFailed, err
	}
	rec, err := f.reviewRecord(ctx)
	if err != nil {
		return store.FollowupFailed, err
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
		return store.FollowupFailed, err
	}
	reply, err := review.ParseFollowUp(resp.Raw)
	if err != nil {
		return store.FollowupFailed, err
	}
	body := review.FollowUpBody(reply, resp.Model)
	replyID, err := f.reply(ctx, body)
	if err != nil {
		return store.FollowupFailed, err
	}
	f.logger.Info("follow-up answered", "model", resp.Model, "reply", replyID, "input_tokens", resp.InputTokens,
		"output_tokens", resp.OutputTokens, "cost_usd", resp.CostUSD)
	err = f.w.Store.WithAccount(ctx, f.account.ID(), func(tx pgx.Tx) error {
		return store.InsertUsage(ctx, tx, store.Usage{
			AccountID: f.account.ID(), RepositoryID: f.pr.repositoryID, Role: store.RoleFollowUp, Model: resp.Model, Upstream: resp.Upstream,
			Input: resp.InputTokens, Output: resp.OutputTokens, CostUSD: resp.CostUSD,
		})
	})
	if err != nil {
		f.logger.Error("follow-up usage not recorded", "error", err)
	}
	if err := f.record(ctx, store.FollowupAnswered, "", replyID, resp.Model); err != nil {
		f.logger.Error("follow-up not recorded", "error", err, "reply", replyID)
	}
	return store.FollowupAnswered, nil
}

var (
	mentionPattern = regexp.MustCompile(`(?i)(^|[^\w@])@([\w-]+)`)
	reviewPattern  = regexp.MustCompile(`(?i)(^|[^\w@])@([\w-]+)\s+review\b`)
	dismissPattern = regexp.MustCompile(`(?i)(^|[^\w@])@([\w-]+)\s+dismiss(?:ed)?\b`)
	pausePattern   = regexp.MustCompile(`(?i)(^|[^\w@])@([\w-]+)\s+(pause|resume)\b`)
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

// requestsDismiss reports whether body asks slug to dismiss the finding it
// replies to, "@<slug> dismiss", and returns the reason: the rest of the
// comment.
func requestsDismiss(body, slug string) (string, bool) {
	for _, m := range dismissPattern.FindAllStringSubmatchIndex(body, -1) {
		if strings.EqualFold(body[m[4]:m[5]], slug) {
			return strings.TrimSpace(strings.TrimLeft(body[m[1]:], " \t\r\n,:;.-")), true
		}
	}
	return "", false
}

// dismiss records the finding the comment replies to as dismissed, with
// reason, resolves its thread and says so: later reviews of the pull
// request are told not to raise it again. Only a reply in one of the bot's
// finding threads names a finding; anywhere else, the reply says how to
// use it. It counts against the hourly follow-up limit.
func (f *followUp) dismiss(ctx context.Context, reason string) (store.FollowupStatus, error) {
	limited, err := f.rateLimited(ctx)
	if err != nil {
		return store.FollowupFailed, err
	}
	if limited {
		return store.FollowupLimited, nil
	}
	fingerprint, err := f.dismissedFinding(ctx)
	if err != nil {
		return store.FollowupFailed, err
	}
	if fingerprint == "" {
		return f.answer(ctx, review.DismissHintBody, "dismiss outside a finding thread")
	}
	var found bool
	err = f.w.Store.WithAccount(ctx, f.account.ID(), func(tx pgx.Tx) error {
		finding, ok, err := store.LatestFinding(ctx, tx, f.pr.id, fingerprint)
		if err != nil || !ok {
			return err
		}
		found = true
		return store.RecordDismissal(ctx, tx, store.Dismissal{
			AccountID: f.account.ID(), PullRequestID: f.pr.id, Fingerprint: fingerprint, Finding: finding,
			Reason: reason, Author: f.comment.Author, CommentID: f.comment.ID,
		})
	})
	if err != nil {
		return store.FollowupFailed, err
	}
	if !found {
		return f.answer(ctx, review.DismissUnknownBody, "finding unknown")
	}
	// Best effort, like the review's own thread resolution: the record and
	// the reply stand either way.
	if _, err := f.client.ResolveThread(ctx, f.owner, f.repo, f.pr.number, f.comment.InReplyTo, false); err != nil {
		f.logger.Warn("dismissed finding's thread not resolved", "comment", f.comment.InReplyTo, "error", err)
	}
	f.logger.Info("finding dismissed", "fingerprint", fingerprint, "reason", reason)
	return f.answer(ctx, review.DismissedBody, "finding dismissed")
}

// dismissedFinding is the fingerprint of the finding the comment replies
// to: the one the bot's root comment of its thread carries a FindingMarker
// for. "" when the comment is not such a reply.
func (f *followUp) dismissedFinding(ctx context.Context) (string, error) {
	if !f.comment.Inline || f.comment.InReplyTo == 0 {
		return "", nil
	}
	root, err := f.client.GetComment(ctx, f.owner, f.repo, f.comment.InReplyTo, true)
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(root.Author, f.botLogin) {
		return "", nil
	}
	fingerprint, _ := review.MarkedFinding(root.Body)
	return fingerprint, nil
}

// answer posts body as the reply and records the mention answered, for
// reason.
func (f *followUp) answer(ctx context.Context, body, reason string) (store.FollowupStatus, error) {
	replyID, err := f.reply(ctx, body)
	if err != nil {
		return store.FollowupFailed, err
	}
	if err := f.record(ctx, store.FollowupAnswered, reason, replyID, ""); err != nil {
		f.logger.Error("follow-up not recorded", "error", err, "reply", replyID)
	}
	return store.FollowupAnswered, nil
}

// requestsPause reports whether body asks slug to pause or resume the pull
// request's automatic reviews, "@<slug> pause" or "@<slug> resume", and
// which.
func requestsPause(body, slug string) (paused, ok bool) {
	for _, m := range pausePattern.FindAllStringSubmatch(body, -1) {
		if strings.EqualFold(m[2], slug) {
			return strings.EqualFold(m[3], "pause"), true
		}
	}
	return false, false
}

// pause pauses or resumes the pull request's automatic reviews, as someone
// with write access asked, and says so. It counts against the hourly
// follow-up limit.
func (f *followUp) pause(ctx context.Context, slug string, paused bool) (store.FollowupStatus, error) {
	limited, err := f.rateLimited(ctx)
	if err != nil {
		return store.FollowupFailed, err
	}
	if limited {
		return store.FollowupLimited, nil
	}
	err = f.w.Store.WithAccount(ctx, f.account.ID(), func(tx pgx.Tx) error {
		return store.PausePullRequest(ctx, tx, f.pr.id, paused)
	})
	if err != nil {
		return store.FollowupFailed, err
	}
	if paused {
		f.logger.Info("automatic reviews paused")
		return f.answer(ctx, review.PausedBody(slug), "reviews paused")
	}
	f.logger.Info("automatic reviews resumed")
	return f.answer(ctx, review.ResumedBody, "reviews resumed")
}

// requestReview queues a review of the pull request's head, as the
// dashboard's re-run does, for someone with write access who asked with
// "@<bot> review": it is how a pull request from a fork, which is not
// reviewed on its own, gets one. It replies that it did, and counts
// against the hourly follow-up limit.
func (f *followUp) requestReview(ctx context.Context) (store.FollowupStatus, error) {
	limited, err := f.rateLimited(ctx)
	if err != nil {
		return store.FollowupFailed, err
	}
	if limited {
		return store.FollowupLimited, nil
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
		return store.FollowupIgnored, f.record(ctx, store.FollowupIgnored, "the pull request is closed", 0, "")
	}
	if err != nil {
		return store.FollowupFailed, fmt.Errorf("worker: queue the requested review: %w", err)
	}
	body := review.ReviewQueuedBody(f.pr.headSHA, already)
	replyID, err := f.reply(ctx, body)
	if err != nil {
		return store.FollowupFailed, err
	}
	f.logger.Info("review requested", "already_queued", already, "reply", replyID)
	if err := f.record(ctx, store.FollowupAnswered, "review requested", replyID, ""); err != nil {
		f.logger.Error("follow-up not recorded", "error", err, "reply", replyID)
	}
	return store.FollowupAnswered, nil
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
	return true, f.record(ctx, store.FollowupLimited, "", replyID, "")
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
	// The asking comment closes the thread whatever the timestamps say.
	asking := f.comment
	if i := slices.IndexFunc(comments, func(c forge.Comment) bool { return c.ID == f.comment.ID }); i >= 0 {
		asking = comments[i]
		comments = slices.Delete(comments, i, i+1)
	}
	slices.SortStableFunc(comments, func(a, b forge.Comment) int { return a.CreatedAt.Compare(b.CreatedAt) })
	comments = append(comments, asking)
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
		if rec.vars, err = filterVars(ctx, tx, f.pr.id, last.trigger); err != nil || last.id == "" {
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
		if err := json.Unmarshal(stages, &rec.context); err != nil {
			return fmt.Errorf("worker: decode context pack: %w", err)
		}
		if err := json.Unmarshal(files, &rec.files); err != nil {
			return fmt.Errorf("worker: decode repository files: %w", err)
		}
		return nil
	})
	return rec, err
}

// repoConfig applies the .kritika.yaml at the pull request's merge base to
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
	stepper, err := f.w.Steppers.Stepper(f.file, f.account, ref.Provider())
	if err != nil {
		return model.CompletionResponse{}, err
	}
	spec, _ := f.file.Provider(f.account, ref.Provider())
	completer := model.Structured{Stepper: stepper, OnStep: f.w.recorder().OnStep(ctx, f.logger, store.ModelCall{
		AccountID: f.account.ID(), ReviewID: reviewID, FollowupCommentID: f.comment.ID, Kind: store.ModelCallFollowUp,
	}, adapter.Mask(f.file, spec))}
	req := model.CompletionRequest{
		System: system, User: msg, Model: ref.Model(),
		Schema: review.FollowUpSchema(), SchemaName: "reply", MaxTokens: followUpMaxOutputTokens,
	}
	if fb := f.settings.Models.Fallback; fb != "" && fb.Provider() == ref.Provider() {
		req.Fallbacks = []string{fb.Model()}
	}
	var resp model.CompletionResponse
	err = f.w.withLease(ctx, f.account, string(ref), f.settings.Limits.Concurrency, f.jobID, func(ctx context.Context) error {
		var err error
		resp, err = completer.Complete(ctx, req)
		f.w.Metrics.ModelCall(f.account.Key(), adapter.ServedRef(ref, resp.Model), store.RoleFollowUp, adapter.Outcome(err),
			resp.InputTokens, resp.CachedTokens, resp.OutputTokens, resp.CostUSD)
		return err
	})
	return resp, err
}

// reply posts body, led by its FollowUpMarker, where the mention was made:
// in its inline thread, or on the conversation.
func (f *followUp) reply(ctx context.Context, body string) (int64, error) {
	body = review.FollowUpMarker(f.comment.ID) + "\n" + body
	if f.comment.Inline {
		return f.client.ReplyInline(ctx, f.owner, f.repo, f.pr.number, f.comment, body)
	}
	return f.client.CreateComment(ctx, f.owner, f.repo, f.pr.number, body)
}

func (f *followUp) record(ctx context.Context, status store.FollowupStatus, reason string, replyID int64, modelName string) error {
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
