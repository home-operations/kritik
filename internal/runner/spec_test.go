package runner

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf8"

	"github.com/home-operations/kritika/internal/repoconfig"
	"github.com/home-operations/kritika/internal/review"
)

const (
	shaA = "0123456789abcdef0123456789abcdef01234567"
	shaB = "89abcdef0123456789abcdef0123456789abcdef"
)

func reviewSpec() Spec {
	return Spec{
		Version: SpecVersion, Kind: KindReview, RunID: "run-1", CloneURL: "https://forge.example.com/acme/widgets.git",
		Head: shaA, Base: shaB, Ignore: []string{"vendor/**"}, RepoFiles: []string{"docs/rules.md"},
		Agent: &AgentLimits{MaxSteps: 30, MaxToolOutputBytes: 16 << 10, MaxTokens: 200000},
		Model: &ModelEndpoint{GatewayURL: "http://kritika-gateway:8082", Model: "review"},
		Prompt: &Prompt{
			Repository: "acme/widgets",
			PullRequest: repoconfig.PullRequest{Number: 7, Title: "Add b", Author: "octocat", Body: "Adds b.", BaseRef: "main", State: "open",
				Labels: []byte(`[{"name":"deps"}]`)},
			MaxDeltaFiles: 25, Prior: []review.Finding{{Path: "main.go", Line: 1, Severity: review.SeverityNit, Title: "earlier finding"}}},
	}
}

func TestDecodeSpec(t *testing.T) {
	encode := func(s Spec) string {
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	tests := []struct {
		name    string
		in      string
		wantErr string
	}{
		{name: "valid review", in: encode(reviewSpec())},
		{name: "valid index without base", in: encode(Spec{Version: SpecVersion, Kind: KindIndex, RunID: "r", CloneURL: "u", Head: shaA})},
		{name: "unknown version", in: strings.Replace(encode(reviewSpec()), `"version":10`, `"version":11`, 1), wantErr: "version"},
		{name: "unknown field", in: strings.Replace(encode(reviewSpec()), `{`, `{"token":"x",`, 1), wantErr: "unknown field"},
		{name: "bad head sha", in: strings.Replace(encode(reviewSpec()), shaA, "abc", 1), wantErr: "head"},
		{name: "uppercase sha", in: strings.Replace(encode(reviewSpec()), shaA, strings.ToUpper(shaA), 1), wantErr: "head"},
		{name: "bad prior head", in: func() string { s := reviewSpec(); s.PriorHead = "zz"; return encode(s) }(), wantErr: "priorHead"},
		{name: "review without base", in: func() string { s := reviewSpec(); s.Base = ""; return encode(s) }(), wantErr: "base"},
		{name: "unknown kind", in: func() string { s := reviewSpec(); s.Kind = "lint"; return encode(s) }(), wantErr: "kind"},
		{name: "a mode", in: strings.Replace(encode(reviewSpec()), `{`, `{"mode":"agentic",`, 1), wantErr: "unknown field"},
		{name: "missing run id", in: func() string { s := reviewSpec(); s.RunID = ""; return encode(s) }(), wantErr: "runId"},
		{name: "a review without model", in: func() string { s := reviewSpec(); s.Model = nil; return encode(s) }(), wantErr: "model"},
		{name: "a review without limits", in: func() string { s := reviewSpec(); s.Agent = nil; return encode(s) }(), wantErr: "agent"},
		{name: "a review without a prompt", in: func() string { s := reviewSpec(); s.Prompt = nil; return encode(s) }(), wantErr: "prompt"},
		{name: "a review without the gateway", in: func() string { s := reviewSpec(); s.Model.GatewayURL = ""; return encode(s) }(), wantErr: "gateway"},
		{name: "a version 4 document", in: func() string { s := reviewSpec(); s.Version = 4; return encode(s) }(), wantErr: "version 4"},
		{name: "valid with commands", in: func() string {
			s := reviewSpec()
			s.Agent.Commands, s.Agent.CommandTimeoutSeconds = []string{"curl", "rg"}, 30
			return encode(s)
		}()},
		{name: "commands without a timeout", in: func() string { s := reviewSpec(); s.Agent.Commands = []string{"rg"}; return encode(s) }(),
			wantErr: "command timeout"},
		{name: "a command given as a path", in: func() string {
			s := reviewSpec()
			s.Agent.Commands, s.Agent.CommandTimeoutSeconds = []string{"../../tmp/x"}, 30
			return encode(s)
		}(), wantErr: "bare command name"},
		{name: "trailing data", in: encode(reviewSpec()) + "{}", wantErr: "trailing"},
		{name: "not json", in: "nope", wantErr: "runner"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DecodeSpec([]byte(tt.in))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("DecodeSpec: %v", err)
				}
				if got.Version != SpecVersion || got.Head != shaA {
					t.Fatalf("decoded = %+v", got)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestSpecRoundTripKeepsAgentFields(t *testing.T) {
	want := reviewSpec()
	b, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeSpec(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Agent.MaxSteps != 30 || got.Model.Model != "review" ||
		got.Model.GatewayURL != "http://kritika-gateway:8082" || got.Prompt.PullRequest.Title != "Add b" || len(got.Prompt.Prior) != 1 {
		t.Fatalf("round trip = %+v %+v %+v", got, got.Agent, got.Model)
	}
}

func TestSecretsMask(t *testing.T) {
	tests := []struct {
		name    string
		secrets Secrets
		in      string
		want    string
	}{
		{name: "empty secrets leave text untouched", in: "clone ok", want: "clone ok"},
		{name: "both replaced", secrets: Secrets{GitToken: "tok-123", GatewayToken: "key-456"},
			in: "auth tok-123 and key-456 twice tok-123", want: "auth *** and *** twice ***"},
		{name: "overlapping secrets mask the longer whole", secrets: Secrets{GitToken: "abc", GatewayToken: "abcdef"},
			in: "x abcdef y abc", want: "x *** y ***"},
		{name: "only one set", secrets: Secrets{GatewayToken: "key"}, in: "key", want: "***"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.secrets.Mask(tt.in); got != tt.want {
				t.Fatalf("Mask = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHeartbeatBeatsOnInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const interval = time.Second
		var beats atomic.Int32
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			heartbeat(ctx, interval, func(context.Context) error {
				beats.Add(1)
				return nil
			}, slog.New(slog.DiscardHandler))
			close(done)
		}()
		synctest.Wait()
		if n := beats.Load(); n != 1 {
			t.Fatalf("beats = %d at start, want 1 (one immediately)", n)
		}
		time.Sleep(3 * interval)
		synctest.Wait()
		if n := beats.Load(); n != 4 {
			t.Fatalf("beats = %d after 3 intervals, want 4", n)
		}
		cancel()
		<-done
		time.Sleep(3 * interval)
		synctest.Wait()
		if n := beats.Load(); n != 4 {
			t.Fatalf("beats = %d after cancel, want 4: heartbeat kept beating after its context ended", n)
		}
	})
}

func TestPromptTrim(t *testing.T) {
	finding := func(explanation string) review.Finding {
		return review.Finding{Path: "main.go", Line: 1, Severity: review.SeverityNit, Title: "t", Explanation: explanation}
	}
	findings := func(n int, explanation string) []review.Finding {
		out := make([]review.Finding, n)
		for i := range out {
			out[i] = finding(explanation)
		}
		return out
	}
	// A three-byte rune straddles the body limit.
	straddling := strings.Repeat("a", MaxBodyBytes-1) + "€" + "tail"
	tests := []struct {
		name      string
		body      string
		prior     []review.Finding
		wantBody  int
		wantPrior int
	}{
		{name: "small prompt unchanged", body: "Adds b.", prior: findings(3, "x"), wantBody: 7, wantPrior: 3},
		{name: "too many findings", body: "b", prior: findings(250, "x"), wantBody: 1, wantPrior: MaxPriorFindings},
		{name: "body cut at a rune boundary", body: straddling, wantBody: MaxBodyBytes - 1},
		{name: "huge findings halved until they fit", body: "b", prior: findings(MaxPriorFindings, strings.Repeat("<", 4<<10)),
			wantBody: 1, wantPrior: 25},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &Prompt{PullRequest: repoconfig.PullRequest{Body: tt.body}, Prior: tt.prior,
				Issues: []review.Issue{{Number: 1, Body: strings.Repeat("a", MaxIssueBytes+5)}}}
			p.Trim()
			if len(p.PullRequest.Body) != tt.wantBody || len(p.Prior) != tt.wantPrior || !utf8.ValidString(p.PullRequest.Body) {
				t.Fatalf("body = %d bytes, prior = %d; want %d, %d", len(p.PullRequest.Body), len(p.Prior), tt.wantBody, tt.wantPrior)
			}
			if len(p.Issues[0].Body) != MaxIssueBytes {
				t.Fatalf("issue body = %d bytes, want %d", len(p.Issues[0].Body), MaxIssueBytes)
			}
		})
	}
}

func TestEncodeSpecWorstCaseFits(t *testing.T) {
	s := reviewSpec()
	// Every '<' encodes as six bytes.
	s.Prompt.PullRequest.Body = strings.Repeat("<", 1<<20)
	for range review.MaxLinkedIssues {
		s.Prompt.Issues = append(s.Prompt.Issues, review.Issue{Number: 1, Title: "t", Body: strings.Repeat("<", 1<<20)})
	}
	s.Prompt.Prior = make([]review.Finding, 1000)
	for i := range s.Prompt.Prior {
		s.Prompt.Prior[i] = review.Finding{Path: "main.go", Line: i + 1, Severity: review.SeverityNit, Title: "t",
			Explanation: strings.Repeat("<", 2<<10)}
	}
	if _, err := EncodeSpec(s); err == nil {
		t.Fatal("an untrimmed spec over the limit must be refused")
	}
	s.Prompt.Trim()
	b, err := EncodeSpec(s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeSpec(b); err != nil {
		t.Fatal(err)
	}
}

func TestReadSpec(t *testing.T) {
	dir := t.TempDir()
	b, err := EncodeSpec(reviewSpec())
	if err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(dir, "spec.json")
	big := filepath.Join(dir, "big.json")
	if err := os.WriteFile(good, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(big, []byte(strings.Repeat(" ", MaxSpecBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadSpec(good); err != nil || got.Prompt.PullRequest.Title != "Add b" {
		t.Fatalf("ReadSpec = %+v, %v", got, err)
	}
	for path, want := range map[string]string{big: "byte limit", filepath.Join(dir, "missing.json"): "read spec"} {
		if _, err := ReadSpec(path); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("ReadSpec(%s) = %v, want %q", path, err, want)
		}
	}
}
