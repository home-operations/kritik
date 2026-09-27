package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/home-operations/kritik/internal/agent"
	"github.com/home-operations/kritik/internal/chunk"
	"github.com/home-operations/kritik/internal/gitfetch"
	"github.com/home-operations/kritik/internal/model"
	"github.com/home-operations/kritik/internal/repoconfig"
	"github.com/home-operations/kritik/internal/store"
	"github.com/home-operations/kritik/internal/tasks"
)

// taskSubmit is the tool whose input is a task's answer.
const taskSubmit = "submit_answer"

const taskSubmitDescription = "Submit the answer and end the task. The input is the whole answer, in the shape its schema gives. " +
	"Call it exactly once, when you are done."

// taskFilesSource is the context source glob files are gathered under,
// as .Context.files holds the files a task names by path.
const taskFilesSource = "files"

// runTask fetches the commit the task was defined at, gathers the context
// sources only a checkout can, and runs the agent with the task's prompts
// and answer schema, as the worker rendered them.
func runTask(ctx context.Context, st *store.Store, p Spec, secrets Secrets, logger *slog.Logger) error {
	if err := setPhase(ctx, st, p.RunID, "fetching"); err != nil {
		return err
	}
	res, err := gitfetch.Run(ctx, gitfetch.Fetch{CloneURL: p.CloneURL, Token: secrets.GitToken, Head: p.Head})
	if err != nil {
		return err
	}
	defer func() { _ = res.Close() }()
	head, err := res.Head.Tree()
	if err != nil {
		return fmt.Errorf("runner: head tree: %w", err)
	}
	if err := setPhase(ctx, st, p.RunID, "reviewing"); err != nil {
		return err
	}
	stepper, err := gatewayStepper(p, secrets)
	if err != nil {
		return err
	}
	out, timeline, sources := taskAgent(ctx, stepper, p, head, logger)
	return recordAgent(ctx, st, p, secrets, out, timeline, sources, logger)
}

// taskAgent runs the task's agent over head: the worker's user prompt
// followed by the context gathered here, the tools the task names, and
// its answer schema as the submit tool's. It returns the sources the
// commands fetched.
func taskAgent(
	ctx context.Context, stepper model.Stepper, p Spec, head *object.Tree, logger *slog.Logger,
) (agent.Result, []store.TimelineStep, []string) {
	tree := agent.NewTree(head, p.Ignore)
	maxOutput := p.Agent.limits().MaxToolOutputBytes
	run, cleanup := commandTool(ctx, p, tree, maxOutput, logger)
	defer cleanup()
	gathered, notes := gatherTask(ctx, p.Task, head, p.Ignore, run)
	for _, n := range notes {
		logger.Info("task context", "note", n)
	}
	user := strings.Join(append([]string{p.Task.User}, gathered...), "\n\n")
	agentRun := run
	if run != nil {
		agentRun = run.Only(p.Task.Run)
	}
	tools := taskTools(p.Task, tree, maxOutput, agentRun)
	logger.Info("agent started", "task", p.Task.Name, "model", p.Model.Model, "prompt_chars", len(p.Task.System)+len(user),
		"tools", len(tools))
	submit := model.ToolDef{Name: taskSubmit, Description: taskSubmitDescription, InputSchema: p.Task.Schema}
	out, timeline := agentLoop(ctx, stepper, p, tools, submit, p.Task.System, user, time.Duration(p.Agent.TimeoutSeconds)*time.Second, logger)
	sources := []string{}
	if run != nil {
		sources = run.Sources()
	}
	if agentRun != nil {
		for _, s := range agentRun.Sources() {
			if !slices.Contains(sources, s) {
				sources = append(sources, s)
			}
		}
	}
	return out, timeline, sources
}

// taskTools are the read-only tools the task names and, when it offers
// commands the image has, the run tool limited to them.
func taskTools(t *TaskPrompt, tree *agent.Tree, maxOutput int, run *agent.RunTool) []agent.Tool {
	var tools []agent.Tool
	for _, name := range t.Tools {
		switch name {
		case "read_file":
			tools = append(tools, agent.ReadFileTool(tree, maxOutput))
		case "grep":
			tools = append(tools, agent.GrepTool(tree, maxOutput))
		case "list_files":
			tools = append(tools, agent.ListFilesTool(tree, maxOutput))
		}
	}
	if run != nil {
		tools = append(tools, run)
	}
	return tools
}

// taskFile is a file a task's context gathered, as .Context.files holds
// it.
type taskFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// gatherTask gathers the task's glob files and its commands' output within
// its budget, each source fenced as untrusted, followed by notes of what
// the budget or the runner left out, fenced too since they name paths and
// commands the repository chose.
func gatherTask(ctx context.Context, t *TaskPrompt, head *object.Tree, ignore []string, run *agent.RunTool) (gathered, notes []string) {
	b := &tasks.Budget{PerSource: t.SourceBytes, Left: t.ContextBytes}
	files := make([]taskFile, 0, len(t.Files))
	for _, f := range t.Files {
		files = append(files, globFiles(ctx, head, ignore, f, b)...)
	}
	add := func(name string, v any) {
		s, err := tasks.FenceContext(name, v)
		if err != nil {
			b.Notes = append(b.Notes, fmt.Sprintf("context %s not encoded: %v", name, err))
			return
		}
		gathered = append(gathered, s)
	}
	if len(files) > 0 {
		add(taskFilesSource, files)
	}
	for _, c := range t.Commands {
		out, err := contextCommand(ctx, run, c)
		if err != nil {
			b.Notes = append(b.Notes, fmt.Sprintf("context %s not gathered: %v", c.Name, err))
			continue
		}
		add(c.Name, b.Take(c.Name, out))
	}
	if notes = b.Notes; len(notes) > 0 {
		add("notes", notes)
	}
	return gathered, notes
}

// globFiles reads up to f.Max (or maxGlobFiles) text files matching
// f.Glob, in path order, each within the budget. Ignored paths and files
// over repoconfig.MaxFileBytes are left out.
func globFiles(ctx context.Context, head *object.Tree, ignore []string, f TaskFiles, b *tasks.Budget) []taskFile {
	limit := f.Max
	if limit <= 0 || limit > maxGlobFiles {
		limit = maxGlobFiles
	}
	var matched []*object.File
	_ = head.Files().ForEach(func(file *object.File) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if ok, _ := doublestar.Match(f.Glob, file.Name); ok && !chunk.Ignored(ignore, file.Name) && file.Size <= repoconfig.MaxFileBytes {
			matched = append(matched, file)
		}
		return nil
	})
	slices.SortFunc(matched, func(a, b *object.File) int { return strings.Compare(a.Name, b.Name) })
	if len(matched) > limit {
		b.Notes = append(b.Notes, fmt.Sprintf("context files %s kept %d of %d matches", f.Glob, limit, len(matched)))
		matched = matched[:limit]
	}
	var out []taskFile
	for _, file := range matched {
		content, err := readText(file)
		if err != nil {
			b.Notes = append(b.Notes, fmt.Sprintf("context files %s: %v", file.Name, err))
			continue
		}
		out = append(out, taskFile{Path: file.Name, Content: b.Take("files "+file.Name, content)})
	}
	return out
}

// maxGlobFiles caps the files one glob gathers, as tasks.Check caps max.
const maxGlobFiles = 50

// readText is file's content, refused when it is binary.
func readText(file *object.File) (string, error) {
	r, err := file.Reader()
	if err != nil {
		return "", err
	}
	defer func() { _ = r.Close() }()
	raw, err := io.ReadAll(io.LimitReader(r, repoconfig.MaxFileBytes))
	if err != nil {
		return "", err
	}
	if slices.Contains(raw[:min(len(raw), 8<<10)], 0) {
		return "", errors.New("binary file left out")
	}
	return string(raw), nil
}

// contextCommand runs c through the run tool, as the agent's run tool
// would: allowlisted, without a shell, bounded in time and output.
func contextCommand(ctx context.Context, run *agent.RunTool, c TaskCommand) (string, error) {
	if run == nil || !slices.Contains(run.Names(), c.Argv[0]) {
		return "", fmt.Errorf("%s is not available on this runner", c.Argv[0])
	}
	input, err := json.Marshal(map[string]any{"command": c.Argv[0], "args": c.Argv[1:]})
	if err != nil {
		return "", err
	}
	return run.Run(ctx, input)
}
