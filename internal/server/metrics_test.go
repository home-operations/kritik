package server

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestConfigErrorGauge(t *testing.T) {
	reg := prometheus.NewRegistry()
	g := NewConfigErrorGauge(reg)
	value := func(stage ConfigErrorStage) float64 { return testutil.ToFloat64(g.g.WithLabelValues(string(stage))) }
	if n := testutil.CollectAndCount(g.g); n != 1 {
		t.Fatalf("series before any error = %d, want the apply stage at 0", n)
	}
	for _, st := range []struct {
		name    string
		failing bool
		want    float64
	}{
		{"apply fails", true, 1},
		{"apply recovers", false, 0},
	} {
		t.Run(st.name, func(t *testing.T) {
			g.Set(ConfigErrorApply, st.failing)
			if v := value(ConfigErrorApply); v != st.want {
				t.Fatalf("apply = %v, want %v", v, st.want)
			}
		})
	}
	var nilGauge *ConfigErrorGauge
	nilGauge.Set(ConfigErrorApply, true)
	if !ConfigErrorApply.Valid() || ConfigErrorStage("load").Valid() {
		t.Fatal("Valid is wrong")
	}
}
