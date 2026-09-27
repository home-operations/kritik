package webapi

import (
	"reflect"
	"testing"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/store"
	"github.com/home-operations/kritik/internal/tasks"
)

func TestRepoTasks(t *testing.T) {
	t.Parallel()
	settings := configfile.Settings{
		Mode: configfile.ReviewSingle,
		Tasks: []tasks.Task{{
			Name: "welcome", On: []tasks.Trigger{{Event: tasks.EventPullRequest, Actions: []string{"opened"}}},
			Mode: tasks.ModeSingle, Actions: tasks.Actions{Comment: &tasks.CommentSpec{}},
		}},
		DashboardTasks: []tasks.Task{{
			Name: "close-stale", On: []tasks.Trigger{{Event: tasks.EventIssue}}, Mode: tasks.ModeSingle,
			Actions: tasks.Actions{State: &tasks.StateSpec{Propose: tasks.StateSet{Close: true}}, Comment: &tasks.CommentSpec{Mode: tasks.CommentNone}},
		}},
		TaskBounds: (&configfile.TaskBounds{Enabled: new(true)}).Resolve(),
	}
	doc := "tasks:\n" +
		"  - name: triage\n" +
		"    mode: single\n" +
		"    if: subject.kind == \"issue\"\n" +
		"    on: [{ issue: [opened, reopened] }, { raw: { event: issues, actions: [transferred] } }]\n" +
		"    actions: { labels: { propose: { add: [bug] } } }\n"
	bad := "tasks: [{ name: Bad }]\n"

	welcome := TaskDef{
		Name: "welcome", Source: configfile.SourceFile, Triggers: []string{"pull_request.opened"}, Mode: "single",
		Actions: []string{tasks.ActionComment},
	}
	stale := TaskDef{
		Name: "close-stale", Source: configfile.SourceDashboard, Triggers: []string{"issue.*"}, Mode: "single", Actions: []string{},
	}
	triage := TaskDef{
		Name: "triage", Source: configfile.SourceRepository, Triggers: []string{"issue.opened", "issue.reopened"},
		If: `subject.kind == "issue"`, Mode: "single", Actions: []string{tasks.ActionLabels},
	}
	tests := []struct {
		name      string
		row       *store.RepoFileRow
		wantTasks []TaskDef
		wantNotes []TaskNote
	}{
		{
			name:      "no file read yet",
			wantTasks: []TaskDef{welcome, stale},
			wantNotes: []TaskNote{{Task: "close-stale", What: "actions.state", Reason: "the action is not allowed"}},
		},
		{
			name:      "no file at the commit",
			row:       &store.RepoFileRow{},
			wantTasks: []TaskDef{welcome, stale},
			wantNotes: []TaskNote{{Task: "close-stale", What: "actions.state", Reason: "the action is not allowed"}},
		},
		{
			name:      "a file with a task",
			row:       &store.RepoFileRow{Doc: &doc},
			wantTasks: []TaskDef{welcome, stale, triage},
			wantNotes: []TaskNote{
				{Task: "close-stale", What: "actions.state", Reason: "the action is not allowed"},
				{Task: "triage", What: "on[1] raw:issues.transferred", Reason: "the event is not allowed"},
			},
		},
		{
			name:      "a file that does not parse",
			row:       &store.RepoFileRow{Doc: &bad},
			wantTasks: []TaskDef{welcome, stale},
			wantNotes: []TaskNote{{Task: "close-stale", What: "actions.state", Reason: "the action is not allowed"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotTasks, gotNotes := repoTasks(settings, tt.row)
			if !reflect.DeepEqual(gotTasks, tt.wantTasks) {
				t.Errorf("tasks = %+v, want %+v", gotTasks, tt.wantTasks)
			}
			if !reflect.DeepEqual(gotNotes, tt.wantNotes) {
				t.Errorf("notes = %+v, want %+v", gotNotes, tt.wantNotes)
			}
		})
	}
}
