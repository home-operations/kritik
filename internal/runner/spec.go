package runner

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/home-operations/kritik/internal/gitfetch"
	"github.com/home-operations/kritik/internal/repoconfig"
	"github.com/home-operations/kritik/internal/review"
)

// SpecVersion is the only job document version this runner understands. A
// worker and runner on different images must agree on it, so a runner
// refuses any other version instead of guessing at its meaning.
const SpecVersion = 2

// HeartbeatInterval is how often a runner stamps runner_runs.heartbeat_at.
// The worker's staleness threshold is several of these.
const HeartbeatInterval = 15 * time.Second

// Kind is what a run does.
type Kind string

// Kinds of run.
const (
	// KindReview fetches head and merge-base, diffs them and writes a
	// context pack.
	KindReview Kind = "review"
	// KindIndex chunks a tree into the index staging table.
	KindIndex Kind = "index"
)

// Valid reports whether k is a kind of run the runner implements.
func (k Kind) Valid() bool { return k == KindReview || k == KindIndex }

func (k Kind) String() string { return string(k) }

// Mode is how a review is carried out.
type Mode string

// Review modes. The empty mode is single.
const (
	ModeSingle  Mode = "single"
	ModeAgentic Mode = "agentic"
)

// Valid reports whether m is a review mode; the empty mode is single.
func (m Mode) Valid() bool { return m == "" || m == ModeSingle || m == ModeAgentic }

func (m Mode) String() string { return string(m) }

// ModelEndpoint is where an agentic run's model calls go: the worker's
// gateway, which holds the provider key, picks the provider model and its
// fallbacks, and counts what the run spends (ADR-0004). The run's token for
// it reaches the pod as a job-scoped secret.
type ModelEndpoint struct {
	// GatewayURL is the gateway's address, http://host:port.
	GatewayURL string `json:"gatewayUrl"`
	// Model is the name the gateway knows the run's model by.
	Model string `json:"model"`
}

// AgentLimits bound an agentic run. A zero limit takes the agent loop's
// default; a zero timeout leaves the tool loop to the Job deadline.
type AgentLimits struct {
	MaxSteps           int   `json:"maxSteps"`
	MaxToolOutputBytes int   `json:"maxToolOutputBytes"`
	MaxTokens          int64 `json:"maxTokens"`
	TimeoutSeconds     int   `json:"timeoutSeconds,omitempty"`
	// Commands name the binaries the run tool may execute; the runner
	// offers those it finds on its PATH, and no run tool without any.
	Commands              []string `json:"commands,omitempty"`
	CommandTimeoutSeconds int      `json:"commandTimeoutSeconds,omitempty"`
}

// Prompt is what an agentic run needs beyond the checkout to write its
// review prompt and to tell whether the worker will skip the review: the
// pull request, the operator's review defaults the merge-base .kritik.yaml
// may override, and the last completed review's findings.
type Prompt struct {
	Repository string `json:"repository"`
	// PullRequest is also what the merge-base .kritik.yaml filter sees.
	PullRequest repoconfig.PullRequest `json:"pullRequest"`
	// Instructions name repository files, as the operator's review
	// settings do.
	Instructions        []string `json:"instructions,omitempty"`
	RequireSuggestedFix bool     `json:"requireSuggestedFix,omitempty"`
	// MaxDeltaFiles is the incremental re-review threshold.
	MaxDeltaFiles int              `json:"maxDeltaFiles"`
	Prior         []review.Finding `json:"prior,omitempty"`
	// UnchangedPatchID, when the head's patch id equals it, means the
	// worker will skip the review, so the agent is not run.
	UnchangedPatchID string `json:"unchangedPatchId,omitempty"`
}

// Spec is the job document a worker hands a runner: everything the run
// needs except its secrets.
type Spec struct {
	Version int    `json:"version"`
	Kind    Kind   `json:"kind"`
	RunID   string `json:"runId"`
	// CloneURL is fetched with Secrets.GitToken.
	CloneURL string `json:"cloneUrl"`
	// Head is the commit under review or to index. Base is the merge-base
	// for a review and the previously indexed commit for an incremental
	// index. PriorHead is the head of the last completed review.
	Head      string `json:"head"`
	Base      string `json:"base,omitempty"`
	PriorHead string `json:"priorHead,omitempty"`
	// Ignore globs are skipped by the context stages.
	Ignore []string `json:"ignore,omitempty"`
	// RepoFiles are repository paths the operator's review settings name,
	// read from the merge base alongside .kritik.yaml.
	RepoFiles []string       `json:"repoFiles,omitempty"`
	Mode      Mode           `json:"mode,omitempty"`
	Agent     *AgentLimits   `json:"agent,omitempty"`
	Model     *ModelEndpoint `json:"model,omitempty"`
	Prompt    *Prompt        `json:"prompt,omitempty"`
}

// Validate checks a spec is one this runner can carry out.
func (s Spec) Validate() error {
	if s.Version != SpecVersion {
		return fmt.Errorf("runner: spec version %d is not supported (want %d)", s.Version, SpecVersion)
	}
	if !s.Kind.Valid() {
		return fmt.Errorf("runner: spec kind %q is not review or index", s.Kind)
	}
	if !s.Mode.Valid() {
		return fmt.Errorf("runner: spec mode %q is not single or agentic", s.Mode)
	}
	if s.RunID == "" || s.CloneURL == "" {
		return errors.New("runner: spec needs runId and cloneUrl")
	}
	if !gitfetch.IsSHA(s.Head) {
		return fmt.Errorf("runner: spec head %q is not a commit SHA", s.Head)
	}
	if s.Kind == KindReview && s.Base == "" {
		return errors.New("runner: a review spec needs a base")
	}
	for name, sha := range map[string]string{"base": s.Base, "priorHead": s.PriorHead} {
		if sha != "" && !gitfetch.IsSHA(sha) {
			return fmt.Errorf("runner: spec %s %q is not a commit SHA", name, sha)
		}
	}
	if s.Mode == ModeAgentic {
		if s.Agent == nil {
			return errors.New("runner: an agentic spec needs agent limits")
		}
		if s.Model == nil || s.Model.Model == "" || s.Model.GatewayURL == "" {
			return errors.New("runner: an agentic spec needs a model and the gateway to reach it through")
		}
		if s.Prompt == nil {
			return errors.New("runner: an agentic spec needs a prompt")
		}
		if len(s.Agent.Commands) > 0 && s.Agent.CommandTimeoutSeconds <= 0 {
			return errors.New("runner: an agentic spec with commands needs a command timeout")
		}
		for _, c := range s.Agent.Commands {
			// A name with a separator would make exec.LookPath take it as a path.
			if c == "" || strings.ContainsRune(c, '/') {
				return fmt.Errorf("runner: spec command %q is not a bare command name", c)
			}
		}
	}
	return nil
}

// Spec size bounds. A spec travels as a key of the run's Secret, which
// Kubernetes caps at 1 MiB with the credentials beside it, so what grows
// with a pull request is cut before it is encoded.
const (
	// MaxSpecBytes bounds an encoded spec, leaving the Secret room for the
	// git and gateway tokens.
	MaxSpecBytes = 900 << 10
	// MaxPriorFindings is how many of the last review's findings a spec
	// carries.
	MaxPriorFindings = 200
	// MaxBodyBytes bounds the pull request body a spec carries. Encoding
	// can grow it sixfold (each '<' becomes \u003c), which the spec bound
	// still holds.
	MaxBodyBytes = 64 << 10
)

// maxPromptBytes is what Trim leaves an encoded prompt, under MaxSpecBytes
// with room for the rest of the spec.
const maxPromptBytes = 768 << 10

// Trim cuts what a pull request can grow without bound to what a spec
// carries: the body, at a rune boundary, and the prior findings, first to
// MaxPriorFindings and then, while the encoded prompt is still over its
// share of the spec, by half at a time, keeping the first in the order the
// worker read them.
func (p *Prompt) Trim() {
	if body := p.PullRequest.Body; len(body) > MaxBodyBytes {
		n := MaxBodyBytes
		for n > 0 && !utf8.RuneStart(body[n]) {
			n--
		}
		p.PullRequest.Body = body[:n]
	}
	if len(p.Prior) > MaxPriorFindings {
		p.Prior = p.Prior[:MaxPriorFindings]
	}
	for len(p.Prior) > 0 {
		b, err := json.Marshal(p)
		if err != nil || len(b) <= maxPromptBytes {
			return
		}
		p.Prior = p.Prior[:len(p.Prior)/2]
	}
}

// EncodeSpec is the job document a runner reads, refused when it is too
// large to deliver.
func EncodeSpec(s Spec) ([]byte, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("runner: encode spec: %w", err)
	}
	if len(b) > MaxSpecBytes {
		return nil, fmt.Errorf("runner: encoded spec is %d bytes, over the %d byte limit", len(b), MaxSpecBytes)
	}
	return b, nil
}

// ReadSpec reads and strictly decodes the job document at path.
func ReadSpec(path string) (Spec, error) {
	f, err := os.Open(path)
	if err != nil {
		return Spec{}, fmt.Errorf("runner: read spec: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, MaxSpecBytes+1))
	if err != nil {
		return Spec{}, fmt.Errorf("runner: read spec: %w", err)
	}
	if len(data) > MaxSpecBytes {
		return Spec{}, fmt.Errorf("runner: spec at %s is over the %d byte limit", path, MaxSpecBytes)
	}
	return DecodeSpec(data)
}

// DecodeSpec parses a job document strictly: a field this runner does not
// know is an error, not something to ignore.
func DecodeSpec(data []byte) (Spec, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var s Spec
	if err := dec.Decode(&s); err != nil {
		return Spec{}, fmt.Errorf("runner: decode spec: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Spec{}, errors.New("runner: decode spec: trailing data after the document")
	}
	if err := s.Validate(); err != nil {
		return Spec{}, err
	}
	return s, nil
}

// Secrets are a run's credentials, delivered apart from the spec: the git
// token it fetches with and, for an agentic run, its token for the model
// gateway.
type Secrets struct {
	GitToken     string
	GatewayToken string
}

// Mask replaces every occurrence of each non-empty secret in text with
// "***". Longer secrets go first so one containing another is masked whole.
func (s Secrets) Mask(text string) string {
	values := []string{s.GitToken, s.GatewayToken}
	slices.SortFunc(values, func(a, b string) int { return len(b) - len(a) })
	for _, v := range values {
		if v != "" {
			text = strings.ReplaceAll(text, v, "***")
		}
	}
	return text
}
