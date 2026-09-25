package runner

import (
	"slices"
	"testing"
)

func TestCommandEnv(t *testing.T) {
	for _, name := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy"} {
		t.Setenv(name, "")
	}
	t.Setenv("PATH", "/usr/local/bin:/usr/bin")
	t.Setenv("KRITIK_GIT_TOKEN", "secret")

	env, proxied := commandEnv("/tmp/home")
	if proxied || !slices.Equal(env, []string{"PATH=/usr/local/bin:/usr/bin", "HOME=/tmp/home"}) {
		t.Fatalf("without a proxy: env = %q, proxied = %v", env, proxied)
	}

	t.Setenv("HTTPS_PROXY", "http://gateway:8082")
	t.Setenv("HTTP_PROXY", "http://gateway:8082")
	t.Setenv("no_proxy", "localhost")
	env, proxied = commandEnv("/tmp/home")
	want := []string{
		"PATH=/usr/local/bin:/usr/bin", "HOME=/tmp/home",
		"HTTP_PROXY=http://gateway:8082", "http_proxy=http://gateway:8082",
		"HTTPS_PROXY=http://gateway:8082", "https_proxy=http://gateway:8082",
		"NO_PROXY=localhost", "no_proxy=localhost",
	}
	if !proxied || !slices.Equal(env, want) {
		t.Fatalf("with the gateway: env = %q, proxied = %v", env, proxied)
	}
}
