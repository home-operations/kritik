package forgejo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// repoLabelsPageSize bounds each GET /labels page, for the same
// short-page-means-last-page reasoning as reviewsPageSize.
const repoLabelsPageSize = 50

// RepoLabels implements forge.Client.
func (c *Client) RepoLabels(ctx context.Context, owner, repo string) ([]string, error) {
	labels, err := c.repoLabels(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(labels))
	for i, l := range labels {
		out[i] = l.Name
	}
	return out, nil
}

func (c *Client) repoLabels(ctx context.Context, owner, repo string) ([]label, error) {
	var out []label
	page := 1
	for {
		var batch []label
		path := fmt.Sprintf("%s/labels?page=%d&limit=%d", repoPath(owner, repo), page, repoLabelsPageSize)
		if err := c.do(ctx, http.MethodGet, path, nil, &batch); err != nil {
			return nil, fmt.Errorf("forgejo: list labels of %s/%s: %w", owner, repo, err)
		}
		out = append(out, batch...)
		if len(batch) < repoLabelsPageSize {
			return out, nil
		}
		page++
	}
}

// AddLabels implements forge.Client, by label id, which every Forgejo and
// Gitea release takes, where older Gitea releases take no names. A name
// the repository has no label for adds nothing at all.
func (c *Client) AddLabels(ctx context.Context, owner, repo string, number int, labels []string) error {
	if len(labels) == 0 {
		return nil
	}
	known, err := c.repoLabels(ctx, owner, repo)
	if err != nil {
		return fmt.Errorf("forgejo: add labels to %s/%s#%d: %w", owner, repo, number, err)
	}
	ids := make([]int64, len(labels))
	for i, name := range labels {
		id, ok := findLabelID(known, name)
		if !ok {
			return fmt.Errorf("forgejo: add labels to %s/%s#%d: the repository has no label %q", owner, repo, number, name)
		}
		ids[i] = id
	}
	path := fmt.Sprintf("%s/issues/%d/labels", repoPath(owner, repo), number)
	if err := c.do(ctx, http.MethodPost, path, issueLabelsOption{Labels: ids}, nil); err != nil {
		return fmt.Errorf("forgejo: add labels to %s/%s#%d: %w", owner, repo, number, err)
	}
	return nil
}

// RemoveLabel implements forge.Client. A label not currently applied is not
// an error. Forgejo's DELETE endpoint addresses a label by numeric id, not
// name, so this first resolves name against the issue's current labels; a
// lookup failure is always an error, but the DELETE call itself tolerates a
// 404 (the label was removed concurrently) as a no-op.
func (c *Client) RemoveLabel(ctx context.Context, owner, repo string, number int, name string) error {
	var current []label
	listPath := fmt.Sprintf("%s/issues/%d/labels", repoPath(owner, repo), number)
	if err := c.do(ctx, http.MethodGet, listPath, nil, &current); err != nil {
		return fmt.Errorf("forgejo: remove label %q from %s/%s#%d: %w", name, owner, repo, number, err)
	}
	id, ok := findLabelID(current, name)
	if !ok {
		return nil
	}
	deletePath := fmt.Sprintf("%s/issues/%d/labels/%d", repoPath(owner, repo), number, id)
	if err := c.do(ctx, http.MethodDelete, deletePath, nil, nil); err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("forgejo: remove label %q from %s/%s#%d: %w", name, owner, repo, number, err)
	}
	return nil
}

// findLabelID returns the numeric id of the label named name within labels,
// and whether it was found.
func findLabelID(labels []label, name string) (int64, bool) {
	for _, l := range labels {
		if l.Name == name {
			return l.ID, true
		}
	}
	return 0, false
}
