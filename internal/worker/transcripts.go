package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/model"
	"github.com/home-operations/kritik/internal/store"
	"github.com/home-operations/kritik/internal/transcript"
)

// transcriptTimeout bounds recording one model call. Recording is best
// effort: the transcript view loses a turn rather than a review, or a
// runner's step, waiting on it.
const transcriptTimeout = 2 * time.Second

// transcriptMask masks, in text bound for model_calls, the provider's key
// and URL credentials, every egress credential, and extra (a run token,
// say), longest first so one secret containing another is masked whole.
// Tool input and schemas are raw JSON, where a secret appears escaped, so
// each secret's JSON-escaped forms are masked too.
func transcriptMask(f *configfile.File, p configfile.Provider, extra ...string) func(string) string {
	// Every non-empty secret is masked however short: a very short one
	// garbles the transcript, which is better than leaking it.
	plain := append(providerSecrets(p), extra...)
	for _, cred := range f.EgressRules().Credentials {
		plain = append(plain, cred)
		if _, token, ok := strings.Cut(cred, " "); ok {
			plain = append(plain, token)
		}
	}
	var secrets []string
	for _, s := range plain {
		if s != "" {
			secrets = append(secrets, s)
			secrets = append(secrets, jsonEscaped(s)...)
		}
	}
	slices.SortFunc(secrets, func(a, b string) int { return len(b) - len(a) })
	secrets = slices.Compact(secrets)
	return func(text string) string {
		for _, s := range secrets {
			text = strings.ReplaceAll(text, s, "***")
		}
		return text
	}
}

// jsonEscaped are the forms s takes inside a JSON string, with and without
// HTML escaping, where they differ from s.
func jsonEscaped(s string) []string {
	var out []string
	for _, html := range []bool{true, false} {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(html)
		_ = enc.Encode(s) // cannot fail for a string
		if e := strings.TrimSuffix(strings.TrimSuffix(buf.String(), "\n"), `"`)[1:]; e != s {
			out = append(out, e)
		}
	}
	return out
}

// recordModelCall records one model call for the transcript view: c
// names what made the call and how long it took, req, resp and stepErr are
// the call itself. An agent step is stored as a delta against what its run
// has recorded, read in the same transaction as the insert. Everything is
// masked before it is encoded. A failure is logged and counted, never
// returned.
func (b *Base) recordModelCall(
	ctx context.Context, logger *slog.Logger, c store.ModelCall, req model.StepRequest, resp model.StepResponse, stepErr error,
	mask func(string) string,
) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), transcriptTimeout)
	defer cancel()
	c.Model, c.Upstream, c.Stop, c.Usage, c.CostUSD = resp.Model, resp.Upstream, resp.Stop, resp.Usage, resp.CostUSD
	if c.Model == "" {
		c.Model = req.Model
	}
	if stepErr != nil {
		c.Error = mask(stepErr.Error())
	}
	err := b.Store.WithTenant(ctx, c.TenantID, func(tx pgx.Tx) error {
		var prev transcript.State
		if c.Kind == store.ModelCallAgentStep {
			var err error
			if prev, c.Step, err = store.AgentState(ctx, tx, c.RunnerRunID); err != nil {
				return err
			}
		}
		row := transcript.Delta(prev, req, mask)
		row.Response = transcript.NewResponse(resp, mask)
		c.Row = row.Encode()
		return store.InsertModelCall(ctx, tx, c)
	})
	outcome := "ok"
	if err != nil {
		outcome = "error"
		logger.Warn("model call not recorded", "kind", c.Kind, "error", err)
	}
	b.Metrics.TranscriptWrite(string(c.Kind), outcome)
}

// onStep is a model.Structured OnStep that records each step as c.
func (b *Base) onStep(
	ctx context.Context, logger *slog.Logger, c store.ModelCall, mask func(string) string,
) func(model.StepRequest, model.StepResponse, error, time.Duration) {
	return func(req model.StepRequest, resp model.StepResponse, err error, d time.Duration) {
		c.Duration = d
		b.recordModelCall(ctx, logger, c, req, resp, err, mask)
	}
}
