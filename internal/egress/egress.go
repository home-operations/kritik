// Package egress is the worker's forward proxy: the one route out of the
// cluster for runner pods, which reach it through HTTPS_PROXY and have no
// other egress. A CONNECT to an allowed host on 443 is tunnelled without
// inspection, which is how git, the model SDKs and curl reach TLS
// endpoints; a plain http:// request to an allowed host is upgraded to
// HTTPS here, with a configured credential added, so a runner can use an
// API at an installation's rate limit without holding a token. Every other
// destination is refused by hostname.
package egress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Rules is what the proxy allows and adds, read on every request so a
// configuration reload takes effect at once.
type Rules struct {
	// Hosts are allowed destinations, lowercase, with a leading "*." for a
	// suffix match. The port is not part of a rule: CONNECT is allowed to
	// 443 only, and an upgraded request always goes to 443.
	Hosts []string
	// Credentials map a host to the Authorization header value an
	// upgraded request to it carries.
	Credentials map[string]string
}

// Allows reports whether host, without a port, matches a rule.
func (r Rules) Allows(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, rule := range r.Hosts {
		switch {
		case strings.HasPrefix(rule, "*."):
			if strings.HasSuffix(host, rule[1:]) && len(host) > len(rule)-1 {
				return true
			}
		case host == rule:
			return true
		}
	}
	return false
}

// Outcomes a request ends with, for metrics.
const (
	OutcomeAllowed = "allowed"
	OutcomeRefused = "refused"
	OutcomeError   = "error"
)

// Proxy is the forward proxy handler.
type Proxy struct {
	// Rules returns the current rules; it is called per request.
	Rules func() Rules
	// Dial opens the upstream connection for a CONNECT; nil means a
	// net.Dialer with connectTimeout.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// Client sends upgraded requests; nil means a client that does not
	// follow redirects, so the runner's own client sees them.
	Client *http.Client
	// Observe, if set, is called once per request with its kind (connect,
	// http) and outcome.
	Observe func(kind, outcome string)
	Logger  *slog.Logger
}

const (
	connectTimeout = 15 * time.Second
	// tunnelIdle ends a CONNECT tunnel neither side has used for this long.
	tunnelIdle = 5 * time.Minute
	// upgradeTimeout bounds one upgraded request, response body included.
	upgradeTimeout = 2 * time.Minute
)

func (p *Proxy) observe(kind, outcome string) {
	if p.Observe != nil {
		p.Observe(kind, outcome)
	}
}

// ServeHTTP implements http.Handler.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.connect(w, r)
		return
	}
	p.upgrade(w, r)
}

// connect tunnels a CONNECT to host:443 when the host is allowed.
func (p *Proxy) connect(w http.ResponseWriter, r *http.Request) {
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil || port != "443" || !p.Rules().Allows(host) {
		p.observe("connect", OutcomeRefused)
		p.Logger.Warn("egress refused", "kind", "connect", "host", r.Host)
		http.Error(w, "egress: destination not allowed", http.StatusForbidden)
		return
	}
	dial := p.Dial
	if dial == nil {
		dial = (&net.Dialer{Timeout: connectTimeout}).DialContext
	}
	upstream, err := dial(r.Context(), "tcp", net.JoinHostPort(host, port))
	if err != nil {
		p.observe("connect", OutcomeError)
		p.Logger.Warn("egress dial failed", "host", r.Host, "error", err)
		http.Error(w, "egress: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = upstream.Close() }()
	hj, ok := w.(http.Hijacker)
	if !ok {
		p.observe("connect", OutcomeError)
		http.Error(w, "egress: connection cannot be hijacked", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	client, buf, err := hj.Hijack()
	if err != nil {
		p.observe("connect", OutcomeError)
		p.Logger.Warn("egress hijack failed", "host", r.Host, "error", err)
		return
	}
	defer func() { _ = client.Close() }()
	p.observe("connect", OutcomeAllowed)
	p.Logger.Debug("egress tunnel", "host", r.Host)
	tunnel(client, buf, upstream)
}

// tunnel copies both ways until either side closes or the tunnel idles
// out, then closes both.
func tunnel(client net.Conn, buffered io.Reader, upstream net.Conn) {
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = client.Close()
			_ = upstream.Close()
		})
	}
	touch := func() {
		deadline := time.Now().Add(tunnelIdle)
		_ = client.SetDeadline(deadline)
		_ = upstream.SetDeadline(deadline)
	}
	touch()
	var wg sync.WaitGroup
	copy := func(dst io.Writer, src io.Reader) {
		defer wg.Done()
		defer stop()
		buf := make([]byte, 32<<10)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				touch()
				if _, werr := dst.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}
	wg.Add(2)
	go copy(upstream, buffered)
	go copy(client, upstream)
	wg.Wait()
}

// hopByHop are headers that belong to the proxy hop and are not forwarded.
var hopByHop = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Proxy-Connection",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// upgrade forwards an absolute-URI http:// request to the same host over
// HTTPS, adding the host's credential.
func (p *Proxy) upgrade(w http.ResponseWriter, r *http.Request) {
	if !r.URL.IsAbs() || r.URL.Scheme != "http" {
		p.observe("http", OutcomeRefused)
		http.Error(w, "egress: only absolute http:// requests and CONNECT are served", http.StatusBadRequest)
		return
	}
	host := r.URL.Hostname()
	rules := p.Rules()
	if (r.URL.Port() != "" && r.URL.Port() != "80") || !rules.Allows(host) {
		p.observe("http", OutcomeRefused)
		p.Logger.Warn("egress refused", "kind", "http", "host", r.URL.Host)
		http.Error(w, "egress: destination not allowed", http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), upgradeTimeout)
	defer cancel()
	target := *r.URL
	target.Scheme, target.Host = "https", host
	req, err := http.NewRequestWithContext(ctx, r.Method, target.String(), r.Body)
	if err != nil {
		p.observe("http", OutcomeError)
		http.Error(w, "egress: "+err.Error(), http.StatusBadRequest)
		return
	}
	req.ContentLength = r.ContentLength
	req.Header = r.Header.Clone()
	for _, h := range hopByHop {
		req.Header.Del(h)
	}
	req.Header.Del("Authorization")
	if cred, ok := rules.Credentials[strings.ToLower(host)]; ok {
		req.Header.Set("Authorization", cred)
	}
	client := p.Client
	if client == nil {
		client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	resp, err := client.Do(req)
	if err != nil {
		p.observe("http", OutcomeError)
		p.Logger.Warn("egress request failed", "host", host, "error", err)
		http.Error(w, "egress: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	p.observe("http", OutcomeAllowed)
	p.Logger.Debug("egress request", "host", host, "method", r.Method, "path", r.URL.Path, "status", resp.StatusCode)
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// ProxyURL checks a gateway URL a runner will be handed: http, a host, no
// path.
func ProxyURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("egress: gateway url: %w", err)
	}
	if u.Scheme != "http" || u.Host == "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("egress: gateway url must be http://host:port")
	}
	return u, nil
}
