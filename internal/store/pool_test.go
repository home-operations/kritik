package store

import (
	"testing"
	"time"
)

func TestPoolConfig(t *testing.T) {
	tests := []struct {
		name         string
		conn         Conn
		statementMax time.Duration
		wantMax      int32
		wantTimeout  string
	}{
		{name: "the ceiling given applies", conn: Conn{URL: "postgres://u:p@db/kritika"}, wantMax: 16},
		{name: "the URI's pool_max_conns wins", conn: Conn{URL: "postgres://u:p@db/kritika?pool_max_conns=40"}, wantMax: 40},
		{name: "a keyword DSN's pool_max_conns wins too", conn: Conn{URL: "host=db user=u password=p dbname=kritika pool_max_conns=3"}, wantMax: 3},
		{name: "a statement timeout is set in milliseconds", conn: Conn{URL: "postgres://u:p@db/kritika"}, statementMax: 10 * time.Minute, wantMax: 16, wantTimeout: "600000"},
		{name: "parameters take the ceiling", conn: Conn{Host: "db", Port: 5432, Database: "kritika", SSLMode: "disable", User: "u", Password: "p"}, wantMax: 16},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := poolConfig(tt.conn, "kritika-test", tt.statementMax, 16)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.MaxConns != tt.wantMax {
				t.Errorf("MaxConns = %d, want %d", cfg.MaxConns, tt.wantMax)
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

// TestConnParameters: a connection built from parameters carries each of
// them, whatever characters they hold, and the password never has to be
// escaped.
func TestConnParameters(t *testing.T) {
	conn := Conn{
		Host: "kritika-postgres-rw.kritika.svc", Port: 5433, Database: "kri tika", SSLMode: "require",
		ConnectTimeout: 10 * time.Second, User: `o'brien\x`, Password: "p@ss w'rd/%\\#?&=",
	}
	cfg, err := conn.config()
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.ConnConfig
	if c.Host != conn.Host || c.Port != conn.Port || c.Database != conn.Database || c.User != conn.User || c.Password != conn.Password {
		t.Fatalf("parsed = host %q port %d db %q user %q password %q", c.Host, c.Port, c.Database, c.User, c.Password)
	}
	if c.ConnectTimeout != 10*time.Second {
		t.Errorf("connect timeout = %s", c.ConnectTimeout)
	}
	if c.TLSConfig == nil {
		t.Error("sslmode=require left TLS off")
	}
	if cfg, err := (Conn{Host: "db", Port: 5432, Database: "d", SSLMode: "disable", User: "u"}).config(); err != nil || cfg.ConnConfig.TLSConfig != nil || cfg.ConnConfig.ConnectTimeout != 0 {
		t.Errorf("sslmode=disable without a timeout = %+v, %v", cfg, err)
	}
	for _, c := range []Conn{{}, {URL: "postgres://u@db/k"}, {Host: "db"}} {
		if c.Set() != (c.URL != "" || c.Host != "") {
			t.Errorf("Set(%+v) = %v", c, c.Set())
		}
	}
}
