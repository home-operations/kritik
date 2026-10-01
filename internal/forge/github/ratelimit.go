package github

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"time"
)

// rateLimitMaxWait bounds how long a request waits out a rate limit before
// it is sent again. A longer wait is not worth holding a worker for: the
// response goes back to go-github, which then refuses every request until
// the reset without asking GitHub, and to the caller, whose job River
// retries later.
const rateLimitMaxWait = time.Minute

// rateLimitTransport sends a request GitHub refused for a rate limit once
// more when the limit resets soon. GitHub's secondary limits, on content
// creation above all, answer with a Retry-After of seconds to tens of
// seconds; without the wait every such refusal fails a review or a poll
// outright, and River's retry, which knows nothing of Retry-After, may
// come back inside the same window.
type rateLimitTransport struct {
	base    http.RoundTripper
	maxWait time.Duration
	// observe, when set, is told of every rate-limited response: the wait
	// it asked for, and whether the request waited it out and was resent.
	observe func(wait time.Duration, waited bool)
	// sleep overrides the wait in tests.
	sleep func(ctx context.Context, d time.Duration) error
}

func (t *rateLimitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	wait, limited := rateLimitWait(resp, time.Now())
	if !limited {
		return resp, nil
	}
	// A body that cannot be read again cannot be sent again.
	retry := wait <= t.maxWait && (req.Body == nil || req.Body == http.NoBody || req.GetBody != nil)
	if t.observe != nil {
		t.observe(wait, retry)
	}
	if !retry {
		return resp, nil
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	if err := t.wait(req.Context(), wait); err != nil {
		return nil, err
	}
	again := req.Clone(req.Context())
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		again.Body = body
	}
	return t.base.RoundTrip(again)
}

func (t *rateLimitTransport) wait(ctx context.Context, d time.Duration) error {
	if t.sleep != nil {
		return t.sleep(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// rateLimitWait reads how long a response says to wait. GitHub answers a
// rate-limited request with 403 or 429 and either Retry-After, in seconds,
// for a secondary limit, or x-ratelimit-remaining 0 with the reset as an
// epoch for the primary one; the wait is never under a second, and a reset
// gets a second's grace for clock skew. Any other response is not a limit.
func rateLimitWait(resp *http.Response, now time.Time) (time.Duration, bool) {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return 0, false
	}
	if v := resp.Header.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil {
			return max(time.Duration(secs)*time.Second, time.Second), true
		}
	}
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		if epoch, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			return max(time.Unix(epoch, 0).Sub(now)+time.Second, time.Second), true
		}
	}
	return 0, false
}
