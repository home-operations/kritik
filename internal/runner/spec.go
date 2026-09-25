package runner

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/home-operations/kritik/internal/model"
)

// SpecVersion is the only job document version this runner understands. A
// worker and runner on different images must agree on it, so a runner
// refuses any other version instead of guessing at its meaning.
const SpecVersion = 1

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

// ModelEndpoint is the model an agentic run talks to. The key is not here:
// it reaches the pod as a job-scoped secret.
type ModelEndpoint struct {
	Provider  model.ProviderType `json:"provider"`
	BaseURL   string             `json:"baseUrl,omitempty"`
	Model     string             `json:"model"`
	Fallbacks []string           `json:"fallbacks,omitempty"`
	Pricing   model.Pricing      `json:"pricing,omitempty"`
}

// AgentLimits bound an agentic run.
type AgentLimits struct {
	MaxSteps           int   `json:"maxSteps"`
	MaxToolOutputBytes int   `json:"maxToolOutputBytes"`
	MaxTokens          int64 `json:"maxTokens"`
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
	Ignore []string       `json:"ignore,omitempty"`
	Mode   Mode           `json:"mode,omitempty"`
	Agent  *AgentLimits   `json:"agent,omitempty"`
	Model  *ModelEndpoint `json:"model,omitempty"`
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
	if !isSHA(s.Head) {
		return fmt.Errorf("runner: spec head %q is not a commit SHA", s.Head)
	}
	if s.Kind == KindReview && s.Base == "" {
		return errors.New("runner: a review spec needs a base")
	}
	for name, sha := range map[string]string{"base": s.Base, "priorHead": s.PriorHead} {
		if sha != "" && !isSHA(sha) {
			return fmt.Errorf("runner: spec %s %q is not a commit SHA", name, sha)
		}
	}
	if s.Mode == ModeAgentic {
		if s.Agent == nil {
			return errors.New("runner: an agentic spec needs agent limits")
		}
		if s.Model == nil || s.Model.Model == "" {
			return errors.New("runner: an agentic spec needs a model endpoint")
		}
		if !s.Model.Provider.Valid() {
			return fmt.Errorf("runner: spec model provider %q is not supported", s.Model.Provider)
		}
	}
	return nil
}

// isSHA reports whether s is a full lowercase SHA-1 commit id.
func isSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
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

// Secrets are a run's credentials, delivered apart from the spec.
type Secrets struct {
	GitToken    string
	ModelAPIKey string
}

// Mask replaces every occurrence of each non-empty secret in text with
// "***". Longer secrets go first so one containing another is masked whole.
func (s Secrets) Mask(text string) string {
	values := []string{s.GitToken, s.ModelAPIKey}
	slices.SortFunc(values, func(a, b string) int { return len(b) - len(a) })
	for _, v := range values {
		if v != "" {
			text = strings.ReplaceAll(text, v, "***")
		}
	}
	return text
}
