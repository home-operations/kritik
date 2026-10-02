package metrics

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// PoolCollector reports a connection pool's live state on every scrape:
// how many connections it holds and in what state, its ceiling, and how
// many acquires had to wait for one to free. Jobs and requests queue
// inside the pool without a sign otherwise, and the pool's ceiling is
// the one the database's connection limit is shared out by.
type PoolCollector struct {
	name string
	stat func() *pgxpool.Stat
}

// NewPoolCollector builds a collector over a pool's Stat, labelled pool=name.
func NewPoolCollector(name string, stat func() *pgxpool.Stat) *PoolCollector {
	return &PoolCollector{name: name, stat: stat}
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
	s := c.stat()
	ch <- prometheus.MustNewConstMetric(poolConns, prometheus.GaugeValue, float64(s.IdleConns()), c.name, "idle")
	ch <- prometheus.MustNewConstMetric(poolConns, prometheus.GaugeValue, float64(s.AcquiredConns()), c.name, "acquired")
	ch <- prometheus.MustNewConstMetric(poolConns, prometheus.GaugeValue, float64(s.ConstructingConns()), c.name, "constructing")
	ch <- prometheus.MustNewConstMetric(poolMax, prometheus.GaugeValue, float64(s.MaxConns()), c.name)
	ch <- prometheus.MustNewConstMetric(poolAcquires, prometheus.CounterValue, float64(s.AcquireCount()), c.name)
	ch <- prometheus.MustNewConstMetric(poolWaited, prometheus.CounterValue, float64(s.EmptyAcquireCount()), c.name)
	ch <- prometheus.MustNewConstMetric(poolWait, prometheus.CounterValue, s.AcquireDuration().Seconds(), c.name)
}
