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
	if n := testutil.CollectAndCount(g.g); n != 2 {
		t.Fatalf("series before any error = %d, want both stages at 0", n)
	}
	steps := []struct {
		name      string
		stage     ConfigErrorStage
		failing   bool
		wantMerge float64
		wantApply float64
	}{
		{"merge fails", ConfigErrorMerge, true, 1, 0},
		{"apply fails", ConfigErrorApply, true, 1, 1},
		{"merge recovers", ConfigErrorMerge, false, 0, 1},
		{"apply recovers", ConfigErrorApply, false, 0, 0},
	}
	for _, st := range steps {
		t.Run(st.name, func(t *testing.T) {
			g.Set(st.stage, st.failing)
			if m, a := value(ConfigErrorMerge), value(ConfigErrorApply); m != st.wantMerge || a != st.wantApply {
				t.Fatalf("merge=%v apply=%v, want %v %v", m, a, st.wantMerge, st.wantApply)
			}
		})
	}
	var nilGauge *ConfigErrorGauge
	nilGauge.Set(ConfigErrorMerge, true)
	if !ConfigErrorMerge.Valid() || ConfigErrorStage("other").Valid() {
		t.Fatal("Valid is wrong")
	}
}
