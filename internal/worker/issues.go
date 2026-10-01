package worker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"

	"github.com/home-operations/kritika/internal/forge"
	"github.com/home-operations/kritika/internal/review"
)

// linkedIssues reads the issues of owner/repo that body, the pull request
// description, says the change closes, for the prompt. A number the forge
// has no issue for, or that names a pull request, is left out; one the
// forge would not give is left out with a note the summary states, since
// the App may lack the permission to read it.
func linkedIssues(ctx context.Context, client forge.Client, owner, repo, body string, logger *slog.Logger) ([]review.Issue, []string) {
	numbers := review.LinkedIssues(body, owner+"/"+repo)
	issues := make([]review.Issue, 0, len(numbers))
	var notes []string
	for _, n := range numbers {
		is, err := client.Issue(ctx, owner, repo, n)
		switch {
		case errors.Is(err, fs.ErrNotExist), err == nil && is.PullRequest:
			continue
		case err != nil:
			logger.Warn("linked issue not read", "issue", n, "error", err)
			notes = append(notes, fmt.Sprintf("issue #%d, which the description says this closes, could not be read", n))
			continue
		}
		issues = append(issues, review.Issue{Number: is.Number, Title: is.Title, Body: is.Body})
	}
	return issues, notes
}
