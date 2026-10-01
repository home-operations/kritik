package runner

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

// gatewayWait bounds how long a runner waits for its proxy, the gateway,
// to answer before it fetches. A runner started as the service comes up
// can beat the gateway's Service endpoints by the readiness probe's
// period and a little propagation; much longer than that is an outage the
// run should report rather than sit out.
const gatewayWait = 90 * time.Second

// gatewayPoll is how often the wait tries again; a variable for the tests.
var gatewayPoll = 2 * time.Second

// waitForGateway blocks until the HTTPS proxy the pod was handed answers a
// request, or gatewayWait passes. Any HTTP answer counts, a 404 included:
// the gateway is up and the Service routes to it. Without a proxy there is
// nothing to wait for.
func waitForGateway(ctx context.Context, logger *slog.Logger) error {
	url := proxyURL()
	if url == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, gatewayWait)
	defer cancel()
	// A zero Transport reads no proxy from the environment: the probe
	// goes to the gateway, not through it.
	client := &http.Client{Timeout: gatewayPoll, Transport: &http.Transport{}}
	start := time.Now()
	for attempt := 1; ; attempt++ {
		err := probeGateway(ctx, client, url)
		if err == nil {
			if attempt > 1 {
				logger.Info("gateway reachable", "url", url, "attempts", attempt, "waited", time.Since(start).Round(time.Millisecond))
			}
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("runner: the gateway at %s did not answer within %s: %w", url, gatewayWait, err)
		}
		if attempt == 1 {
			logger.Info("waiting for the gateway", "url", url, "error", err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("runner: the gateway at %s did not answer within %s: %w", url, gatewayWait, err)
		case <-time.After(gatewayPoll):
		}
	}
}

// probeGateway makes one request to the gateway itself, not through it.
func probeGateway(ctx context.Context, client *http.Client, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

// httpsProxyEnv names the proxy gitfetch and the run tool read.
const httpsProxyEnv = "HTTPS_PROXY"

// proxyURL is the HTTPS proxy the pod was handed, under either spelling,
// as gitfetch and the run tool read it.
func proxyURL() string {
	for _, name := range []string{httpsProxyEnv, strings.ToLower(httpsProxyEnv)} {
		if v := os.Getenv(name); v != "" {
			return v
		}
	}
	return ""
}
