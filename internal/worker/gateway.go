package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/agent"
	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/contextpack"
	"github.com/home-operations/kritik/internal/model"
	"github.com/home-operations/kritik/internal/review"
	"github.com/home-operations/kritik/internal/store"
)

// Gateway serves the worker's gateway listener: the egress proxy runner
// pods reach the outside through (ADR-0008), and the model and
// similar-code endpoints an agentic runner calls with its run token
// (ADR-0004, ADR-0026). No provider key enters a runner pod: the gateway
// reserves each call against the run's budget and checks the account's
// monthly cap, answers it through the account's provider, and records
// what it spent where the caps see it.
type Gateway struct {
	Base
	// Proxy serves CONNECT and absolute-URI requests.
	Proxy    http.Handler
	Steppers *Completers
	// Embedders resolves the instance's embedder for stage 4; nil serves
	// no similar code.
	Embedders *Embedders
}

// maxGatewayBody bounds one step's request: the whole conversation so far,
// every tool output in it capped.
const maxGatewayBody = 16 << 20

// GatewayDrain is how long a stopping worker lets model steps in flight
// finish. A step it cuts is paid for and not recorded, and the runner's
// retry is paid for again, so it covers a long step rather than the usual
// few seconds; the chart's grace period outlasts it.
const GatewayDrain = 2 * time.Minute

// maxStepOutput caps the answer to one step, whatever the runner asks: the
// agent loop's own cap.
var maxStepOutput = agent.DefaultLimits.MaxOutputTokensPerStep

// ServeHTTP implements http.Handler.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodConnect || r.URL.IsAbs():
		g.Proxy.ServeHTTP(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
		g.chat(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/similar":
		g.similarCode(w, r)
	default:
		http.NotFound(w, r)
	}
}

// refuse answers a step with an error. Only a 500, the gateway's own
// trouble reaching its database, is worth the runner's retry; every other
// refusal is final: the provider was already retried, the budget is spent,
// or the request is wrong.
func refuse(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	if status != http.StatusInternalServerError {
		w.Header().Set("X-Should-Retry", "false")
	}
	w.WriteHeader(status)
	_, _ = w.Write(model.EncodeChatError(code, message))
}

// runCall is a runner's request to the gateway, authenticated by its run
// token.
type runCall struct {
	token   string
	grant   store.GatewayGrant
	file    *configfile.File
	account *configfile.Account
	logger  *slog.Logger
}

// admit authenticates r by its run token, finds the run's account in the
// current configuration and checks the account's monthly cap, or refuses
// r and reports false.
func (g *Gateway) admit(w http.ResponseWriter, r *http.Request) (runCall, bool) {
	ctx := r.Context()
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	grant, err := g.Store.LookupGatewayToken(ctx, token)
	if errors.Is(err, store.ErrGatewayToken) {
		refuse(w, http.StatusUnauthorized, "invalid_token", "the run token is not valid")
		return runCall{}, false
	}
	if err != nil {
		g.Logger.Error("gateway: token lookup failed", "error", err)
		refuse(w, http.StatusInternalServerError, "server_error", "the run token could not be checked")
		return runCall{}, false
	}
	file := g.Current.Get()
	account, found := file.AccountByID(grant.AccountID)
	if !found {
		refuse(w, http.StatusForbidden, "invalid_token", "the run's account is not in the configuration")
		return runCall{}, false
	}
	logger := g.Logger.With("account", account.Key(), "run", review.ShortSHA(grant.RunID))
	capped, err := g.monthCapped(ctx, file, account)
	if err != nil {
		logger.Error("gateway: caps not read", "error", err)
		refuse(w, http.StatusInternalServerError, "server_error", "the caps could not be checked")
		return runCall{}, false
	}
	if capped != "" {
		logger.Info("gateway: call refused", "reason", capped)
		refuse(w, http.StatusTooManyRequests, model.BudgetCode, capped)
		return runCall{}, false
	}
	return runCall{token: token, grant: grant, file: file, account: account, logger: logger}, true
}

// reserve reserves tokens against c's run budget before the call spends
// them, or refuses the request and reports false. Each reservation is
// visible to the next at once, so concurrent calls cannot all pass a
// budget one of them spends.
func (g *Gateway) reserve(ctx context.Context, w http.ResponseWriter, c runCall, tokens int64) bool {
	ok, err := g.Store.ReserveGatewayTokens(ctx, c.token, tokens)
	if err != nil {
		c.logger.Error("gateway: call not reserved", "error", err)
		refuse(w, http.StatusInternalServerError, "server_error", "the run's budget could not be checked")
		return false
	}
	if !ok {
		reason := fmt.Sprintf("the run's budget of %d tokens is spent", c.grant.Budget)
		c.logger.Info("gateway: call refused", "reason", reason)
		refuse(w, http.StatusTooManyRequests, model.BudgetCode, reason)
		return false
	}
	return true
}

func (g *Gateway) chat(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := g.admit(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxGatewayBody))
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		refuse(w, http.StatusRequestEntityTooLarge, "invalid_request", err.Error())
		return
	}
	if err != nil {
		refuse(w, http.StatusBadRequest, "invalid_request", "reading the request: "+err.Error())
		return
	}
	req, err := model.DecodeChatRequest(body)
	if err != nil {
		refuse(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.Model != gatewayModel {
		refuse(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("the model is %q, not %q", gatewayModel, req.Model))
		return
	}

	ref := configfile.ModelRef(c.grant.Model)
	provider, _ := c.file.Provider(c.account, ref.Provider())
	stepper, err := g.Steppers.Stepper(c.file, c.account, ref.Provider())
	if err != nil {
		c.logger.Error("gateway: no model adapter", "error", err)
		refuse(w, http.StatusInternalServerError, "server_error", "the run's model is not configured")
		return
	}
	req.Model, req.Fallbacks = ref.Model(), nil
	if fb := configfile.ModelRef(c.grant.Fallback); fb != "" && fb.Provider() == ref.Provider() {
		req.Fallbacks = []string{fb.Model()}
	}
	if req.MaxTokens <= 0 || req.MaxTokens > maxStepOutput {
		req.MaxTokens = maxStepOutput
	}
	// The step is reserved before it runs, its prompt estimated at four
	// characters a token of the request; its actual spend replaces the
	// estimate once the provider answers.
	reserved := int64(len(body))/4 + req.MaxTokens
	if !g.reserve(ctx, w, c, reserved) {
		return
	}
	start := time.Now()
	resp, err := stepper.Step(ctx, req)
	took := time.Since(start)
	g.Metrics.ModelCall(c.account.Key(), servedRef(ref, resp.Model), roleReview, callOutcome(err), resp.Usage.Prompt(), resp.Usage.CacheRead,
		resp.Usage.Output, resp.CostUSD)
	if cerr := g.charge(ctx, c.grant, c.token, reserved, resp, err == nil); cerr != nil {
		// A step that was answered is paid for either way; the run still
		// gets the answer.
		c.logger.Error("gateway: step not charged", "error", cerr)
	}
	// Recorded before the runner gets its answer, so the next step's delta
	// is taken against this one; recordModelCall bounds how long it waits.
	g.recordModelCall(ctx, c.logger, store.ModelCall{
		AccountID: c.grant.AccountID, ReviewID: c.grant.ReviewID, RunnerRunID: c.grant.RunID, Kind: store.ModelCallAgentStep, Duration: took,
	}, req, resp, err, transcriptMask(c.file, provider, c.token))
	if err != nil {
		// The provider's error goes to a pod that reads untrusted content;
		// it must not carry the key, or credentials in the provider's URL,
		// if the provider or the SDK echoed them.
		msg := maskProvider(err.Error(), provider)
		c.logger.Warn("gateway: step failed", "error", msg)
		refuse(w, http.StatusBadGateway, "upstream_error", msg)
		return
	}
	c.logger.Debug("gateway: step", "model", resp.Model, "input_tokens", resp.Usage.Prompt(), "output_tokens", resp.Usage.Output,
		"cost_usd", resp.CostUSD)
	out, err := model.EncodeChatResponse(c.grant.RunID, resp)
	if err != nil {
		refuse(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(out)
}

// similarReserve is what a stage 4 request is reserved against its run's
// budget before it embeds: every hunk it may send, at four characters a
// token.
const similarReserve = similarHunks * similarHunkChar / 4

// similarCode serves a run stage 4 over the diff of the context pack it
// wrote: the nearest chunks of its repository's index, embedded against
// its budget. The request carries nothing, and what it returns is code of
// the repository the runner has already checked out.
func (g *Gateway) similarCode(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := g.admit(w, r)
	if !ok {
		return
	}
	var diff string
	var changed []string
	var jobID int64
	err := g.Store.WithAccount(ctx, c.grant.AccountID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT p.diff, p.changed_paths, r.river_job_id FROM context_packs p, reviews r
			WHERE p.runner_run_id = $1 AND r.id = $2`, c.grant.RunID, c.grant.ReviewID).Scan(&diff, &changed, &jobID)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		refuse(w, http.StatusBadRequest, "invalid_request", "the run has written no context pack")
		return
	}
	if err != nil {
		c.logger.Error("gateway: context pack not read", "error", err)
		refuse(w, http.StatusInternalServerError, "server_error", "the run's context pack could not be read")
		return
	}
	if !g.reserve(ctx, w, c, similarReserve) {
		return
	}
	chunks, tokens, err := g.similar(ctx, g.Embedders, c.file, similarRequest{
		account: c.account, repositoryID: c.grant.RepositoryID, reviewID: c.grant.ReviewID, jobID: jobID,
		slots: c.file.Settings(c.account, "").Limits.Concurrency, diff: diff, changed: changed,
	}, c.logger)
	cctx, cancel := detach(ctx)
	defer cancel()
	if cerr := g.Store.ChargeGatewayToken(cctx, c.token, tokens-similarReserve); cerr != nil {
		c.logger.Error("gateway: similar code not charged", "error", cerr)
	}
	if err != nil {
		// The embedder's error may carry its key or URL; the runner only
		// learns that the stage failed.
		c.logger.Warn("gateway: similar code failed", "error", err)
		refuse(w, http.StatusBadGateway, "upstream_error", "similar-code retrieval failed")
		return
	}
	if chunks == nil {
		chunks = []contextpack.Chunk{}
	}
	out, err := json.Marshal(similarResponse{Chunks: chunks})
	if err != nil {
		refuse(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(out)
}

// similarResponse is the body /v1/similar answers with.
type similarResponse struct {
	Chunks []contextpack.Chunk `json:"chunks"`
}

// monthCapped says why the account may not take another step this month,
// or "". A step spends tokens, not a review, so only the month's cap
// applies.
func (g *Gateway) monthCapped(ctx context.Context, file *configfile.File, account *configfile.Account) (string, error) {
	limits := file.Settings(account, "").Limits
	return capReached(ctx, g.Store, account.ID(), configfile.Limits{TokensPerMonth: limits.TokensPerMonth})
}

// charge settles a step's reservation: an answered step's actual spend
// replaces it and is recorded against the run's review, where the caps
// count it; a failed step is refunded. The two writes are independent, so
// a failed usage row still leaves the run's budget charged.
func (g *Gateway) charge(
	ctx context.Context, grant store.GatewayGrant, token string, reserved int64, resp model.StepResponse, answered bool,
) error {
	ctx, cancel := detach(ctx)
	defer cancel()
	if !answered {
		return g.Store.ChargeGatewayToken(ctx, token, -reserved)
	}
	spent := resp.Usage.Prompt() + resp.Usage.Output
	budgetErr := g.Store.ChargeGatewayToken(ctx, token, spent-reserved)
	usageErr := g.Store.WithAccount(ctx, grant.AccountID, func(tx pgx.Tx) error {
		return insertUsage(ctx, tx, usageRow{
			accountID: grant.AccountID, repositoryID: grant.RepositoryID, reviewID: grant.ReviewID, role: roleReview, model: resp.Model,
			upstream: resp.Upstream, input: resp.Usage.Prompt(), output: resp.Usage.Output, costUSD: resp.CostUSD,
		})
	})
	return errors.Join(budgetErr, usageErr)
}

// maskProvider removes a provider's key, and any credentials in its base
// URL, from text bound for a runner.
func maskProvider(text string, p configfile.Provider) string {
	for _, s := range providerSecrets(p) {
		if s != "" {
			text = strings.ReplaceAll(text, s, "***")
		}
	}
	return text
}

// providerSecrets are a provider's key and the credentials in its base
// URL, whole and the password alone; some may be empty.
func providerSecrets(p configfile.Provider) []string {
	secrets := []string{p.APIKeyValue().Value()}
	if u, err := url.Parse(p.BaseURL); err == nil && u.User != nil {
		secrets = append(secrets, u.User.String())
		if pw, ok := u.User.Password(); ok {
			secrets = append(secrets, pw)
		}
	}
	return secrets
}
