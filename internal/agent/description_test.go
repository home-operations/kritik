package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestReadDescriptionTool(t *testing.T) {
	body := "### Release Notes\n" + strings.Repeat("- a fix\n", 10) + "### v0.12.17"
	tool := ReadDescriptionTool(body, map[int]string{12: "Steps to reproduce.", 13: "  "}, 1<<20)
	tests := []struct {
		name, input, want, wantErr string
	}{
		{name: "the description", input: `{}`, want: body},
		{name: "no input", input: ``, want: body},
		{name: "a linked issue", input: `{"issue":12}`, want: "Steps to reproduce."},
		{name: "an empty issue", input: `{"issue":13}`, want: "(empty)"},
		{name: "an issue the prompt does not list", input: `{"issue":99}`, wantErr: "issue #99 is not one the prompt lists"},
		{name: "a malformed input", input: `{"issue":"12"}`, wantErr: "read_description"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tool.Run(t.Context(), json.RawMessage(tt.input))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("got %q, %v; want %q", got, err, tt.want)
			}
		})
	}
	t.Run("the output is capped", func(t *testing.T) {
		got, err := ReadDescriptionTool(body, nil, 20).Run(t.Context(), nil)
		if err != nil || !strings.HasPrefix(got, body[:20]+"\n[truncated ") {
			t.Fatalf("got %q, %v", got, err)
		}
	})
}
