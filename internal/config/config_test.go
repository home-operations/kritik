package config

import (
	"bytes"
	"encoding/base64"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr bool
		check   func(t *testing.T, c *Config)
	}{
		{
			name: "defaults",
			check: func(t *testing.T, c *Config) {
				if c.Addr != ":8080" || c.MetricsAddr != ":8081" {
					t.Fatalf("addr defaults = %q, %q", c.Addr, c.MetricsAddr)
				}
				if c.LogFormat != "json" {
					t.Fatalf("log format default = %q", c.LogFormat)
				}
				if c.ConfigFile != "" || c.ConfigReloadInterval != 10*time.Second {
					t.Fatalf("config file defaults = %q, %s", c.ConfigFile, c.ConfigReloadInterval)
				}
				if c.DatabaseOwnerURL != "" {
					t.Fatalf("owner should be unset by default: %+v", c)
				}
				if lvl, _ := c.Level(); lvl != slog.LevelInfo {
					t.Fatalf("level default = %v", lvl)
				}
				if c.WebAddr != ":8083" {
					t.Fatalf("web addr default = %q", c.WebAddr)
				}
				if c.WebURL != "" || c.WebURLParsed() != nil || c.WebEnabled(RoleAll) {
					t.Fatalf("web should be unconfigured by default: %+v", c)
				}
			},
		},
		{
			name: "explicit values",
			env:  map[string]string{"KRITIK_ADDR": ":9090", "KRITIK_LOG_LEVEL": "debug", "KRITIK_LOG_FORMAT": "text"},
			check: func(t *testing.T, c *Config) {
				if c.Addr != ":9090" {
					t.Fatalf("addr = %q", c.Addr)
				}
				if lvl, _ := c.Level(); lvl != slog.LevelDebug {
					t.Fatalf("level = %v", lvl)
				}
			},
		},
		{name: "bad level", env: map[string]string{"KRITIK_LOG_LEVEL": "loud"}, wantErr: true},
		{name: "bad format", env: map[string]string{"KRITIK_LOG_FORMAT": "xml"}, wantErr: true},
		{name: "zero reload interval", env: map[string]string{"KRITIK_CONFIG_RELOAD_INTERVAL": "0s"}, wantErr: true},
		{name: "database url required", env: map[string]string{"KRITIK_DATABASE_URL": ""}, wantErr: true},
		{name: "same role for app and runner", env: map[string]string{"KRITIK_DATABASE_RUNNER_ROLE": "kritik_app"}, wantErr: true},
		{name: "zero leader retry", env: map[string]string{"KRITIK_LEADER_RETRY_INTERVAL": "0"}, wantErr: true},
		{name: "unknown executor", env: map[string]string{"KRITIK_EXECUTOR": "docker"}, wantErr: true},
		{name: "zero review workers", env: map[string]string{"KRITIK_REVIEW_WORKERS": "0"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("KRITIK_DATABASE_URL", "postgres://app@db/kritik")
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			cfg, err := Load()
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			tt.check(t, cfg)
		})
	}
}

func TestWebURL(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr bool
		want    string // expected WebURL after Load, defaults to url when empty and wantErr is false
		path    string // expected WebBasePath
	}{
		{name: "unset", url: ""},
		{name: "valid https", url: "https://dash.example.com"},
		{name: "trailing slash trimmed", url: "http://dash.example.com/", want: "http://dash.example.com"},
		{name: "under a path", url: "https://example.com/kritik/", want: "https://example.com/kritik", path: "/kritik"},
		{name: "missing scheme", url: "dash.example.com", wantErr: true},
		{name: "non-http scheme", url: "ftp://dash.example.com", wantErr: true},
		{name: "missing host", url: "https:///path", wantErr: true},
		{name: "with query", url: "https://dash.example.com?x=1", wantErr: true},
		{name: "with fragment", url: "https://dash.example.com#frag", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("KRITIK_DATABASE_URL", "postgres://app@db/kritik")
			t.Setenv("KRITIK_WEB_URL", tt.url)
			cfg, err := Load()
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			want := tt.want
			if want == "" {
				want = tt.url
			}
			if cfg.WebURL != want || cfg.WebBasePath() != tt.path {
				t.Fatalf("WebURL = %q, base path %q, want %q, %q", cfg.WebURL, cfg.WebBasePath(), want, tt.path)
			}
			if tt.url == "" {
				if cfg.WebURLParsed() != nil || cfg.WebEnabled(RoleAll) || !cfg.WebEnabled(RoleWeb) {
					t.Fatalf("web role always serves the dashboard, all does only once WebURL is set: %+v", cfg)
				}
				return
			}
			u := cfg.WebURLParsed()
			if u == nil || u.String() == "" {
				t.Fatalf("WebURLParsed() = %v", u)
			}
			if !cfg.WebEnabled(RoleAll) || !cfg.WebEnabled(RoleWeb) {
				t.Fatal("web should be enabled for all and web once WebURL is set")
			}
		})
	}
}

func TestRoleValidation(t *testing.T) {
	t.Setenv("KRITIK_DATABASE_URL", "postgres://app@db/kritik")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ValidateWorker(); err == nil {
		t.Fatal("kubernetes executor without a runner image must fail")
	}
	cfg.RunnerImage = "img"
	if err := cfg.ValidateWorker(); err != nil {
		t.Fatal(err)
	}
	cfg.Executor, cfg.RunnerImage = "local", ""
	if err := cfg.ValidateWorker(); err == nil {
		t.Fatal("local executor without a runner DSN must fail")
	}
	if err := cfg.ValidateRunner(); err == nil {
		t.Fatal("runner without its inputs must fail")
	}
	cfg.RunSpecFile = "/var/run/kritik/spec.json"
	if err := cfg.ValidateRunner(); err != nil {
		t.Fatal(err)
	}
	if err := cfg.ValidateWeb(); err == nil {
		t.Fatal("web role without KRITIK_WEB_URL must fail")
	}
	cfg.WebURL = "https://dash.example.com"
	if err := cfg.ValidateWeb(); err != nil {
		t.Fatal(err)
	}
}

func TestParseRole(t *testing.T) {
	tests := []struct {
		in      string
		want    Role
		wantErr bool
	}{
		{in: "all", want: RoleAll},
		{in: " Worker ", want: RoleWorker},
		{in: "ingest", want: RoleIngest},
		{in: "runner", want: RoleRunner},
		{in: "Web", want: RoleWeb},
		{in: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseRole(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseRole(%q) = %q, want error", tt.in, got)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("ParseRole(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
			}
		})
	}
}

func TestDashboardKeyring(t *testing.T) {
	key := func(b byte) string { return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{b}, 32)) }
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
		wantNil bool
	}{
		{name: "unset", wantNil: true},
		{name: "current key", env: map[string]string{"KRITIK_DASHBOARD_KEY": key('a')}},
		{name: "current and old keys", env: map[string]string{"KRITIK_DASHBOARD_KEY": key('a'), "KRITIK_DASHBOARD_OLD_KEYS": key('b') + "," + key('c')}},
		{name: "short key", env: map[string]string{"KRITIK_DASHBOARD_KEY": "c2hvcnQ="}, wantErr: "KRITIK_DASHBOARD_KEY"},
		{name: "bad old key", env: map[string]string{"KRITIK_DASHBOARD_KEY": key('a'), "KRITIK_DASHBOARD_OLD_KEYS": "nope"}, wantErr: "KRITIK_DASHBOARD_OLD_KEYS"},
		{name: "old key repeats the current one", env: map[string]string{"KRITIK_DASHBOARD_KEY": key('a'), "KRITIK_DASHBOARD_OLD_KEYS": key('a')}, wantErr: "duplicate"},
		{name: "old keys without a current key", env: map[string]string{"KRITIK_DASHBOARD_OLD_KEYS": key('b')}, wantErr: "KRITIK_DASHBOARD_KEY"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("KRITIK_DATABASE_URL", "postgres://app@db/kritik")
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			cfg, err := Load()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Load = %v, want an error mentioning %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := cfg.DashboardKeyring(); (got == nil) != tt.wantNil {
				t.Fatalf("DashboardKeyring() = %v, want nil %v", got, tt.wantNil)
			}
		})
	}
}

func TestEnv(t *testing.T) {
	t.Setenv("KRITIK_DATABASE_URL", "postgres://app:secret@db/kritik")
	t.Setenv("KRITIK_ADDR", ":9090")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]EnvVar{}
	for _, e := range cfg.Env() {
		vars[e.Name] = e
	}
	for name, want := range map[string]EnvVar{
		"KRITIK_ADDR":                   {Name: "KRITIK_ADDR", Value: ":9090", Set: true},
		"KRITIK_METRICS_ADDR":           {Name: "KRITIK_METRICS_ADDR", Value: ":8081"},
		"KRITIK_CONFIG_RELOAD_INTERVAL": {Name: "KRITIK_CONFIG_RELOAD_INTERVAL", Value: "10s"},
		"KRITIK_DATABASE_URL":           {Name: "KRITIK_DATABASE_URL", Value: "set", Secret: true, Set: true},
		"KRITIK_DASHBOARD_KEY":          {Name: "KRITIK_DASHBOARD_KEY", Value: "not set", Secret: true},
	} {
		if vars[name] != want {
			t.Errorf("%s = %+v, want %+v", name, vars[name], want)
		}
	}
	for _, e := range cfg.Env() {
		if strings.Contains(e.Value, "secret") {
			t.Fatalf("%s shows a secret: %q", e.Name, e.Value)
		}
	}
}
