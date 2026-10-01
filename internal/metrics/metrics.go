// Package metrics is every Prometheus series kritik exports beyond the Go
// runtime. One Metrics value is registered per process and shared by its
// parts; a nil *Metrics records nothing, so tests need not register one.
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics holds the collectors. Labels are bounded by configuration
// (accounts, connections, models) or by fixed vocabularies (status,
// stage, outcome); nothing per PR or per commit is ever a label.
type Metrics struct {
	webhooks       *prometheus.CounterVec
	polls          *prometheus.CounterVec
	polled         *prometheus.CounterVec
	reviews        *prometheus.CounterVec
	reviewDuration *prometheus.HistogramVec
	followups      *prometheus.CounterVec
	findings       *prometheus.CounterVec
	indexRuns      *prometheus.CounterVec
	indexChunks    *prometheus.CounterVec
	contextChunks  *prometheus.CounterVec
	runnerRuns     *prometheus.CounterVec
	runnerDuration *prometheus.HistogramVec
	leaseWait      *prometheus.HistogramVec
	reviewSnoozes  *prometheus.CounterVec
	jobsRescued    *prometheus.CounterVec
	modelCalls     *prometheus.CounterVec
	modelTokens    *prometheus.CounterVec
	modelCost      *prometheus.CounterVec
	egress         *prometheus.CounterVec
	transcripts    *prometheus.CounterVec
	leader         prometheus.Gauge
}

// Label names shared across series.
const (
	lblAccount    = "account"
	lblModel      = "model"
	lblRole       = "role"
	lblOutcome    = "outcome"
	lblConnection = "connection"
	lblKind       = "kind"
)

// New registers the collectors on reg.
func New(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		webhooks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kritik_webhooks_total", Help: "Webhook deliveries by connection and what became of them.",
		}, []string{lblConnection, lblOutcome}),
		polls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kritik_polls_total", Help: "Backstop polls per connection, by outcome (ok, error).",
		}, []string{lblConnection, lblOutcome}),
		polled: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kritik_polled_pull_requests_total", Help: "Open pull requests the backstop poll handed to ingest.",
		}, []string{lblConnection}),
		reviews: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kritik_reviews_total", Help: "Reviews finished, by terminal status.",
		}, []string{lblAccount, "status"}),
		egress: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kritik_egress_requests_total",
			Help: "Requests runner pods made through the gateway, by kind (connect, http) and outcome (allowed, refused, error).",
		}, []string{lblKind, lblOutcome}),
		leader: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "kritik_leader", Help: "1 while this replica holds the leader lock, else 0.",
		}),
		transcripts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kritik_transcript_writes_total",
			Help: "Model calls recorded for the transcript view, by kind (agent_step, followup) and outcome (ok, error).",
		}, []string{lblKind, lblOutcome}),
		reviewDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "kritik_review_duration_seconds", Help: "Wall time of a review job from pickup to terminal status.",
			Buckets: []float64{5, 10, 20, 30, 60, 120, 300, 600, 900},
		}, []string{lblAccount}),
		followups: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kritik_followups_total", Help: "Follow-up mentions handled, by outcome: answered, limited, ignored, failed.",
		}, []string{lblAccount, lblOutcome}),
		findings: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kritik_findings_total", Help: "Findings posted, by severity.",
		}, []string{lblAccount, "severity"}),
		indexRuns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kritik_index_runs_total", Help: "Index runs finished, by mode and status.",
		}, []string{lblAccount, "mode", "status"}),
		indexChunks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kritik_index_chunks_total", Help: "Chunks embedded into the index.",
		}, []string{lblAccount}),
		contextChunks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kritik_context_chunks_total",
			Help: "Context chunks a review's prompt was given, by stage (overlay, definition, caller, similar).",
		}, []string{lblAccount, "stage"}),
		runnerRuns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kritik_runner_runs_total", Help: "Runner Jobs finished, by kind and outcome.",
		}, []string{lblAccount, "kind", lblOutcome}),
		runnerDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "kritik_runner_duration_seconds", Help: "Runner Job time from start to finish.",
			Buckets: []float64{2, 5, 10, 20, 30, 60, 120, 300, 600, 900},
		}, []string{"kind"}),
		leaseWait: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "kritik_lease_wait_seconds", Help: "Time spent waiting for a model concurrency slot.",
			Buckets: []float64{0.01, 0.1, 1, 5, 15, 30, 60, 120, 300},
		}, []string{lblAccount, lblModel}),
		reviewSnoozes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kritik_review_snoozes_total", Help: "Reviews put back on the queue because every model slot was held.",
		}, []string{lblAccount, lblModel}),
		jobsRescued: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kritik_jobs_rescued_total", Help: "Jobs of a dead replica handed back to the queue by the leader, by kind and state.",
		}, []string{lblKind, "state"}),
		modelCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kritik_model_calls_total", Help: "Model calls, by the model that answered, role and outcome.",
		}, []string{lblAccount, lblModel, lblRole, lblOutcome}),
		modelTokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kritik_model_tokens_total",
			Help: "Tokens spent, by role and direction (input, cached, output); " +
				"cached is the part of input the provider served from its prompt cache.",
		}, []string{lblAccount, lblModel, lblRole, "direction"}),
		modelCost: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kritik_model_cost_usd_total", Help: "Provider-reported cost in US dollars, by role.",
		}, []string{lblAccount, lblModel, lblRole}),
	}
	reg.MustRegister(m.webhooks, m.polls, m.polled, m.reviews, m.reviewDuration, m.followups, m.findings,
		m.indexRuns, m.indexChunks, m.contextChunks,
		m.runnerRuns, m.runnerDuration, m.leaseWait, m.reviewSnoozes, m.jobsRescued, m.modelCalls, m.modelTokens, m.modelCost, m.egress,
		m.transcripts,
		m.leader)
	return m
}

// Webhook counts one delivery.
func (m *Metrics) Webhook(connection, outcome string) {
	if m != nil {
		m.webhooks.WithLabelValues(connection, outcome).Inc()
	}
}

// Poll counts one connection poll and the pull requests it handed on.
func (m *Metrics) Poll(connection, outcome string, pullRequests int) {
	if m != nil {
		m.polls.WithLabelValues(connection, outcome).Inc()
		if pullRequests > 0 {
			m.polled.WithLabelValues(connection).Add(float64(pullRequests))
		}
	}
}

// Review counts a finished review and its duration.
func (m *Metrics) Review(account, status string, took time.Duration) {
	if m != nil {
		m.reviews.WithLabelValues(account, status).Inc()
		m.reviewDuration.WithLabelValues(account).Observe(took.Seconds())
	}
}

// FollowUp counts one handled mention.
func (m *Metrics) FollowUp(account, outcome string) {
	if m != nil {
		m.followups.WithLabelValues(account, outcome).Inc()
	}
}

// Findings counts posted findings of one severity.
func (m *Metrics) Findings(account, severity string, n int) {
	if m != nil && n > 0 {
		m.findings.WithLabelValues(account, severity).Add(float64(n))
	}
}

// IndexRun counts a finished index run and the chunks it embedded.
func (m *Metrics) IndexRun(account, mode, status string, chunks int) {
	if m != nil {
		m.indexRuns.WithLabelValues(account, mode, status).Inc()
		if chunks > 0 {
			m.indexChunks.WithLabelValues(account).Add(float64(chunks))
		}
	}
}

// ContextChunks counts the chunks one stage gave a review's prompt.
func (m *Metrics) ContextChunks(account, stage string, n int) {
	if m != nil && n > 0 {
		m.contextChunks.WithLabelValues(account, stage).Add(float64(n))
	}
}

// RunnerRun counts a finished runner Job; took is zero when unknown.
func (m *Metrics) RunnerRun(account, kind, outcome string, took time.Duration) {
	if m != nil {
		m.runnerRuns.WithLabelValues(account, kind, outcome).Inc()
		if took > 0 {
			m.runnerDuration.WithLabelValues(kind).Observe(took.Seconds())
		}
	}
}

// LeaseWait records how long a lease took to acquire.
func (m *Metrics) LeaseWait(account, model string, took time.Duration) {
	if m != nil {
		m.leaseWait.WithLabelValues(account, model).Observe(took.Seconds())
	}
}

// ReviewSnoozed counts a review put back on the queue to wait for a model
// slot.
func (m *Metrics) ReviewSnoozed(account, model string) {
	if m != nil {
		m.reviewSnoozes.WithLabelValues(account, model).Inc()
	}
}

// JobRescued counts a job of a dead replica handed back to the queue by
// the leader, by job kind and the state it was handed back in.
func (m *Metrics) JobRescued(kind, state string) {
	if m != nil {
		m.jobsRescued.WithLabelValues(kind, state).Inc()
	}
}

// ModelCall records one call: outcome is ok or error; tokens and cost are
// added only for ok. cachedTokens is the part of inputTokens the provider
// served from its prompt cache.
func (m *Metrics) ModelCall(account, model, role, outcome string, inputTokens, cachedTokens, outputTokens int64, costUSD float64) {
	if m == nil {
		return
	}
	m.modelCalls.WithLabelValues(account, model, role, outcome).Inc()
	if outcome != "ok" {
		return
	}
	if inputTokens > 0 {
		m.modelTokens.WithLabelValues(account, model, role, "input").Add(float64(inputTokens))
	}
	if cachedTokens > 0 {
		m.modelTokens.WithLabelValues(account, model, role, "cached").Add(float64(cachedTokens))
	}
	if outputTokens > 0 {
		m.modelTokens.WithLabelValues(account, model, role, "output").Add(float64(outputTokens))
	}
	if costUSD > 0 {
		m.modelCost.WithLabelValues(account, model, role).Add(costUSD)
	}
}

// Egress counts one gateway request.
func (m *Metrics) Egress(kind, outcome string) {
	if m == nil {
		return
	}
	m.egress.WithLabelValues(kind, outcome).Inc()
}

// Leading records whether this replica holds the leader lock.
func (m *Metrics) Leading(held bool) {
	if m == nil {
		return
	}
	v := 0.0
	if held {
		v = 1
	}
	m.leader.Set(v)
}

// TranscriptWrite counts one model call recorded, or not, for the
// transcript view.
func (m *Metrics) TranscriptWrite(kind, outcome string) {
	if m != nil {
		m.transcripts.WithLabelValues(kind, outcome).Inc()
	}
}
