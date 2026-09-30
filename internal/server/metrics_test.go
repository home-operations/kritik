package server

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestConfigErrorGauge(t *testing.T) {
	reg := prometheus.NewRegistry()
	g := NewConfigErrorGauge(reg)
	if v := testutil.ToFloat64(g.g); v != 0 {
		t.Fatalf("gauge before any error = %v, want 0", v)
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
			g.Set(st.failing)
			if v := testutil.ToFloat64(g.g); v != st.want {
				t.Fatalf("gauge = %v, want %v", v, st.want)
			}
		})
	}
	var nilGauge *ConfigErrorGauge
	nilGauge.Set(true)
}
