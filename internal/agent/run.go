package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/home-operations/kritik/internal/model"
)

// Source bounds: how many URLs one run records, and how long each may be.
const (
	maxSources     = 50
	maxSourceBytes = 2048
)

// sourceEscaper percent-encodes what would end a Markdown autolink or a
// code span early, so a recorded URL renders as one link and nothing else.
var sourceEscaper = strings.NewReplacer(" ", "%20", "<", "%3C", ">", "%3E", "`", "%60")

// RunConfig configures the run tool.
type RunConfig struct {
	// Dir is the checkout the commands run in.
	Dir string
	// Env is the commands' whole environment.
	Env []string
	// Commands map each name the model may run to the binary it runs.
	Commands map[string]string
	// Timeout bounds one command.
	Timeout time.Duration
	// MaxOutputBytes caps the combined output kept of one command.
	MaxOutputBytes int
	// Proxied says the commands reach the network through the egress
	// gateway, which fetches a plain http:// URL over HTTPS.
	Proxied bool
	// Note, when set, is appended to the tool's description.
	Note string
}

// RunTool executes one allowlisted binary with the model's arguments,
// directly and without a shell, in a checkout of the head commit
// (ADR-0008), and records every http(s) URL curl is given as a source the
// review consulted.
type RunTool struct {
	cfg     RunConfig
	names   []string
	schema  json.RawMessage
	sources []string
}

// NewRunTool builds the run tool over c.Commands.
func NewRunTool(c RunConfig) *RunTool {
	names := make([]string, 0, len(c.Commands))
	for name := range c.Commands {
		names = append(names, name)
	}
	slices.Sort(names)
	// A []string always encodes.
	enum, _ := json.Marshal(names)
	schema := fmt.Sprintf(`{
	"type": "object",
	"properties": {
		"command": {"type": "string", "enum": %s, "description": "The command to run."},
		"args": {"type": "array", "items": {"type": "string"}, "description": "Arguments, one per element, passed exactly as given."}
	},
	"required": ["command"],
	"additionalProperties": false
}`, enum)
	return &RunTool{cfg: c, names: names, schema: json.RawMessage(schema)}
}

// Names are the commands the tool offers, sorted.
func (rt *RunTool) Names() []string { return rt.names }

// Sources are the URLs curl was given, in first-use order, never nil.
func (rt *RunTool) Sources() []string { return append([]string{}, rt.sources...) }

func (rt *RunTool) Def() model.ToolDef {
	desc := fmt.Sprintf("Run one of these commands in a checkout of the head commit: %s. The command runs directly, "+
		"without a shell: arguments are passed exactly as given, with no globbing, pipes or redirection. It is stopped "+
		"after %s, and its exit code and combined output are returned.", strings.Join(rt.names, ", "), rt.cfg.Timeout)
	if slices.Contains(rt.names, "curl") {
		if rt.cfg.Proxied {
			desc += " The network is reached through a gateway that allows only some hosts. Give curl http:// URLs: " +
				"the gateway fetches them over HTTPS and authenticates to the hosts it holds a credential for."
		} else {
			desc += " Give curl https:// URLs."
		}
	}
	if rt.cfg.Note != "" {
		desc += " " + rt.cfg.Note
	}
	return model.ToolDef{Name: "run", Description: desc, InputSchema: rt.schema}
}

func (rt *RunTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	var req struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}
	if err := decodeInput(input, &req); err != nil {
		return "", fmt.Errorf("agent: run: %w", err)
	}
	bin, ok := rt.cfg.Commands[req.Command]
	if !ok {
		return "", fmt.Errorf("agent: run: %q is not one of %s", req.Command, strings.Join(rt.names, ", "))
	}
	if req.Command == "curl" {
		rt.record(req.Args)
	}

	cctx, cancel := context.WithTimeout(ctx, rt.cfg.Timeout)
	defer cancel()
	out := &cappedBuffer{max: rt.cfg.MaxOutputBytes}
	cmd := exec.CommandContext(cctx, bin, req.Args...)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = rt.cfg.Dir, rt.cfg.Env, out, out
	// Once the command is killed, a child still holding its output open is
	// not waited for.
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	switch {
	case ctx.Err() != nil:
		return "", ctx.Err()
	case cctx.Err() != nil:
		return fmt.Sprintf("stopped after %s\n%s", rt.cfg.Timeout, out), nil
	case err != nil:
		if _, ok := errors.AsType[*exec.ExitError](err); !ok {
			return "", fmt.Errorf("agent: run: %s: %w", req.Command, err)
		}
	}
	return fmt.Sprintf("exit code %d\n%s", cmd.ProcessState.ExitCode(), out), nil
}

// record keeps each http(s) URL in args as a source, without any
// credentials it carries.
func (rt *RunTool) record(args []string) {
	for _, a := range args {
		u, err := url.Parse(a)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			continue
		}
		u.User = nil
		if rt.cfg.Proxied {
			u.Scheme = "https"
		}
		s := sourceEscaper.Replace(u.String())
		if len(rt.sources) < maxSources && len(s) <= maxSourceBytes && !slices.Contains(rt.sources, s) {
			rt.sources = append(rt.sources, s)
		}
	}
}

// cappedBuffer keeps the first max bytes written to it and counts the
// rest, so a command's output never grows past what the tool returns.
type cappedBuffer struct {
	buf     bytes.Buffer
	max     int
	dropped int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	keep := min(max(b.max-b.buf.Len(), 0), len(p))
	b.buf.Write(p[:keep])
	b.dropped += len(p) - keep
	return len(p), nil
}

// String is the kept output as valid UTF-8, with a note of what was cut.
func (b *cappedBuffer) String() string {
	s := strings.ToValidUTF8(b.buf.String(), "�")
	if b.dropped > 0 {
		s += fmt.Sprintf("\n[truncated %d bytes]", b.dropped)
	}
	return s
}
