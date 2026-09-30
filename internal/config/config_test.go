package config

import (
	"log/slog"
	"strings"
	"testing"
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
				if c.WebAddr != ":8083" {
					t.Fatalf("web addr default = %q", c.WebAddr)
				}
				if c.WebURL != "" || c.WebURLParsed() != nil {
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
	t.Setenv("KRITIK_DATABASE_URL", "postgres://app@db/kritik")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	err = cfg.ValidateServe()
	if err == nil || !strings.Contains(err.Error(), "KRITIK_RUNNER_IMAGE") || !strings.Contains(err.Error(), "KRITIK_WEB_URL") {
		t.Fatalf("serve without a runner image or a web URL = %v, want both named", err)
	}
	cfg.RunnerImage, cfg.WebURL = "img", "https://dash.example.com"
	if err := cfg.ValidateServe(); err != nil {
		t.Fatal(err)
	}
	cfg.Executor, cfg.RunnerImage = "local", ""
	if err := cfg.ValidateServe(); err == nil || !strings.Contains(err.Error(), "KRITIK_RUNNER_DATABASE_URL") {
		t.Fatalf("local executor without a runner DSN = %v", err)
	}
	if err := cfg.ValidateRunner(); err == nil {
		t.Fatal("run without its inputs must fail")
	}
	cfg.RunSpecFile = "/var/run/kritik/spec.json"
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
		"KRITIK_ADDR":         {Name: "KRITIK_ADDR", Value: ":9090", Set: true},
		"KRITIK_METRICS_ADDR": {Name: "KRITIK_METRICS_ADDR", Value: ":8081"},
		"KRITIK_DATABASE_URL": {Name: "KRITIK_DATABASE_URL", Value: "set", Secret: true, Set: true},
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
