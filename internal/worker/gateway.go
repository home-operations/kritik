package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/model"
	"github.com/home-operations/kritik/internal/store"
)

// StepperSource resolves a configured provider to its model adapter.
type StepperSource interface {
	Stepper(f *configfile.File, name string) (model.Stepper, error)
}

// Gateway serves the worker's gateway listener: the egress proxy runner
// pods reach the outside through (ADR-0008), and the model endpoint an
// agentic runner calls with its run token (ADR-0004). No provider key
// enters a runner pod: the gateway checks the run's budget and the
// tenant's monthly cap before each step, answers it through the tenant's
// provider, and records what it spent where the caps see it.
type Gateway struct {
	Base
	// Proxy serves CONNECT and absolute-URI requests.
	Proxy    http.Handler
	Steppers StepperSource
}

// maxGatewayBody bounds one step's request: the whole conversation so far,
// every tool output in it capped.
const maxGatewayBody = 16 << 20

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

// refuse answers a step with an error the runner does not retry: the
// gateway's own adapter has already retried the provider.
func refuse(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Should-Retry", "false")
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
	if err != nil {
		refuse(w, http.StatusRequestEntityTooLarge, "invalid_request", err.Error())
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
	capped, err := g.capped(ctx, file, tenant, grant)
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
	resp, err := stepper.Step(ctx, req)
	g.Metrics.ModelCall(tenant.Slug, grant.Model, roleReview, callOutcome(err), resp.Usage.Prompt(), resp.Usage.CacheRead,
		resp.Usage.Output, resp.CostUSD)
	if err != nil {
		// The provider's error goes to a pod that reads untrusted content;
		// it must not carry the key if the provider echoed it.
		msg := err.Error()
		if key := provider.APIKeyValue().Value(); key != "" {
			msg = strings.ReplaceAll(msg, key, "***")
		}
		logger.Warn("gateway: step failed", "error", msg)
		refuse(w, http.StatusBadGateway, "upstream_error", msg)
		return
	}
	if err := g.charge(ctx, grant, token, resp); err != nil {
		// The answer is paid for either way; the run still gets it.
		logger.Error("gateway: step not charged", "error", err)
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

// capped says why the run may not take another step, or "": its budget is
// spent, or the tenant's monthly token cap is.
func (g *Gateway) capped(ctx context.Context, file *configfile.File, tenant *configfile.Tenant, grant store.GatewayGrant) (string, error) {
	if grant.Spent >= grant.Budget {
		return fmt.Sprintf("the run's budget of %d tokens is spent", grant.Budget), nil
	}
	limits := file.Settings(tenant, "").Limits
	if limits.TokensPerMonth <= 0 {
		return "", nil
	}
	u, err := readUsage(ctx, g.Store, tenant.ID())
	if err != nil {
		return "", err
	}
	if u.tokens >= limits.TokensPerMonth {
		return fmt.Sprintf("tokensPerMonth (%d) reached", limits.TokensPerMonth), nil
	}
	return "", nil
}

// charge records a step's usage against the run's review, where the caps
// count it, and against the run's budget.
func (g *Gateway) charge(ctx context.Context, grant store.GatewayGrant, token string, resp model.StepResponse) error {
	ctx = context.WithoutCancel(ctx)
	err := g.Store.WithTenant(ctx, grant.TenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO usage
			(tenant_id, repository_id, review_id, role, model, upstream, input_tokens, output_tokens, cost_usd)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			grant.TenantID, grant.RepositoryID, grant.ReviewID, roleReview, resp.Model, resp.Upstream,
			resp.Usage.Prompt(), resp.Usage.Output, resp.CostUSD)
		return err
	})
	if err != nil {
		return fmt.Errorf("worker: insert gateway usage: %w", err)
	}
	return g.Store.ChargeGatewayToken(ctx, token, resp.Usage.Prompt()+resp.Usage.Output)
}
