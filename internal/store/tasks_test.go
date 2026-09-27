package store

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/home-operations/kritik/internal/tasks"
)

func TestTaskRunAppliedJSON(t *testing.T) {
	tests := []struct {
		name string
		in   TaskRunApplied
		want string
	}{
		{"empty", TaskRunApplied{}, `{}`},
		{
			"every field",
			TaskRunApplied{
				AddLabels: []string{"bug"}, RemoveLabels: []string{"triage"}, Assignees: []string{"alice"},
				Reviewers: []string{"bob"}, State: "closed",
				Inline:  []tasks.Inline{{Path: "a.go", Line: 3, EndLine: 5, Body: "nit"}},
				Comment: "update",
			},
			`{"add_labels":["bug"],"remove_labels":["triage"],"assignees":["alice"],"reviewers":["bob"],"state":"closed",` +
				`"inline":[{"path":"a.go","line":3,"end_line":5,"body":"nit"}],"comment":"update"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := json.Marshal(tt.in)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(b) != tt.want {
				t.Errorf("Marshal = %s, want %s", b, tt.want)
			}
			var got TaskRunApplied
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if !reflect.DeepEqual(got, tt.in) {
				t.Errorf("round trip = %+v, want %+v", got, tt.in)
			}
		})
	}
}
