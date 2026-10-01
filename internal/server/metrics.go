package server

import "github.com/prometheus/client_golang/prometheus"

// ConfigDriftGauge is 1 while this replica's configuration file differs from
// the one the leader last applied to the store.
type ConfigDriftGauge struct{ g prometheus.Gauge }

// NewConfigDriftGauge registers the gauge on reg.
func NewConfigDriftGauge(reg prometheus.Registerer) *ConfigDriftGauge {
	g := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "kritika_config_drift",
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

// ConfigErrorGauge is 1 while the leader's attempt to apply the
// configuration to the store was refused; the last applied state stays
// live meanwhile.
type ConfigErrorGauge struct{ g prometheus.Gauge }

// NewConfigErrorGauge registers the gauge on reg, at 0.
func NewConfigErrorGauge(reg prometheus.Registerer) *ConfigErrorGauge {
	g := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "kritika_config_error",
		Help: "1 while the latest attempt to apply the configuration to the store failed, else 0.",
	})
	reg.MustRegister(g)
	return &ConfigErrorGauge{g: g}
}

// Set records whether applying is failing. A nil gauge records nothing.
func (c *ConfigErrorGauge) Set(failing bool) {
	if c == nil {
		return
	}
	if failing {
		c.g.Set(1)
		return
	}
	c.g.Set(0)
}
