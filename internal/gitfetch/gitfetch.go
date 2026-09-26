// Package gitfetch fetches exactly the commits a review needs, the head and
// the merge-base and, for a re-review, the head of the last review, at
// depth one into a throwaway bare repository, and diffs their trees. Trees
// are enough: `git diff` compares trees and needs no history. It is pure
// go-git; the runner image has no git binary.
package gitfetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

// Refs the two commits are fetched into. They are private to kritik so the
// bare repository never gains a branch a later fetch could confuse.
const (
	headRef  = "refs/kritik/head"
	baseRef  = "refs/kritik/base"
	priorRef = "refs/kritik/prior"
)

const remoteName = "origin"

// Fetch describes what to fetch.
type Fetch struct {
	// CloneURL is the HTTPS clone URL, or a local path for tests.
	CloneURL string
	// Token, when set, authenticates as x-access-token, which is what a GitHub
	// App installation token and a Forgejo token both expect.
	Token string
	// Head and Base are full commit SHAs.
	Head, Base string
	// Prior, when set, is the full SHA of the head the last review saw. It
	// is fetched best effort: a force-push may have made it unreachable.
	Prior string
}

// Result is the two fetched commits and the diff between them.
type Result struct {
	Head *object.Commit
	Base *object.Commit
	// Diff is the unified diff from base to head.
	Diff string
	// PatchID is a stable identity of the change, independent of line
	// numbers and of which commits carry it, in the spirit of
	// `git patch-id --stable`: a rebase that leaves the change untouched
	// keeps its patch id.
	PatchID string
	// Changed lists the paths the diff touches, head-side names.
	Changed []string
	// Prior is the fetched prior head, nil when none was asked for or it
	// could not be fetched, in which case PriorErr says why. DeltaDiff and
	// DeltaChanged are the diff from it to head and the paths that diff
	// touches.
	Prior        *object.Commit
	PriorErr     error
	DeltaDiff    string
	DeltaChanged []string
	// Dir is the bare repository on disk; the caller removes it.
	Dir string
}

// Close removes the bare repository.
func (r *Result) Close() error { return os.RemoveAll(r.Dir) }

// Run fetches head and base at depth one and diffs them. The temp dir is
// removed on error; on success the caller owns it through Result.Close.
func Run(ctx context.Context, f Fetch) (*Result, error) {
	if !IsSHA(f.Head) || (f.Base != "" && !IsSHA(f.Base)) || (f.Prior != "" && !IsSHA(f.Prior)) {
		return nil, fmt.Errorf("gitfetch: head %q, base %q and prior %q must be full commit SHAs", f.Head, f.Base, f.Prior)
	}
	dir, err := os.MkdirTemp("", "kritik-fetch-")
	if err != nil {
		return nil, fmt.Errorf("gitfetch: temp dir: %w", err)
	}
	res, err := run(ctx, f, dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return res, nil
}

func run(ctx context.Context, f Fetch, dir string) (*Result, error) {
	repo, err := git.PlainInit(dir, true)
	if err != nil {
		return nil, fmt.Errorf("gitfetch: init: %w", err)
	}
	if _, err := repo.CreateRemote(&config.RemoteConfig{Name: remoteName, URLs: []string{f.CloneURL}}); err != nil {
		return nil, fmt.Errorf("gitfetch: remote: %w", err)
	}
	var auth transport.AuthMethod
	if f.Token != "" {
		auth = &githttp.BasicAuth{Username: "x-access-token", Password: f.Token}
	}
	// Fetching a bare SHA needs the server to allow it; GitHub, GitLab and
	// Forgejo do for reachable commits. Both refspecs in one fetch so the
	// server can send one pack.
	err = repo.FetchContext(ctx, &git.FetchOptions{
		RemoteName:   remoteName,
		Auth:         auth,
		Depth:        1,
		Tags:         git.NoTags,
		RefSpecs:     refSpecs(f),
		ProxyOptions: proxyFromEnv(),
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return nil, fmt.Errorf("gitfetch: fetch: %w", err)
	}
	head, err := repo.CommitObject(plumbing.NewHash(f.Head))
	if err != nil {
		return nil, fmt.Errorf("gitfetch: head %s: %w", f.Head, err)
	}
	if f.Base == "" {
		// Head only: no diff, the caller walks the tree.
		return &Result{Head: head, Dir: dir}, nil
	}
	base, err := repo.CommitObject(plumbing.NewHash(f.Base))
	if err != nil {
		return nil, fmt.Errorf("gitfetch: base %s: %w", f.Base, err)
	}
	diff, changed, err := diffCommits(ctx, base, head)
	if err != nil {
		return nil, err
	}
	res := &Result{Head: head, Base: base, Diff: diff, PatchID: PatchID(diff), Changed: changed, Dir: dir}
	if f.Prior == "" {
		return res, nil
	}
	if res.Prior, res.PriorErr = fetchPrior(ctx, repo, auth, f.Prior); res.Prior == nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("gitfetch: prior: %w", ctx.Err())
		}
		return res, nil
	}
	if res.DeltaDiff, res.DeltaChanged, err = diffCommits(ctx, res.Prior, head); err != nil {
		return nil, err
	}
	return res, nil
}

// proxyFromEnv is the proxy the pod was handed as HTTPS_PROXY, which go-git
// does not read on its own. Empty means a direct connection.
func proxyFromEnv() transport.ProxyOptions {
	for _, name := range []string{"HTTPS_PROXY", "https_proxy"} {
		if v := os.Getenv(name); v != "" {
			return transport.ProxyOptions{URL: v}
		}
	}
	return transport.ProxyOptions{}
}

// fetchPrior fetches the prior head in a fetch of its own, so that an
// unreachable prior cannot fail the fetch of head and base. The error says
// why the prior could not be had; the caller decides whether that matters.
func fetchPrior(ctx context.Context, repo *git.Repository, auth transport.AuthMethod, prior string) (*object.Commit, error) {
	if c, err := repo.CommitObject(plumbing.NewHash(prior)); err == nil {
		return c, nil
	}
	err := repo.FetchContext(ctx, &git.FetchOptions{
		RemoteName:   remoteName,
		Auth:         auth,
		ProxyOptions: proxyFromEnv(),
		Depth:        1,
		Tags:         git.NoTags,
		RefSpecs:     []config.RefSpec{config.RefSpec(prior + ":" + priorRef)},
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return nil, fmt.Errorf("gitfetch: fetch prior %s: %w", prior, err)
	}
	c, err := repo.CommitObject(plumbing.NewHash(prior))
	if err != nil {
		return nil, fmt.Errorf("gitfetch: prior %s: %w", prior, err)
	}
	return c, nil
}

// diffCommits diffs two commits' trees and lists the touched paths,
// head-side names.
func diffCommits(ctx context.Context, from, to *object.Commit) (string, []string, error) {
	fromTree, err := from.Tree()
	if err != nil {
		return "", nil, fmt.Errorf("gitfetch: tree of %s: %w", from.Hash, err)
	}
	toTree, err := to.Tree()
	if err != nil {
		return "", nil, fmt.Errorf("gitfetch: tree of %s: %w", to.Hash, err)
	}
	changes, err := object.DiffTreeWithOptions(ctx, fromTree, toTree, object.DefaultDiffTreeOptions)
	if err != nil {
		return "", nil, fmt.Errorf("gitfetch: diff: %w", err)
	}
	patch, err := changes.PatchContext(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("gitfetch: patch: %w", err)
	}
	var changed []string
	for _, c := range changes {
		name := c.To.Name
		if name == "" {
			name = c.From.Name
		}
		changed = append(changed, name)
	}
	return patch.String(), changed, nil
}

func refSpecs(f Fetch) []config.RefSpec {
	specs := []config.RefSpec{config.RefSpec(f.Head + ":" + headRef)}
	if f.Base != "" {
		specs = append(specs, config.RefSpec(f.Base+":"+baseRef))
	}
	return specs
}

// PatchID hashes a unified diff with everything positional stripped: hunk
// headers (line numbers move on a rebase), index lines (blob ids move with
// them) and trailing whitespace. What remains is the file names and the
// added and removed lines, which is what `git patch-id --stable` keys on.
func PatchID(diff string) string {
	h := sha256.New()
	for line := range strings.SplitSeq(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "@@"), strings.HasPrefix(line, "index "):
			continue
		}
		h.Write([]byte(strings.TrimRight(line, " \t\r")))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// IsSHA reports whether s is a full lowercase SHA-1 commit id.
func IsSHA(s string) bool {
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
