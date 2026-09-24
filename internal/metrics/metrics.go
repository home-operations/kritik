// Package metrics is every Prometheus series kritik exports beyond the Go
// runtime. One Metrics value is registered per process and shared by the
// roles; a nil *Metrics records nothing, so tests need not register one.
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics holds the collectors. Labels are bounded by configuration
// (tenants, installations, models) or by fixed vocabularies (status,
// stage, outcome); nothing per PR or per commit is ever a label.
type Metrics struct {
	webhooks       *prometheus.CounterVec
	polls          *prometheus.CounterVec
	polled         *prometheus.CounterVec
	reviews        *prometheus.CounterVec
	reviewDuration *prometheus.HistogramVec
	followups      *prometheus.CounterVec
	findings       *prometheus.CounterVec
	contextChunks  *prometheus.CounterVec
	indexRuns      *prometheus.CounterVec
	indexChunks    *prometheus.CounterVec
	runnerRuns     *prometheus.CounterVec
	runnerDuration *prometheus.HistogramVec
	leaseWait      *prometheus.HistogramVec
	modelCalls     *prometheus.CounterVec
	modelTokens    *prometheus.CounterVec
	modelCost      *prometheus.CounterVec
}

// Label names shared across series.
const (
	lblTenant       = "tenant"
	lblModel        = "model"
	lblRole         = "role"
	lblOutcome      = "outcome"
	lblInstallation = "installation"
)

// New registers the collectors on reg.
func New(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		webhooks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kritik_webhooks_total", Help: "Webhook deliveries by installation and what became of them.",
		}, []string{lblInstallation, lblOutcome}),
		polls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kritik_polls_total", Help: "Backstop polls per installation, by outcome (ok, error).",
		}, []string{lblInstallation, lblOutcome}),
		polled: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kritik_polled_pull_requests_total", Help: "Open pull requests the backstop poll handed to ingest.",
		}, []string{lblInstallation}),
		reviews: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kritik_reviews_total", Help: "Reviews finished, by terminal status.",
		}, []string{lblTenant, "status"}),
		reviewDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "kritik_review_duration_seconds", Help: "Wall time of a review job from pickup to terminal status.",
			Buckets: []float64{5, 10, 20, 30, 60, 120, 300, 600, 900},
		}, []string{lblTenant}),
		followups: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kritik_followups_total", Help: "Follow-up mentions handled, by outcome: answered, limited, ignored, failed.",
		}, []string{lblTenant, lblOutcome}),
		findings: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kritik_findings_total", Help: "Findings posted, by severity.",
		}, []string{lblTenant, "severity"}),
		contextChunks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kritik_context_chunks_total", Help: "Context chunks put in front of the model, by stage.",
		}, []string{lblTenant, "stage"}),
		indexRuns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kritik_index_runs_total", Help: "Index runs finished, by mode and status.",
		}, []string{lblTenant, "mode", "status"}),
		indexChunks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kritik_index_chunks_total", Help: "Chunks embedded into the index.",
		}, []string{lblTenant}),
		runnerRuns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kritik_runner_runs_total", Help: "Runner Jobs finished, by kind and outcome.",
		}, []string{lblTenant, "kind", lblOutcome}),
		runnerDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "kritik_runner_duration_seconds", Help: "Runner Job time from start to finish.",
			Buckets: []float64{2, 5, 10, 20, 30, 60, 120, 300, 600, 900},
		}, []string{"kind"}),
		leaseWait: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "kritik_lease_wait_seconds", Help: "Time spent waiting for a model concurrency slot.",
			Buckets: []float64{0.01, 0.1, 1, 5, 15, 30, 60, 120, 300},
		}, []string{lblTenant, lblModel}),
		modelCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kritik_model_calls_total", Help: "Model calls, by role and outcome.",
		}, []string{lblTenant, lblModel, lblRole, lblOutcome}),
		modelTokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kritik_model_tokens_total",
			Help: "Tokens spent, by role and direction (input, cached, output); " +
				"cached is the part of input the provider served from its prompt cache.",
		}, []string{lblTenant, lblModel, lblRole, "direction"}),
		modelCost: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kritik_model_cost_usd_total", Help: "Provider-reported cost in US dollars, by role.",
		}, []string{lblTenant, lblModel, lblRole}),
	}
	reg.MustRegister(m.webhooks, m.polls, m.polled, m.reviews, m.reviewDuration, m.followups, m.findings,
		m.contextChunks, m.indexRuns, m.indexChunks,
		m.runnerRuns, m.runnerDuration, m.leaseWait, m.modelCalls, m.modelTokens, m.modelCost)
	return m
}

// Webhook counts one delivery.
func (m *Metrics) Webhook(installation, outcome string) {
	if m != nil {
		m.webhooks.WithLabelValues(installation, outcome).Inc()
	}
}

// Poll counts one installation poll and the pull requests it handed on.
func (m *Metrics) Poll(installation, outcome string, pullRequests int) {
	if m != nil {
		m.polls.WithLabelValues(installation, outcome).Inc()
		if pullRequests > 0 {
			m.polled.WithLabelValues(installation).Add(float64(pullRequests))
		}
	}
}

// Review counts a finished review and its duration.
func (m *Metrics) Review(tenant, status string, took time.Duration) {
	if m != nil {
		m.reviews.WithLabelValues(tenant, status).Inc()
		m.reviewDuration.WithLabelValues(tenant).Observe(took.Seconds())
	}
}

// FollowUp counts one handled mention.
func (m *Metrics) FollowUp(tenant, outcome string) {
	if m != nil {
		m.followups.WithLabelValues(tenant, outcome).Inc()
	}
}

// Findings counts posted findings of one severity.
func (m *Metrics) Findings(tenant, severity string, n int) {
	if m != nil && n > 0 {
		m.findings.WithLabelValues(tenant, severity).Add(float64(n))
	}
}

// ContextChunks counts chunks of one stage put in a prompt.
func (m *Metrics) ContextChunks(tenant, stage string, n int) {
	if m != nil && n > 0 {
		m.contextChunks.WithLabelValues(tenant, stage).Add(float64(n))
	}
}

// IndexRun counts a finished index run and the chunks it embedded.
func (m *Metrics) IndexRun(tenant, mode, status string, chunks int) {
	if m != nil {
		m.indexRuns.WithLabelValues(tenant, mode, status).Inc()
		if chunks > 0 {
			m.indexChunks.WithLabelValues(tenant).Add(float64(chunks))
		}
	}
}

// RunnerRun counts a finished runner Job; took is zero when unknown.
func (m *Metrics) RunnerRun(tenant, kind, outcome string, took time.Duration) {
	if m != nil {
		m.runnerRuns.WithLabelValues(tenant, kind, outcome).Inc()
		if took > 0 {
			m.runnerDuration.WithLabelValues(kind).Observe(took.Seconds())
		}
	}
}

// LeaseWait records how long a lease took to acquire.
func (m *Metrics) LeaseWait(tenant, model string, took time.Duration) {
	if m != nil {
		m.leaseWait.WithLabelValues(tenant, model).Observe(took.Seconds())
	}
}

// ModelCall records one call: outcome is ok or error; tokens and cost are
// added only for ok. cachedTokens is the part of inputTokens the provider
// served from its prompt cache.
func (m *Metrics) ModelCall(tenant, model, role, outcome string, inputTokens, cachedTokens, outputTokens int64, costUSD float64) {
	if m == nil {
		return
	}
	m.modelCalls.WithLabelValues(tenant, model, role, outcome).Inc()
	if outcome != "ok" {
		return
	}
	if inputTokens > 0 {
		m.modelTokens.WithLabelValues(tenant, model, role, "input").Add(float64(inputTokens))
	}
	if cachedTokens > 0 {
		m.modelTokens.WithLabelValues(tenant, model, role, "cached").Add(float64(cachedTokens))
	}
	if outputTokens > 0 {
		m.modelTokens.WithLabelValues(tenant, model, role, "output").Add(float64(outputTokens))
	}
	if costUSD > 0 {
		m.modelCost.WithLabelValues(tenant, model, role).Add(costUSD)
	}
}
