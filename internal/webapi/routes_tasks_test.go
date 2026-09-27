package webapi

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/home-operations/kritik/internal/store"
)

func TestTaskRunDuration(t *testing.T) {
	tests := []struct {
		name string
		row  store.TaskRunRow
		want *int64
	}{
		{"queued", store.TaskRunRow{CreatedAt: t0}, nil},
		{"from start", store.TaskRunRow{CreatedAt: t0.Add(-30 * time.Second), StartedAt: &t0, FinishedAt: &t1}, new(int64(90000))},
		{"skipped before it started", store.TaskRunRow{CreatedAt: t0, FinishedAt: &t1}, new(int64(90000))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := taskRun(tt.row).DurationMs
			if (got == nil) != (tt.want == nil) || (got != nil && *got != *tt.want) {
				t.Errorf("DurationMs = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestTaskRunDetailDecodesRecords feeds the records as the task worker
// stores them.
func TestTaskRunDetailDecodesRecords(t *testing.T) {
	tests := []struct {
		name    string
		in      store.TaskRunDetail
		want    func(TaskRunDetail) bool
		wantErr bool
	}{
		{
			name: "queued run has empty records",
			in:   store.TaskRunDetail{},
			want: func(d TaskRunDetail) bool {
				return d.Proposed == nil && d.Applied == nil && len(d.Fields) == 0 && d.Fields != nil && d.Dropped != nil && d.Event == nil
			},
		},
		{
			name: "finished run",
			in: store.TaskRunDetail{
				Event:  &store.TaskEventRow{RawEvent: "issues", Action: "opened", Sender: "ada"},
				Fields: json.RawMessage(`{"priority":"high","area":["db"]}`),
				Proposed: json.RawMessage(`{"summary":"s","fields":{"priority":"high"},"labels":{"add":["bug","x"],"remove":null},
					"inline":[{"path":"a.go","line":3,"end_line":4,"body":"b"}]}`),
				Applied: json.RawMessage(`{"add_labels":["bug"],"inline":[{"path":"a.go","line":3,"end_line":4,"body":"b"}],"comment":"sticky"}`),
				Dropped: json.RawMessage(`[{"action":"labels.add","value":"x","reason":"it is not a label the task lets the model add"}]`),
			},
			want: func(d TaskRunDetail) bool {
				return string(d.Fields["area"]) == `["db"]` && d.Event.Sender == "ada" &&
					reflect.DeepEqual(d.Proposed.AddLabels, []string{"bug", "x"}) && d.Proposed.RemoveLabels != nil &&
					d.Proposed.Inline[0] == TaskInline{Path: "a.go", Line: 3, EndLine: 4, Body: "b"} &&
					reflect.DeepEqual(d.Applied.AddLabels, []string{"bug"}) && d.Applied.RemoveLabels != nil &&
					d.Applied.Inline[0].EndLine == 4 && d.Applied.Comment == "sticky" &&
					d.Dropped[0] == TaskDrop{Action: "labels.add", Value: "x", Reason: "it is not a label the task lets the model add"}
			},
		},
		{name: "corrupt record", in: store.TaskRunDetail{Applied: json.RawMessage(`[1]`)}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := taskRunDetail(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && !tt.want(got) {
				t.Errorf("taskRunDetail = %+v", got)
			}
		})
	}
}
