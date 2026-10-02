package store

import (
	"testing"
	"time"
)

func TestPoolConfig(t *testing.T) {
	tests := []struct {
		name         string
		url          string
		statementMax time.Duration
		wantMax      int32
		wantTimeout  string
		wantPing     time.Duration
	}{
		{name: "the ceiling given applies", url: "postgres://u:p@db/kritika", wantMax: 16, wantPing: pingTimeout},
		{name: "the URI's pool_max_conns wins", url: "postgres://u:p@db/kritika?pool_max_conns=40", wantMax: 40, wantPing: pingTimeout},
		{name: "a keyword DSN's pool_max_conns wins too", url: "host=db user=u password=p dbname=kritika pool_max_conns=3", wantMax: 3, wantPing: pingTimeout},
		{name: "a statement timeout is set in milliseconds", url: "postgres://u:p@db/kritika", statementMax: 10 * time.Minute, wantMax: 16, wantTimeout: "600000", wantPing: pingTimeout},
		{name: "the URI's pool_ping_timeout wins", url: "postgres://u:p@db/kritika?pool_ping_timeout=1s", wantMax: 16, wantPing: time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := poolConfig(tt.url, "kritika-test", tt.statementMax, 16)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.MaxConns != tt.wantMax {
				t.Errorf("MaxConns = %d, want %d", cfg.MaxConns, tt.wantMax)
			}
			if cfg.PingTimeout != tt.wantPing {
				t.Errorf("PingTimeout = %s, want %s", cfg.PingTimeout, tt.wantPing)
			}
			params := cfg.ConnConfig.RuntimeParams
			if params["application_name"] != "kritika-test" || params["tcp_keepalives_idle"] != "30" {
				t.Errorf("runtime params = %v", params)
			}
			if params["statement_timeout"] != tt.wantTimeout {
				t.Errorf("statement_timeout = %q, want %q", params["statement_timeout"], tt.wantTimeout)
			}
		})
	}
}
