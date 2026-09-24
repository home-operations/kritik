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
