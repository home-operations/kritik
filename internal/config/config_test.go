package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/home-operations/kritika/internal/store"
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
				if c.ConfigFile != "" {
					t.Fatalf("config file default = %q", c.ConfigFile)
				}
				if c.DatabaseOwnerURL != "" {
					t.Fatalf("owner should be unset by default: %+v", c)
				}
				if lvl, _ := c.Level(); lvl != slog.LevelInfo {
					t.Fatalf("level default = %v", lvl)
				}
				if c.WebURL != "" || c.WebURLParsed() != nil {
					t.Fatalf("web should be unconfigured by default: %+v", c)
				}
			},
		},
		{
			name: "explicit values",
			env:  map[string]string{"KRITIKA_ADDR": ":9090", "KRITIKA_LOG_LEVEL": "debug", "KRITIKA_LOG_FORMAT": "text"},
			check: func(t *testing.T, c *Config) {
				if c.Addr != ":9090" {
					t.Fatalf("addr = %q", c.Addr)
				}
				if lvl, _ := c.Level(); lvl != slog.LevelDebug {
					t.Fatalf("level = %v", lvl)
				}
			},
		},
		{name: "bad level", env: map[string]string{"KRITIKA_LOG_LEVEL": "loud"}, wantErr: true},
		{name: "bad format", env: map[string]string{"KRITIKA_LOG_FORMAT": "xml"}, wantErr: true},
		{name: "a database connection is required", env: map[string]string{"KRITIKA_DATABASE_URL": ""}, wantErr: true},
		{
			name: "connections built from parameters",
			env: map[string]string{
				"KRITIKA_DATABASE_URL": "", "KRITIKA_DATABASE_HOST": "db", "KRITIKA_DATABASE_PORT": "5433", "KRITIKA_DATABASE_SSLMODE": "verify-full",
				"KRITIKA_DATABASE_USER": "kritika_app", "KRITIKA_DATABASE_PASSWORD": "p'w",
				"KRITIKA_DATABASE_OWNER_USER": "kritika", "KRITIKA_DATABASE_OWNER_PASSWORD": "o",
				"KRITIKA_RUNNER_DATABASE_URL": "postgres://runner@db/kritika",
			},
			check: func(t *testing.T, c *Config) {
				want := store.Conn{Host: "db", Port: 5433, Database: "kritika", SSLMode: "verify-full", ConnectTimeout: 10 * time.Second, User: "kritika_app", Password: "p'w"}
				if c.Database() != want {
					t.Fatalf("Database() = %+v, want %+v", c.Database(), want)
				}
				want.User, want.Password = "kritika", "o"
				if c.OwnerDatabase() != want {
					t.Fatalf("OwnerDatabase() = %+v, want %+v", c.OwnerDatabase(), want)
				}
				if c.RunnerDatabase() != (store.Conn{URL: "postgres://runner@db/kritika"}) {
					t.Fatalf("RunnerDatabase() = %+v, want the URL", c.RunnerDatabase())
				}
				if c.DatabaseParams().User != "" || c.DatabaseParams().Host != "db" {
					t.Fatalf("DatabaseParams() = %+v", c.DatabaseParams())
				}
			},
		},
		{
			name: "a URL wins over parameters and an absent owner stays unset",
			env:  map[string]string{"KRITIKA_DATABASE_HOST": "db", "KRITIKA_DATABASE_USER": "kritika_app"},
			check: func(t *testing.T, c *Config) {
				if c.Database() != (store.Conn{URL: "postgres://app@db/kritika"}) || c.OwnerDatabase().Set() {
					t.Fatalf("Database() = %+v, OwnerDatabase() = %+v", c.Database(), c.OwnerDatabase())
				}
			},
		},
		{name: "a username without a host", env: map[string]string{"KRITIKA_DATABASE_URL": "", "KRITIKA_DATABASE_USER": "kritika_app"}, wantErr: true},
		{name: "the owner's username without a host", env: map[string]string{"KRITIKA_DATABASE_OWNER_USER": "kritika"}, wantErr: true},
		{name: "unknown sslmode", env: map[string]string{"KRITIKA_DATABASE_SSLMODE": "yes"}, wantErr: true},
		{name: "zero connect timeout", env: map[string]string{"KRITIKA_DATABASE_CONNECT_TIMEOUT": "0"}, wantErr: true},
		{name: "same role for app and runner", env: map[string]string{"KRITIKA_DATABASE_RUNNER_ROLE": "kritika_app"}, wantErr: true},
		{name: "zero leader retry", env: map[string]string{"KRITIKA_LEADER_RETRY_INTERVAL": "0"}, wantErr: true},
		{name: "unknown executor", env: map[string]string{"KRITIKA_EXECUTOR": "docker"}, wantErr: true},
		{name: "zero review workers", env: map[string]string{"KRITIKA_REVIEW_WORKERS": "0"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("KRITIKA_DATABASE_URL", "postgres://app@db/kritika")
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
		{name: "under a path", url: "https://example.com/kritika/", want: "https://example.com/kritika", path: "/kritika"},
		{name: "missing scheme", url: "dash.example.com", wantErr: true},
		{name: "non-http scheme", url: "ftp://dash.example.com", wantErr: true},
		{name: "missing host", url: "https:///path", wantErr: true},
		{name: "with query", url: "https://dash.example.com?x=1", wantErr: true},
		{name: "with fragment", url: "https://dash.example.com#frag", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("KRITIKA_DATABASE_URL", "postgres://app@db/kritika")
			t.Setenv("KRITIKA_WEB_URL", tt.url)
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
				if cfg.WebURLParsed() != nil {
					t.Fatalf("WebURLParsed() = %v, want nil", cfg.WebURLParsed())
				}
				return
			}
			u := cfg.WebURLParsed()
			if u == nil || u.String() == "" {
				t.Fatalf("WebURLParsed() = %v", u)
			}
		})
	}
}

func TestCommandValidation(t *testing.T) {
	t.Setenv("KRITIKA_DATABASE_URL", "postgres://app@db/kritika")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	err = cfg.ValidateServe()
	for _, want := range []string{"KRITIKA_RUNNER_IMAGE", "KRITIKA_WEB_URL", "KRITIKA_GATEWAY_URL"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("serve without a runner image, a web URL or the gateway = %v, want %s named", err, want)
		}
	}
	cfg.RunnerImage, cfg.WebURL, cfg.GatewayURL, cfg.RunnerDatabaseSecretKey = "img", "https://dash.example.com", "http://kritika-gateway:8082", "uri"
	if err := cfg.ValidateServe(); err != nil {
		t.Fatal(err)
	}
	cfg.RunnerDatabaseSecretKey = ""
	if err := cfg.ValidateServe(); err == nil || !strings.Contains(err.Error(), "KRITIKA_DATABASE_HOST") {
		t.Fatalf("kubernetes executor with a username in the runner Secret but no host = %v", err)
	}
	cfg.DatabaseHost = "db"
	if err := cfg.ValidateServe(); err != nil {
		t.Fatalf("kubernetes executor with a host = %v", err)
	}
	cfg.Executor, cfg.RunnerImage = ExecutorLocal, ""
	if err := cfg.ValidateServe(); err == nil || !strings.Contains(err.Error(), "KRITIKA_RUNNER_DATABASE_URL") {
		t.Fatalf("local executor without a runner connection = %v", err)
	}
	cfg.RunnerDatabaseUser = "kritika_runner"
	if err := cfg.ValidateServe(); err != nil {
		t.Fatalf("local executor with a runner username = %v", err)
	}
	if err := cfg.ValidateRunner(); err == nil {
		t.Fatal("run without its inputs must fail")
	}
	cfg.RunSpecFile = "/var/run/kritika/spec.json"
	if err := cfg.ValidateRunner(); err != nil {
		t.Fatal(err)
	}
}

func TestParseCommand(t *testing.T) {
	for _, tt := range []struct {
		args    []string
		want    Command
		wantErr bool
	}{
		{args: nil, want: CommandServe},
		{args: []string{"serve"}, want: CommandServe},
		{args: []string{"run"}, want: CommandRun},
		{args: []string{"--role", "all"}, wantErr: true},
		{args: []string{"worker"}, wantErr: true},
		{args: []string{"serve", "extra"}, wantErr: true},
	} {
		got, err := ParseCommand(tt.args)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("ParseCommand(%q) = %q, %v; want %q (error %v)", tt.args, got, err, tt.want, tt.wantErr)
		}
	}
}

func TestEnv(t *testing.T) {
	t.Setenv("KRITIKA_DATABASE_URL", "postgres://app:secret@db/kritika")
	t.Setenv("KRITIKA_DATABASE_HOST", "db")
	t.Setenv("KRITIKA_DATABASE_OWNER_USER", "kritika")
	t.Setenv("KRITIKA_DATABASE_OWNER_PASSWORD", "secret")
	t.Setenv("KRITIKA_ADDR", ":9090")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]EnvVar{}
	for _, e := range cfg.Env() {
		vars[e.Name] = e
	}
	for name, want := range map[string]EnvVar{
		"KRITIKA_ADDR":                    {Name: "KRITIKA_ADDR", Value: ":9090", Set: true},
		"KRITIKA_METRICS_ADDR":            {Name: "KRITIKA_METRICS_ADDR", Value: ":8081"},
		"KRITIKA_DATABASE_URL":            {Name: "KRITIKA_DATABASE_URL", Value: "set", Secret: true, Set: true},
		"KRITIKA_DATABASE_OWNER_PASSWORD": {Name: "KRITIKA_DATABASE_OWNER_PASSWORD", Value: "set", Secret: true, Set: true},
		"KRITIKA_DATABASE_OWNER_USER":     {Name: "KRITIKA_DATABASE_OWNER_USER", Value: "kritika", Set: true},
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
