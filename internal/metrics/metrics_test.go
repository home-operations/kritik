package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestMetricsRecordAndNilIsSafe(t *testing.T) {
	var none *Metrics
	none.Webhook("a", "b")
	none.Review("t", "completed", time.Second)
	none.ModelCall("t", "m", "review", "ok", 1, 0, 1, 0.1)

	reg := prometheus.NewRegistry()
	m := New(reg)
	m.Webhook("onedr0p-github", "enqueued")
	m.Review("onedr0p", "completed", 12*time.Second)
	m.Findings("onedr0p", "warning", 2)
	m.ContextChunks("onedr0p", "similar", 4)
	m.IndexRun("onedr0p", "full", "completed", 565)
	m.RunnerRun("onedr0p", "index", "success", 4*time.Second)
	m.LeaseWait("onedr0p", "openai/gpt-6-sol", 5*time.Millisecond)
	m.ModelCall("onedr0p", "openai/gpt-6-sol", "review", "ok", 1706, 1574, 83, 0.005029)
	m.ModelCall("onedr0p", "openai/gpt-6-sol", "review", "error", 0, 0, 0, 0)

	want := `# HELP kritik_model_tokens_total Tokens spent, by role and direction (input, cached, output); cached is the part of input the provider served from its prompt cache.
# TYPE kritik_model_tokens_total counter
kritik_model_tokens_total{direction="cached",model="openai/gpt-6-sol",role="review",tenant="onedr0p"} 1574
kritik_model_tokens_total{direction="input",model="openai/gpt-6-sol",role="review",tenant="onedr0p"} 1706
kritik_model_tokens_total{direction="output",model="openai/gpt-6-sol",role="review",tenant="onedr0p"} 83
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "kritik_model_tokens_total"); err != nil {
		t.Fatal(err)
	}
	if n := testutil.CollectAndCount(m.modelCalls); n != 2 {
		t.Fatalf("model call series = %d", n)
	}
	if v := testutil.ToFloat64(m.indexChunks.WithLabelValues("onedr0p")); v != 565 {
		t.Fatalf("index chunks = %v", v)
	}
	if v := testutil.ToFloat64(m.modelCost.WithLabelValues("onedr0p", "openai/gpt-6-sol", "review")); v != 0.005029 {
		t.Fatalf("cost = %v", v)
	}
}
