package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/agent"
	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/model"
	"github.com/home-operations/kritik/internal/store"
)

// Gateway serves the worker's gateway listener: the egress proxy runner
// pods reach the outside through (ADR-0008), and the model endpoint an
// agentic runner calls with its run token (ADR-0004). No provider key
// enters a runner pod: the gateway reserves each step against the run's
// budget and checks the tenant's monthly cap, answers it through the
// tenant's provider, and records what it spent where the caps see it.
type Gateway struct {
	Base
	// Proxy serves CONNECT and absolute-URI requests.
	Proxy    http.Handler
	Steppers *Completers
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
var maxStepOutput = agent.Limits{}.WithDefaults().MaxOutputTokensPerStep

// ServeHTTP implements http.Handler.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodConnect || r.URL.IsAbs():
		g.Proxy.ServeHTTP(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
		g.chat(w, r)
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

func (g *Gateway) chat(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	grant, err := g.Store.LookupGatewayToken(ctx, token)
	if errors.Is(err, store.ErrGatewayToken) {
		refuse(w, http.StatusUnauthorized, "invalid_token", "the run token is not valid")
		return
	}
	if err != nil {
		g.Logger.Error("gateway: token lookup failed", "error", err)
		refuse(w, http.StatusInternalServerError, "server_error", "the run token could not be checked")
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
	file := g.Current.Get()
	tenant := tenantByID(file, grant.TenantID)
	if tenant == nil {
		refuse(w, http.StatusForbidden, "invalid_token", "the run's tenant is not in the configuration")
		return
	}
	logger := g.Logger.With("tenant", tenant.Slug, "run", short(grant.RunID))
	capped, err := g.monthCapped(ctx, file, tenant)
	if err != nil {
		logger.Error("gateway: caps not read", "error", err)
		refuse(w, http.StatusInternalServerError, "server_error", "the caps could not be checked")
		return
	}
	if capped != "" {
		logger.Info("gateway: step refused", "reason", capped)
		refuse(w, http.StatusTooManyRequests, model.BudgetCode, capped)
		return
	}

	ref := configfile.ModelRef(grant.Model)
	provider := file.Providers[ref.Provider()]
	stepper, err := g.Steppers.Stepper(file, ref.Provider())
	if err != nil {
		logger.Error("gateway: no model adapter", "error", err)
		refuse(w, http.StatusInternalServerError, "server_error", "the run's model is not configured")
		return
	}
	req.Model, req.Fallbacks = ref.Model(), nil
	if fb := configfile.ModelRef(grant.Fallback); fb != "" && fb.Provider() == ref.Provider() {
		req.Fallbacks = []string{fb.Model()}
	}
	if req.MaxTokens <= 0 || req.MaxTokens > maxStepOutput {
		req.MaxTokens = maxStepOutput
	}
	// The step is reserved before it runs, its prompt estimated at four
	// characters a token of the request, so concurrent steps cannot all
	// pass a budget one of them spends; its actual spend replaces the
	// estimate once the provider answers.
	reserved := int64(len(body))/4 + req.MaxTokens
	ok, err := g.Store.ReserveGatewayTokens(ctx, token, reserved)
	if err != nil {
		logger.Error("gateway: step not reserved", "error", err)
		refuse(w, http.StatusInternalServerError, "server_error", "the run's budget could not be checked")
		return
	}
	if !ok {
		reason := fmt.Sprintf("the run's budget of %d tokens is spent", grant.Budget)
		logger.Info("gateway: step refused", "reason", reason)
		refuse(w, http.StatusTooManyRequests, model.BudgetCode, reason)
		return
	}
	start := time.Now()
	resp, err := stepper.Step(ctx, req)
	took := time.Since(start)
	g.Metrics.ModelCall(tenant.Slug, grant.Model, roleReview, callOutcome(err), resp.Usage.Prompt(), resp.Usage.CacheRead,
		resp.Usage.Output, resp.CostUSD)
	if cerr := g.charge(ctx, grant, token, reserved, resp, err == nil); cerr != nil {
		// A step that was answered is paid for either way; the run still
		// gets the answer.
		logger.Error("gateway: step not charged", "error", cerr)
	}
	// Recorded before the runner gets its answer, so the next step's delta
	// is taken against this one; recordModelCall bounds how long it waits.
	g.recordModelCall(ctx, logger, store.ModelCall{
		TenantID: grant.TenantID, ReviewID: grant.ReviewID, RunnerRunID: grant.RunID, Kind: store.ModelCallAgentStep, Duration: took,
	}, req, resp, err, transcriptMask(file, provider, token))
	if err != nil {
		// The provider's error goes to a pod that reads untrusted content;
		// it must not carry the key, or credentials in the provider's URL,
		// if the provider or the SDK echoed them.
		msg := maskProvider(err.Error(), provider)
		logger.Warn("gateway: step failed", "error", msg)
		refuse(w, http.StatusBadGateway, "upstream_error", msg)
		return
	}
	logger.Debug("gateway: step", "model", resp.Model, "input_tokens", resp.Usage.Prompt(), "output_tokens", resp.Usage.Output,
		"cost_usd", resp.CostUSD)
	out, err := model.EncodeChatResponse(grant.RunID, resp)
	if err != nil {
		refuse(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(out)
}

// monthCapped says why the tenant may not take another step this month,
// or "".
func (g *Gateway) monthCapped(ctx context.Context, file *configfile.File, tenant *configfile.Tenant) (string, error) {
	limits := file.Settings(tenant, "").Limits
	if limits.TokensPerMonth <= 0 {
		return "", nil
	}
	var spent int64
	err := g.Store.WithTenant(ctx, tenant.ID(), func(tx pgx.Tx) error {
		var err error
		spent, err = monthTokens(ctx, tx)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("worker: read month's tokens: %w", err)
	}
	if spent >= limits.TokensPerMonth {
		return fmt.Sprintf("tokensPerMonth (%d) reached", limits.TokensPerMonth), nil
	}
	return "", nil
}

// charge settles a step's reservation: an answered step's actual spend
// replaces it and is recorded against the run's review, where the caps
// count it; a failed step is refunded. The two writes are independent, so
// a failed usage row still leaves the run's budget charged.
func (g *Gateway) charge(
	ctx context.Context, grant store.GatewayGrant, token string, reserved int64, resp model.StepResponse, answered bool,
) error {
	ctx = context.WithoutCancel(ctx)
	if !answered {
		return g.Store.ChargeGatewayToken(ctx, token, -reserved)
	}
	spent := resp.Usage.Prompt() + resp.Usage.Output
	budgetErr := g.Store.ChargeGatewayToken(ctx, token, spent-reserved)
	usageErr := g.Store.WithTenant(ctx, grant.TenantID, func(tx pgx.Tx) error {
		return insertUsage(ctx, tx, reviewUsage{
			tenantID: grant.TenantID, repositoryID: grant.RepositoryID, reviewID: grant.ReviewID, role: roleReview, model: resp.Model,
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
