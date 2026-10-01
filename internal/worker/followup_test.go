package worker

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/home-operations/kritik/internal/forge"
	"github.com/home-operations/kritik/internal/review"
)

// threadForge answers the comment listings from fixed comments and counts
// the inline ones, and answers every permission lookup the same way.
type threadForge struct {
	forge.Client
	inline       []forge.Comment
	conversation []forge.Comment
	inlineLists  int
	permission   forge.Permission
	permErr      error
}

func (f *threadForge) Permission(context.Context, string, string, string) (forge.Permission, error) {
	return f.permission, f.permErr
}

func (f *threadForge) ListInline(context.Context, string, string, int) ([]forge.Comment, error) {
	f.inlineLists++
	return f.inline, nil
}

func (f *threadForge) ListConversation(context.Context, string, string, int) ([]forge.Comment, error) {
	return f.conversation, nil
}

func TestFollowUpThread(t *testing.T) {
	at := func(s int64) time.Time { return time.Unix(s, 0) }
	root := forge.Comment{ID: 1, Body: "@kritik is this safe?", CreatedAt: at(1), Inline: true}
	reply := forge.Comment{ID: 2, Body: "@kritik why?", CreatedAt: at(2), Inline: true, InReplyTo: 1}
	other := forge.Comment{ID: 3, Body: "another thread", CreatedAt: at(3), Inline: true}
	later := forge.Comment{ID: 4, Body: "a later reply", CreatedAt: at(4), Inline: true, InReplyTo: 1}
	first := forge.Comment{ID: 10, Body: "first", CreatedAt: at(5)}
	asking := forge.Comment{ID: 11, Body: "@kritik summarize", CreatedAt: at(6)}
	tests := []struct {
		name       string
		comment    forge.Comment
		want       []string
		wantListed int
	}{
		{name: "a reply gathers its thread, the asking comment last", comment: reply,
			want: []string{root.Body, later.Body, reply.Body}, wantListed: 1},
		{name: "a comment that starts its thread lists nothing", comment: root, want: []string{root.Body}},
		{name: "a conversation comment gathers the conversation", comment: asking, want: []string{first.Body, asking.Body}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &threadForge{inline: []forge.Comment{root, reply, other, later}, conversation: []forge.Comment{first, asking}}
			f := &followUp{client: client, owner: "o", repo: "r", pr: &pullRequest{number: 7}, comment: tt.comment}
			msgs, err := f.thread(t.Context())
			if err != nil {
				t.Fatalf("thread: %v", err)
			}
			var got []string
			for _, m := range msgs {
				got = append(got, m.Body)
			}
			if !slices.Equal(got, tt.want) || client.inlineLists != tt.wantListed {
				t.Fatalf("thread = %q with %d inline listings, want %q with %d", got, client.inlineLists, tt.want, tt.wantListed)
			}
		})
	}
}

func TestRequestsReview(t *testing.T) {
	for _, tt := range []struct {
		body string
		want bool
	}{
		{"@kritik review", true},
		{"Looks fine to me. @Kritik Review please", true},
		{"@kritik review?", true},
		{"@kritik  review\nthe auth change", true},
		{"@kritik reviewed this already?", false},
		{"@kritik why is b here? a review would help", false},
		{"@kritikbot review", false},
		{"someone@kritik review", false},
		{"review @kritik", false},
	} {
		if got := requestsReview(tt.body, "kritik"); got != tt.want {
			t.Errorf("requestsReview(%q) = %v, want %v", tt.body, got, tt.want)
		}
	}
}

func TestMarkedReply(t *testing.T) {
	const login = "kritik[bot]"
	reply := func(id int64, author string, commentID int64) forge.Comment {
		return forge.Comment{ID: id, Author: author, Body: "Here is why.\n\n" + review.FollowUpMarker(commentID)}
	}
	tests := []struct {
		name     string
		comments []forge.Comment
		want     int64
	}{
		{name: "the bot's reply to the comment is found", comments: []forge.Comment{reply(20, login, 7)}, want: 20},
		{name: "a reply to another comment is not", comments: []forge.Comment{reply(21, login, 8)}},
		{name: "another author's marker is not", comments: []forge.Comment{reply(22, "mallory", 7)}},
		{name: "no comments"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := markedReply(tt.comments, login, 7); got != tt.want {
				t.Errorf("markedReply = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestDisqualified(t *testing.T) {
	tests := []struct {
		name       string
		comment    forge.Comment
		permission forge.Permission
		permErr    error
		want       string
	}{
		{name: "a bot author", comment: forge.Comment{Author: "renovate[bot]", AuthorIsBot: true, Body: "@kritik explain"}, want: "author is a bot"},
		{name: "the bot's own login, whatever its case", comment: forge.Comment{Author: "Kritik[Bot]", Body: "@kritik explain"}, want: "author is a bot"},
		{name: "no mention of the bot", comment: forge.Comment{Author: "alice", Body: "looks good"}, want: "does not mention @kritik"},
		{name: "a permission lookup that fails", comment: forge.Comment{Author: "alice", Body: "@kritik explain"},
			permErr: errors.New("boom"), want: "permission unknown"},
		{name: "read access", comment: forge.Comment{Author: "alice", Body: "@kritik explain"},
			permission: forge.PermissionRead, want: "author has read access, write is required"},
		{name: "write access", comment: forge.Comment{Author: "alice", Body: "@kritik explain"}, permission: forge.PermissionWrite},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &threadForge{permission: tt.permission, permErr: tt.permErr}
			f := &followUp{client: client, owner: "o", repo: "r", comment: tt.comment, botLogin: "kritik[bot]", logger: slog.New(slog.DiscardHandler)}
			if got := f.disqualified(t.Context()); got != tt.want {
				t.Errorf("disqualified = %q, want %q", got, tt.want)
			}
		})
	}
}
