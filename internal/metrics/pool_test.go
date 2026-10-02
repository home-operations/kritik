package metrics

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestPoolCollector reads a pool that has never connected: its ceiling and
// its empty state, labelled by name.
func TestPoolCollector(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://kritika@127.0.0.1:1/kritika?pool_max_conns=7")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	reg := prometheus.NewRegistry()
	reg.MustRegister(NewPoolCollector("app", pool.Stat))
	want := `# HELP kritika_db_pool_connections Connections the pool holds, by state: idle, acquired (in use), constructing.
# TYPE kritika_db_pool_connections gauge
kritika_db_pool_connections{pool="app",state="acquired"} 0
kritika_db_pool_connections{pool="app",state="constructing"} 0
kritika_db_pool_connections{pool="app",state="idle"} 0
# HELP kritika_db_pool_max_connections The pool's ceiling on connections.
# TYPE kritika_db_pool_max_connections gauge
kritika_db_pool_max_connections{pool="app"} 7
# HELP kritika_db_pool_empty_acquires_total Connections taken from the pool after waiting for one to free: the pool was empty.
# TYPE kritika_db_pool_empty_acquires_total counter
kritika_db_pool_empty_acquires_total{pool="app"} 0
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want),
		"kritika_db_pool_connections", "kritika_db_pool_max_connections", "kritika_db_pool_empty_acquires_total"); err != nil {
		t.Fatal(err)
	}
	if n, err := testutil.GatherAndCount(reg); err != nil || n != 7 {
		t.Fatalf("gathered %d series, %v; want 7", n, err)
	}
}
