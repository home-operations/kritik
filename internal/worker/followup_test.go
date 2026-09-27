package worker

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/home-operations/kritik/internal/forge"
)

// threadForge answers the comment listings from fixed comments and counts
// the inline ones.
type threadForge struct {
	forge.Client
	inline       []forge.Comment
	conversation []forge.Comment
	inlineLists  int
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
