package worker

import (
	"errors"
	"testing"

	"github.com/home-operations/kritika/internal/executor"
	"github.com/home-operations/kritika/internal/store"
)

func TestErrText(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "nil is empty", err: nil, want: ""},
		{name: "an error is its message", err: errors.New("x"), want: "x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := errText(tt.err); got != tt.want {
				t.Errorf("errText(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

func TestEmbedText(t *testing.T) {
	tests := []struct {
		name  string
		chunk store.StagedChunk
		want  string
	}{
		{name: "with a symbol", chunk: store.StagedChunk{Path: "a/b.go", Symbol: "Build", Kind: "function", Text: "func Build() {}"},
			want: "a/b.go function Build\nfunc Build() {}"},
		{name: "without a symbol", chunk: store.StagedChunk{Path: "values.yaml", Text: "a: 1"}, want: "values.yaml\na: 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := embedText(tt.chunk); got != tt.want {
				t.Errorf("embedText(%+v) = %q, want %q", tt.chunk, got, tt.want)
			}
		})
	}
}

func TestRunOutcome(t *testing.T) {
	tests := []struct {
		name string
		res  executor.Result
		want string
	}{
		{name: "no error", res: executor.Result{}, want: "success"},
		{name: "an error", res: executor.Result{Err: errors.New("x")}, want: "failed"},
		{name: "a deadline", res: executor.Result{Err: errors.New("x"), DeadlineExceeded: true}, want: "deadline"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := runOutcome(tt.res); got != tt.want {
				t.Errorf("runOutcome(%+v) = %s, want %s", tt.res, got, tt.want)
			}
		})
	}
}

func TestMentioned(t *testing.T) {
	tests := []struct {
		body string
		want bool
	}{
		{"@kritika please", true},
		{"hey @Kritika, why?", true},
		{"email me@kritika.io", false},
		{"@kritikabot no", false},
		{"no mention", false},
	}
	for _, tt := range tests {
		t.Run(tt.body, func(t *testing.T) {
			if got := mentioned(tt.body, "kritika"); got != tt.want {
				t.Errorf("mentioned(%q) = %v, want %v", tt.body, got, tt.want)
			}
		})
	}
}
