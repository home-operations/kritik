// Package gateway is the worker's listener for runner pods: the egress
// proxy they reach the outside through, and the model and similar-code
// endpoints a review's runner calls with its run token. No provider key
// enters a runner pod: the gateway reserves each call against the run's
// budget and checks the account's monthly cap, answers it through the
// account's provider, and records what it spent where the caps see it.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritika/internal/adapter"
	"github.com/home-operations/kritika/internal/agent"
	"github.com/home-operations/kritika/internal/configfile"
	"github.com/home-operations/kritika/internal/metrics"
	"github.com/home-operations/kritika/internal/model"
	"github.com/home-operations/kritika/internal/review"
	"github.com/home-operations/kritika/internal/store"
)

// ModelName is the name a review's runner calls its model by; the gateway
// maps it to the provider model the run was granted.
const ModelName = "review"

// Drain is how long a stopping gateway lets model steps in flight finish.
// A step it cuts is paid for and not recorded, and the runner's retry is
// paid for again, so it covers a long step rather than the usual few
// seconds; the chart's grace period outlasts it.
const Drain = 2 * time.Minute

// Server serves the gateway listener.
type Server struct {
	Store   *store.Store
	Current *configfile.Current
	Logger  *slog.Logger
	// Metrics may be nil.
	Metrics *metrics.Metrics
	// Proxy serves CONNECT and absolute-URI requests.
	Proxy    http.Handler
	Steppers *adapter.Steppers
	// Embedders resolves the instance's embedder for similar code; nil
	// serves none.
	Embedders *adapter.Embedders
}

// maxBody bounds one step's request: the whole conversation so far, every
// tool output in it capped.
const maxBody = 16 << 20

// MaxStepOutput caps the answer to one step, whatever the runner asks: the
// agent loop's own cap.
var MaxStepOutput = agent.DefaultLimits.MaxOutputTokensPerStep

// detachTimeout bounds the writes that settle a step once the provider has
// answered, which must land even if the request's ctx ends.
const detachTimeout = 2 * time.Minute

// detach is ctx without its cancellation, bounded by detachTimeout.
func detach(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), detachTimeout)
}

// ServeHTTP implements http.Handler.
func (g *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
// configuration and checks the account's monthly cap, or refuses r and
// reports false.
func (g *Server) admit(w http.ResponseWriter, r *http.Request) (runCall, bool) {
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
	// A step spends tokens, not a review, so only the month's cap applies.
	limits := file.Settings(account, "").Limits
	capped, err := g.Store.CapReached(ctx, account.ID(), configfile.Limits{TokensPerMonth: limits.TokensPerMonth})
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
func (g *Server) reserve(ctx context.Context, w http.ResponseWriter, c runCall, tokens int64) bool {
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

func (g *Server) chat(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := g.admit(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
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
	if req.Model != ModelName {
		refuse(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("the model is %q, not %q", ModelName, req.Model))
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
	// A run's steps are one conversation, whichever provider answers.
	req.Model, req.Fallbacks, req.Session = ref.Model(), nil, c.grant.RunID
	fb := configfile.ModelRef(c.grant.Fallback)
	if fb != "" && fb.Provider() == ref.Provider() {
		req.Fallbacks = []string{fb.Model()}
	}
	if req.MaxTokens <= 0 || req.MaxTokens > MaxStepOutput {
		req.MaxTokens = MaxStepOutput
	}
	// The step is reserved before it runs, its prompt estimated at four
	// characters a token of the request; its actual spend replaces the
	// estimate once the provider answers.
	reserved := int64(len(body))/4 + req.MaxTokens
	if !g.reserve(ctx, w, c, reserved) {
		return
	}
	start := time.Now()
	resp, attempts, err := step(ctx, stepper, req, provider.Retries, sleep, func(err error) {
		c.logger.Warn("gateway: step failed; trying again", "error", maskProvider(err.Error(), provider))
	})
	if err != nil && ctx.Err() == nil && fb != "" && fb.Provider() != ref.Provider() {
		// The review model's attempts are spent; a fallback on another
		// provider gets the same step, with that provider's retries. The
		// request is provider-neutral, so the conversation carries over.
		if fbStepper, fbProvider, ok := g.fallback(c, fb); ok {
			c.logger.Warn("gateway: step failed on the review model; trying the fallback", "fallback", fb,
				"error", maskProvider(err.Error(), provider))
			req.Model = fb.Model()
			var more int
			resp, more, err = step(ctx, fbStepper, req, fbProvider.Retries, sleep, func(err error) {
				c.logger.Warn("gateway: step failed; trying again", "error", maskProvider(err.Error(), fbProvider))
			})
			attempts += more
			ref, provider = fb, fbProvider
		}
	}
	took := time.Since(start)
	g.Metrics.ModelCall(c.account.Key(), adapter.ServedRef(ref, resp.Model), store.RoleReview, adapter.Outcome(err), resp.Usage.Prompt(),
		resp.Usage.CacheRead, resp.Usage.Output, resp.CostUSD)
	if cerr := g.charge(ctx, c.grant, c.token, reserved, resp, err == nil); cerr != nil {
		// A step that was answered is paid for either way; the run still
		// gets the answer.
		c.logger.Error("gateway: step not charged", "error", cerr)
	}
	// Recorded before the runner gets its answer, so the next step's delta
	// is taken against this one; the recorder bounds how long it waits.
	adapter.Recorder{Store: g.Store, Metrics: g.Metrics}.Record(ctx, c.logger, store.ModelCall{
		AccountID: c.grant.AccountID, ReviewID: c.grant.ReviewID, RunnerRunID: c.grant.RunID, Kind: store.ModelCallAgentStep, Duration: took,
	}, req, resp, err, adapter.Mask(c.file, provider, c.token))
	if err != nil {
		// The provider's error goes to a pod that reads untrusted content;
		// it must not carry the key, or credentials in the provider's URL,
		// if the provider or the SDK echoed them.
		msg := maskProvider(err.Error(), provider)
		c.logger.Warn("gateway: step failed", "error", msg)
		refuse(w, http.StatusBadGateway, "upstream_error", msg)
		return
	}
	c.logger.Debug("gateway: step", "model", resp.Model, "attempts", attempts, "input_tokens", resp.Usage.Prompt(),
		"output_tokens", resp.Usage.Output, "cost_usd", resp.CostUSD)
	out, err := model.EncodeChatResponse(c.grant.RunID, resp)
	if err != nil {
		refuse(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(out)
}

// fallback resolves the adapter and provider of a run's fallback model on
// another provider, or reports false when the configuration no longer has
// it, which the step then does without.
func (g *Server) fallback(c runCall, fb configfile.ModelRef) (model.Stepper, configfile.Provider, bool) {
	stepper, err := g.Steppers.Stepper(c.file, c.account, fb.Provider())
	if err != nil {
		c.logger.Error("gateway: no adapter for the fallback", "fallback", fb, "error", err)
		return nil, configfile.Provider{}, false
	}
	provider, _ := c.file.Provider(c.account, fb.Provider())
	return stepper, provider, true
}

// A step that failed in a way another attempt may not is tried again after
// retryMin, doubled each time up to retryMax.
const (
	retryMin = time.Second
	retryMax = 30 * time.Second
)

// step runs one model step through stepper, and after a transient failure
// (model.Transient) tries again, up to retries more times with backoff, or
// the wait the provider asked for when that is longer, while ctx lives;
// failed reports each failure it tries again after. It returns the last
// answer or error and how many attempts it made. One reservation covers
// them all: the request is the same each time.
func step(
	ctx context.Context, stepper model.Stepper, req model.StepRequest, retries int,
	wait func(context.Context, time.Duration) bool, failed func(error),
) (model.StepResponse, int, error) {
	for attempt := 0; ; attempt++ {
		resp, err := stepper.Step(ctx, req)
		if err == nil || attempt >= retries || ctx.Err() != nil || !model.Transient(err) {
			return resp, attempt + 1, err
		}
		failed(err)
		if !wait(ctx, max(store.Backoff(attempt, retryMin, retryMax), model.RetryAfter(err))) {
			return resp, attempt + 1, err
		}
	}
}

// sleep waits d, or until ctx ends, and reports whether it waited d out.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// charge settles a step's reservation: an answered step's actual spend
// replaces it and is recorded against the run's review, where the caps
// count it; a failed step is refunded. The two writes are independent, so
// a failed usage row still leaves the run's budget charged.
func (g *Server) charge(
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
		return store.InsertUsage(ctx, tx, store.Usage{
			AccountID: grant.AccountID, RepositoryID: grant.RepositoryID, ReviewID: grant.ReviewID, Role: store.RoleReview, Model: resp.Model,
			Upstream: resp.Upstream, Input: resp.Usage.Prompt(), Output: resp.Usage.Output, CostUSD: resp.CostUSD,
		})
	})
	return errors.Join(budgetErr, usageErr)
}

// maskProvider removes a provider's key, and any credentials in its base
// URL, from text bound for a runner.
func maskProvider(text string, p configfile.Provider) string {
	for _, s := range adapter.ProviderSecrets(p) {
		if s != "" {
			text = strings.ReplaceAll(text, s, "***")
		}
	}
	return text
}
