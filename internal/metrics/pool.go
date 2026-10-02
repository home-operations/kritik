package metrics

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// PoolCollector reports each connection pool's live state on every scrape:
// how many connections it holds and in what state, its ceiling, and how
// many acquires had to wait for one to free. Jobs and requests queue
// inside the pool without a sign otherwise, and the pool's ceiling is
// the one the database's connection limit is shared out by. One
// collector covers every pool: the registry refuses a second collector
// that describes the same metrics.
type PoolCollector struct {
	stats map[string]func() *pgxpool.Stat
}

// NewPoolCollector builds a collector over each pool's Stat by name,
// labelled pool=name.
func NewPoolCollector(stats map[string]func() *pgxpool.Stat) *PoolCollector {
	return &PoolCollector{stats: stats}
}

const lblPool = "pool"

var (
	poolConns = prometheus.NewDesc("kritika_db_pool_connections",
		"Connections the pool holds, by state: idle, acquired (in use), constructing.", []string{lblPool, "state"}, nil)
	poolMax = prometheus.NewDesc("kritika_db_pool_max_connections",
		"The pool's ceiling on connections.", []string{lblPool}, nil)
	poolAcquires = prometheus.NewDesc("kritika_db_pool_acquires_total",
		"Connections taken from the pool.", []string{lblPool}, nil)
	poolWaited = prometheus.NewDesc("kritika_db_pool_empty_acquires_total",
		"Connections taken from the pool after waiting for one to free: the pool was empty.", []string{lblPool}, nil)
	poolWait = prometheus.NewDesc("kritika_db_pool_acquire_seconds_total",
		"Time spent waiting for connections from the pool.", []string{lblPool}, nil)
)

// Describe implements prometheus.Collector.
func (c *PoolCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- poolConns
	ch <- poolMax
	ch <- poolAcquires
	ch <- poolWaited
	ch <- poolWait
}

// Collect implements prometheus.Collector.
func (c *PoolCollector) Collect(ch chan<- prometheus.Metric) {
	for name, stat := range c.stats {
		s := stat()
		ch <- prometheus.MustNewConstMetric(poolConns, prometheus.GaugeValue, float64(s.IdleConns()), name, "idle")
		ch <- prometheus.MustNewConstMetric(poolConns, prometheus.GaugeValue, float64(s.AcquiredConns()), name, "acquired")
		ch <- prometheus.MustNewConstMetric(poolConns, prometheus.GaugeValue, float64(s.ConstructingConns()), name, "constructing")
		ch <- prometheus.MustNewConstMetric(poolMax, prometheus.GaugeValue, float64(s.MaxConns()), name)
		ch <- prometheus.MustNewConstMetric(poolAcquires, prometheus.CounterValue, float64(s.AcquireCount()), name)
		ch <- prometheus.MustNewConstMetric(poolWaited, prometheus.CounterValue, float64(s.EmptyAcquireCount()), name)
		ch <- prometheus.MustNewConstMetric(poolWait, prometheus.CounterValue, s.AcquireDuration().Seconds(), name)
	}
}
