package adapter

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritika/internal/configfile"
	"github.com/home-operations/kritika/internal/metrics"
	"github.com/home-operations/kritika/internal/model"
	"github.com/home-operations/kritika/internal/store"
	"github.com/home-operations/kritika/internal/transcript"
)

// recordTimeout bounds recording one model call. Recording is best effort:
// the transcript view loses a turn rather than a review, or a runner's
// step, waiting on it.
const recordTimeout = 2 * time.Second

// ProviderSecrets are a provider's key and the credentials in its base
// URL, whole and the password alone; some may be empty.
func ProviderSecrets(p configfile.Provider) []string {
	secrets := []string{p.APIKeyValue().Value()}
	if u, err := url.Parse(p.BaseURL); err == nil && u.User != nil {
		secrets = append(secrets, u.User.String())
		if pw, ok := u.User.Password(); ok {
			secrets = append(secrets, pw)
		}
	}
	return secrets
}

// Mask masks, in text bound for model_calls, the provider's key and URL
// credentials, every egress credential, and extra (a run token, say),
// longest first so one secret containing another is masked whole. Tool
// input and schemas are raw JSON, where a secret appears escaped, so each
// secret's JSON-escaped forms are masked too.
func Mask(f *configfile.File, p configfile.Provider, extra ...string) func(string) string {
	// Every non-empty secret is masked however short: a very short one
	// garbles the transcript, which is better than leaking it.
	plain := append(ProviderSecrets(p), extra...)
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
	// Longest first, so no secret is cut by masking one it contains; equal
	// ones adjacent, so Compact drops the repeats.
	slices.SortFunc(secrets, func(a, b string) int { return cmp.Or(cmp.Compare(len(b), len(a)), cmp.Compare(a, b)) })
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

// Recorder writes model calls to the transcript view. Metrics may be nil.
type Recorder struct {
	Store   *store.Store
	Metrics *metrics.Metrics
}

// Record records one model call: c names what made the call and how long
// it took, req, resp and stepErr are the call itself. An agent step is
// stored as a delta against what its run has recorded, read in the same
// transaction as the insert. Everything is masked before it is encoded. A
// failure is logged and counted, never returned.
func (r Recorder) Record(
	ctx context.Context, logger *slog.Logger, c store.ModelCall, req model.StepRequest, resp model.StepResponse, stepErr error,
	mask func(string) string,
) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	c.Model, c.Upstream, c.Stop, c.Usage, c.CostUSD = resp.Model, resp.Upstream, resp.Stop, resp.Usage, resp.CostUSD
	c.Model = cmp.Or(c.Model, req.Model)
	if stepErr != nil {
		c.Error = mask(stepErr.Error())
	}
	err := r.Store.WithAccount(ctx, c.AccountID, func(tx pgx.Tx) error {
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
	r.Metrics.TranscriptWrite(string(c.Kind), outcome)
}

// OnStep is a model.Structured OnStep that records each step as c.
func (r Recorder) OnStep(
	ctx context.Context, logger *slog.Logger, c store.ModelCall, mask func(string) string,
) func(model.StepRequest, model.StepResponse, error, time.Duration) {
	return func(req model.StepRequest, resp model.StepResponse, err error, d time.Duration) {
		c.Duration = d
		r.Record(ctx, logger, c, req, resp, err, mask)
	}
}
