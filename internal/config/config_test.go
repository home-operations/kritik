package config

import (
	"bytes"
	"encoding/base64"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/home-operations/kritik/internal/jobtimeout"
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
				if c.ConfigFile != "/etc/kritik/config.yaml" || c.ConfigReloadInterval != 10*time.Second {
					t.Fatalf("config file defaults = %q, %s", c.ConfigFile, c.ConfigReloadInterval)
				}
				if c.EmbeddingEnabled() || c.DatabaseOwnerURL != "" || c.ReindexOnModelChange {
					t.Fatalf("embedder and owner should be unset by default: %+v", c)
				}
				if lvl, _ := c.Level(); lvl != slog.LevelInfo {
					t.Fatalf("level default = %v", lvl)
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
		{name: "embedder half configured", env: map[string]string{"KRITIK_EMBED_MODEL": "m", "KRITIK_EMBED_DIMS": "1024"}, wantErr: true},
		{name: "embedder dims over halfvec limit", env: map[string]string{"KRITIK_EMBED_BASE_URL": "https://e", "KRITIK_EMBED_API_KEY": "k", "KRITIK_EMBED_MODEL": "m", "KRITIK_EMBED_DIMS": "4096"}, wantErr: true},
		{name: "embedder max batch zero", env: map[string]string{"KRITIK_EMBED_MAX_BATCH": "0"}, wantErr: true},
		{name: "same role for app and runner", env: map[string]string{"KRITIK_DATABASE_RUNNER_ROLE": "kritik_app"}, wantErr: true},
		{name: "zero leader retry", env: map[string]string{"KRITIK_LEADER_RETRY_INTERVAL": "0"}, wantErr: true},
		{name: "unknown executor", env: map[string]string{"KRITIK_EXECUTOR": "docker"}, wantErr: true},
		{name: "zero review workers", env: map[string]string{"KRITIK_REVIEW_WORKERS": "0"}, wantErr: true},
		{name: "runner deadline past the cap", env: map[string]string{"KRITIK_RUNNER_DEADLINE": (jobtimeout.MaxRunnerDeadline + time.Second).String()}, wantErr: true},
		{
			name: "runner deadline at the cap",
			env:  map[string]string{"KRITIK_RUNNER_DEADLINE": jobtimeout.MaxRunnerDeadline.String()},
			check: func(t *testing.T, c *Config) {
				if c.RunnerDeadline != jobtimeout.MaxRunnerDeadline {
					t.Fatalf("runner deadline = %s", c.RunnerDeadline)
				}
			},
		},
		{
			name: "embedder fully configured",
			env:  map[string]string{"KRITIK_EMBED_BASE_URL": "https://e", "KRITIK_EMBED_API_KEY": "k", "KRITIK_EMBED_MODEL": "m", "KRITIK_EMBED_DIMS": "1024"},
			check: func(t *testing.T, c *Config) {
				if !c.EmbeddingEnabled() || c.EmbedDims != 1024 || c.EmbedMaxBatch != 64 {
					t.Fatalf("embedder = %+v", c)
				}
			},
		},
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
		{in: "web", wantErr: true},
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
