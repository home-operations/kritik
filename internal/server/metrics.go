package server

import "github.com/prometheus/client_golang/prometheus"

// ConfigDriftGauge is 1 while this replica's configuration file differs from
// the one the leader last applied to the store.
type ConfigDriftGauge struct{ g prometheus.Gauge }

// NewConfigDriftGauge registers the gauge on reg.
func NewConfigDriftGauge(reg prometheus.Registerer) *ConfigDriftGauge {
	g := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "kritik_config_drift",
		Help: "1 when this replica's configuration file differs from the one the leader applied, else 0.",
	})
	reg.MustRegister(g)
	return &ConfigDriftGauge{g: g}
}

// Set records whether drift is present.
func (c *ConfigDriftGauge) Set(drifting bool) {
	if drifting {
		c.g.Set(1)
		return
	}
	c.g.Set(0)
}

// ConfigErrorStage is where producing or applying the configuration failed.
type ConfigErrorStage string

// Configuration error stages.
const (
	// ConfigErrorMerge is the file or a dashboard tenant failing to parse,
	// resolve or merge; the last good snapshot stays live.
	ConfigErrorMerge ConfigErrorStage = "merge"
	// ConfigErrorApply is the leader's store refusing the merged snapshot;
	// the last applied state stays live.
	ConfigErrorApply ConfigErrorStage = "apply"
)

// Valid reports whether s is a stage.
func (s ConfigErrorStage) Valid() bool { return s == ConfigErrorMerge || s == ConfigErrorApply }

// ConfigErrorGauge is 1 for a stage while its latest attempt failed.
type ConfigErrorGauge struct{ g *prometheus.GaugeVec }

// NewConfigErrorGauge registers the gauge on reg, with every stage at 0.
func NewConfigErrorGauge(reg prometheus.Registerer) *ConfigErrorGauge {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kritik_config_error",
		Help: "1 while the latest attempt at a stage (merge, apply) of loading the configuration failed, else 0.",
	}, []string{"stage"})
	reg.MustRegister(g)
	for _, s := range []ConfigErrorStage{ConfigErrorMerge, ConfigErrorApply} {
		g.WithLabelValues(string(s)).Set(0)
	}
	return &ConfigErrorGauge{g: g}
}

// Set records whether stage is failing. A nil gauge records nothing.
func (c *ConfigErrorGauge) Set(stage ConfigErrorStage, failing bool) {
	if c == nil || !stage.Valid() {
		return
	}
	v := 0.0
	if failing {
		v = 1
	}
	c.g.WithLabelValues(string(stage)).Set(v)
}
