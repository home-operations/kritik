package egress

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestRulesAllows(t *testing.T) {
	r := Rules{Hosts: []string{"api.github.com", "*.githubusercontent.com"}}
	for host, want := range map[string]bool{
		"api.github.com": true, "API.GITHUB.COM": true, "api.github.com.": true,
		"raw.githubusercontent.com": true, "githubusercontent.com": false, "evil-api.github.com": false, "": false,
	} {
		if got := r.Allows(host); got != want {
			t.Errorf("Allows(%q) = %v, want %v", host, got, want)
		}
	}
}

// upstream is a TLS server the proxy reaches; the proxy's client and dialer
// are pointed at it whatever host a request names.
func upstream(t *testing.T) (*httptest.Server, *http.Client, func(context.Context, string, string) (net.Conn, error)) {
	t.Helper()
	var seen http.Header
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.Header().Set("X-Seen-Auth", r.Header.Get("Authorization"))
		w.Header().Set("X-Seen-Proxy-Auth", r.Header.Get("Proxy-Authorization"))
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		_, _ = io.WriteString(w, r.Method+" "+r.Host+r.URL.Path+" "+string(body))
	}))
	t.Cleanup(srv.Close)
	_ = seen
	addr := srv.Listener.Addr().String()
	transport := srv.Client().Transport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	transport.TLSClientConfig.InsecureSkipVerify = true
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	dial := func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	return srv, client, dial
}

func newProxy(t *testing.T, rules Rules) (*httptest.Server, map[string]int) {
	t.Helper()
	_, client, dial := upstream(t)
	outcomes := map[string]int{}
	p := &Proxy{
		Rules: func() Rules { return rules }, Client: client, Dial: dial, Logger: slog.Default(),
		Observe: func(kind, outcome string) { outcomes[kind+"/"+outcome]++ },
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return srv, outcomes
}

// viaProxy is a client that sends everything through the proxy, trusting
// the test upstream's certificate for CONNECT tunnels.
func viaProxy(proxy *httptest.Server) *http.Client {
	pu, _ := url.Parse(proxy.URL)
	return &http.Client{Transport: &http.Transport{
		Proxy:           http.ProxyURL(pu),
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test upstream
	}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func TestUpgradeAddsCredentialAndStripsProxyHeaders(t *testing.T) {
	proxy, outcomes := newProxy(t, Rules{
		Hosts: []string{"api.github.com"}, Credentials: map[string]string{"api.github.com": "Bearer secret"},
	})
	req, _ := http.NewRequest(http.MethodPost, "http://api.github.com/repos/x/y", strings.NewReader("body"))
	req.Header.Set("Authorization", "Bearer from-the-pod")
	req.Header.Set("Proxy-Authorization", "Basic abc")
	resp, err := viaProxy(proxy).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "POST api.github.com/repos/x/y body" {
		t.Fatalf("status %d body %q", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Seen-Auth"); got != "Bearer secret" {
		t.Fatalf("upstream saw Authorization %q, want the configured credential", got)
	}
	if got := resp.Header.Get("X-Seen-Proxy-Auth"); got != "" {
		t.Fatalf("upstream saw Proxy-Authorization %q", got)
	}
	if outcomes["http/allowed"] != 1 {
		t.Fatalf("outcomes = %v", outcomes)
	}
}

func TestUpgradeReturnsRedirectsUnfollowed(t *testing.T) {
	proxy, _ := newProxy(t, Rules{Hosts: []string{"api.github.com"}})
	resp, err := viaProxy(proxy).Get("http://api.github.com/redirect")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/elsewhere" {
		t.Fatalf("status %d location %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestRefusals(t *testing.T) {
	proxy, outcomes := newProxy(t, Rules{Hosts: []string{"api.github.com"}})
	for _, u := range []string{"http://evil.example/", "http://api.github.com:8443/x"} {
		resp, err := viaProxy(proxy).Get(u)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s: status %d, want 403", u, resp.StatusCode)
		}
	}
	// A CONNECT to a host outside the rules, or to a port other than 443.
	for _, u := range []string{"https://evil.example/", "https://api.github.com:8443/"} {
		_, err := viaProxy(proxy).Get(u)
		if err == nil || !strings.Contains(err.Error(), "Forbidden") {
			t.Fatalf("%s: err = %v, want a Forbidden from the proxy", u, err)
		}
	}
	if outcomes["http/refused"] != 2 || outcomes["connect/refused"] != 2 {
		t.Fatalf("outcomes = %v", outcomes)
	}
}

func TestConnectTunnelsAllowedHosts(t *testing.T) {
	proxy, outcomes := newProxy(t, Rules{Hosts: []string{"*.github.com"}, Credentials: map[string]string{"api.github.com": "Bearer x"}})
	resp, err := viaProxy(proxy).Get("https://api.github.com/tunnelled")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "GET api.github.com/tunnelled " {
		t.Fatalf("body %q", body)
	}
	// A tunnel is opaque: no credential is added inside it.
	if got := resp.Header.Get("X-Seen-Auth"); got != "" {
		t.Fatalf("tunnelled request carried Authorization %q", got)
	}
	if outcomes["connect/allowed"] != 1 {
		t.Fatalf("outcomes = %v", outcomes)
	}
}

func TestProxyURL(t *testing.T) {
	for raw, ok := range map[string]bool{
		"http://kritik-gateway:8082": true, "http://kritik-gateway:8082/": true,
		"https://kritik-gateway:8082": false, "http://": false, "http://gw/path": false, "gw:8082": false,
	} {
		if _, err := ProxyURL(raw); (err == nil) != ok {
			t.Errorf("ProxyURL(%q) err = %v, want ok=%v", raw, err, ok)
		}
	}
}
