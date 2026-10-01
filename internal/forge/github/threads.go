package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Review threads are GraphQL-only on GitHub: REST knows the comments but
// not the thread that groups them, nor its resolved state.

const threadsQuery = `query($owner: String!, $repo: String!, $number: Int!, $after: String) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      reviewThreads(first: 100, after: $after) {
        pageInfo { hasNextPage endCursor }
        nodes { id isResolved comments(first: 100) { nodes { databaseId author { login } } } }
      }
    }
  }
}`

const resolveThreadMutation = `mutation($id: ID!) { resolveReviewThread(input: {threadId: $id}) { thread { isResolved } } }`

// reviewThread is what ResolveThread needs of one thread.
type reviewThread struct {
	ID         string `json:"id"`
	IsResolved bool   `json:"isResolved"`
	Comments   struct {
		Nodes []struct {
			DatabaseID int64 `json:"databaseId"`
			Author     struct {
				Login string `json:"login"`
			} `json:"author"`
		} `json:"nodes"`
	} `json:"comments"`
}

// ResolveThread implements forge.Client.
func (c *Client) ResolveThread(ctx context.Context, owner, repo string, number int, id int64, onlyOwn bool) (bool, error) {
	login, err := c.BotLogin(ctx)
	if err != nil {
		return false, err
	}
	thread, err := c.findThread(ctx, owner, repo, number, id)
	if err != nil {
		return false, fmt.Errorf("github: find the thread of comment %d: %w", id, err)
	}
	if thread == nil || thread.IsResolved {
		return false, nil
	}
	for _, cm := range thread.Comments.Nodes {
		if onlyOwn && !sameLogin(cm.Author.Login, login) {
			return false, nil
		}
	}
	var out struct {
		ResolveReviewThread struct {
			Thread struct {
				IsResolved bool `json:"isResolved"`
			} `json:"thread"`
		} `json:"resolveReviewThread"`
	}
	if err := c.graphql(ctx, resolveThreadMutation, map[string]any{"id": thread.ID}, &out); err != nil {
		return false, fmt.Errorf("github: resolve the thread of comment %d: %w", id, err)
	}
	return out.ResolveReviewThread.Thread.IsResolved, nil
}

// findThread pages through the pull request's review threads for the one
// holding comment id, nil when none does.
func (c *Client) findThread(ctx context.Context, owner, repo string, number int, id int64) (*reviewThread, error) {
	var after *string
	for {
		var out struct {
			Repository struct {
				PullRequest struct {
					ReviewThreads struct {
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []reviewThread `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		vars := map[string]any{"owner": owner, "repo": repo, "number": number, "after": after}
		if err := c.graphql(ctx, threadsQuery, vars, &out); err != nil {
			return nil, err
		}
		threads := out.Repository.PullRequest.ReviewThreads
		for i := range threads.Nodes {
			for _, cm := range threads.Nodes[i].Comments.Nodes {
				if cm.DatabaseID == id {
					return &threads.Nodes[i], nil
				}
			}
		}
		if !threads.PageInfo.HasNextPage {
			return nil, nil
		}
		after = &threads.PageInfo.EndCursor
	}
}

// sameLogin compares a GraphQL author login, which names a bot by its slug
// alone, with a REST login, which suffixes "[bot]".
func sameLogin(a, b string) bool {
	return strings.EqualFold(strings.TrimSuffix(a, "[bot]"), strings.TrimSuffix(b, "[bot]"))
}

// graphql posts one query with its variables through the installation's
// authenticated client and decodes data into out; a response with errors
// is an error.
func (c *Client) graphql(ctx context.Context, query string, vars map[string]any, out any) error {
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.graphqlURL(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.api.Client().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("graphql: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var env struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("graphql: decode response: %w", err)
	}
	if len(env.Errors) > 0 {
		msgs := make([]string, len(env.Errors))
		for i, e := range env.Errors {
			msgs[i] = e.Message
		}
		return errors.New("graphql: " + strings.Join(msgs, "; "))
	}
	if len(env.Data) == 0 {
		return errors.New("graphql: no data")
	}
	return json.Unmarshal(env.Data, out)
}

// graphqlURL is the GraphQL endpoint beside the REST base the client was
// built with: GitHub's own, or an Enterprise host's /api/graphql.
func (c *Client) graphqlURL() string {
	base, err := url.Parse(c.api.BaseURL())
	if err != nil || base.Host == "api.github.com" {
		return "https://api.github.com/graphql"
	}
	return base.Scheme + "://" + base.Host + "/api/graphql"
}
