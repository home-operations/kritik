package webapi

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/home-operations/kritik/internal/configfile"
)

func fakeSeal(b []byte) (string, error) { return "sealed:" + string(b), nil }

func fakeGenerate() (string, error) { return "g3n", nil }

const storedSpec = `{"slug":"alpha","installations":[
	{"name":"alpha-bot","forge":"forgejo","host":"git.example","account":"alpha",
	 "token":{"sealed":"old-token"},"webhookSecret":{"sealed":"old-hook"}},
	{"name":"alpha-gh","forge":"github","account":"alpha",
	 "app":{"clientId":"cid","privateKey":{"sealed":"old-key"},"webhookSecret":{"sealed":"old-app-hook"}}}]}`

func TestSealSpec(t *testing.T) {
	tests := []struct {
		name      string
		spec      string
		stored    string
		want      string
		generated map[string]string
		changed   []string
		errPath   string
	}{
		{
			name:    "value is sealed",
			spec:    `{"slug":"alpha","installations":[{"name":"alpha-bot","token":{"value":"t0k"},"webhookSecret":{"value":"h00k"}}]}`,
			want:    `{"installations":[{"name":"alpha-bot","token":{"sealed":"sealed:t0k"},"webhookSecret":{"sealed":"sealed:h00k"}}],"slug":"alpha"}`,
			changed: []string{"installations[alpha-bot].token", "installations[alpha-bot].webhookSecret"},
		},
		{
			name:   "keep copies the stored sealed value by installation name",
			stored: storedSpec,
			spec: `{"slug":"alpha","installations":[{"name":"alpha-gh","app":{"clientId":"cid","privateKey":{"keep":true},` +
				`"webhookSecret":{"keep":true}}},{"name":"alpha-bot","token":{"keep":true},"webhookSecret":{"value":"new"}}]}`,
			want: `{"installations":[{"app":{"clientId":"cid","privateKey":{"sealed":"old-key"},"webhookSecret":{"sealed":"old-app-hook"}},` +
				`"name":"alpha-gh"},{"name":"alpha-bot","token":{"sealed":"old-token"},"webhookSecret":{"sealed":"sealed:new"}}],"slug":"alpha"}`,
			changed: []string{"installations[alpha-bot].webhookSecret"},
		},
		{
			name:      "generate makes a webhook secret and returns it once",
			spec:      `{"slug":"alpha","installations":[{"name":"alpha-bot","webhookSecret":{"generate":true}}]}`,
			want:      `{"installations":[{"name":"alpha-bot","webhookSecret":{"sealed":"sealed:g3n"}}],"slug":"alpha"}`,
			generated: map[string]string{"installations[alpha-bot].webhookSecret": "g3n"},
			changed:   []string{"installations[alpha-bot].webhookSecret"},
		},
		{
			name:      "generate an app webhook secret",
			spec:      `{"slug":"alpha","installations":[{"name":"gh","app":{"webhookSecret":{"generate":true}}}]}`,
			want:      `{"installations":[{"app":{"webhookSecret":{"sealed":"sealed:g3n"}},"name":"gh"}],"slug":"alpha"}`,
			generated: map[string]string{"installations[gh].app.webhookSecret": "g3n"},
			changed:   []string{"installations[gh].app.webhookSecret"},
		},
		{
			name: "numbers and other keys pass through",
			spec: `{"slug":"alpha","limits":{"tokensPerMonth":12345678901234},"installations":[]}`,
			want: `{"installations":[],"limits":{"tokensPerMonth":12345678901234},"slug":"alpha"}`,
		},
		{
			name:    "env is rejected",
			spec:    `{"slug":"alpha","installations":[{"name":"a","token":{"env":"HOME"}}]}`,
			errPath: "installations[0].token",
		},
		{
			name:    "file is rejected",
			spec:    `{"slug":"alpha","installations":[{"name":"a","gitToken":{"file":"/etc/passwd"}}]}`,
			errPath: "installations[0].gitToken",
		},
		{
			name:    "sealed is rejected",
			spec:    `{"slug":"alpha","installations":[{"name":"a","app":{"privateKey":{"sealed":"stolen"}}}]}`,
			errPath: "installations[0].app.privateKey",
		},
		{
			name:    "set is not a write form",
			spec:    `{"slug":"alpha","installations":[{"name":"a","token":{"set":true}}]}`,
			errPath: "installations[0].token",
		},
		{
			name:    "two forms at once",
			spec:    `{"slug":"alpha","installations":[{"name":"a","token":{"value":"x","keep":true}}]}`,
			errPath: "installations[0].token",
		},
		{
			name:    "empty value",
			spec:    `{"slug":"alpha","installations":[{"name":"a","token":{"value":""}}]}`,
			errPath: "installations[0].token",
		},
		{
			name:    "keep false",
			spec:    `{"slug":"alpha","installations":[{"name":"a","token":{"keep":false}}]}`,
			errPath: "installations[0].token",
		},
		{
			name:    "generate only for webhook secrets",
			spec:    `{"slug":"alpha","installations":[{"name":"a","token":{"generate":true}}]}`,
			errPath: "installations[0].token",
		},
		{
			name:    "keep with nothing stored",
			spec:    `{"slug":"alpha","installations":[{"name":"new-bot","token":{"keep":true}}]}`,
			stored:  storedSpec,
			errPath: "installations[0].token",
		},
		{
			name:    "keep on create",
			spec:    `{"slug":"alpha","installations":[{"name":"alpha-bot","token":{"keep":true}}]}`,
			errPath: "installations[0].token",
		},
		{
			name:    "a plain string is not a secret form",
			spec:    `{"slug":"alpha","installations":[{"name":"a","token":"t0k"}]}`,
			errPath: "installations[0].token",
		},
		{
			name:    "not an object",
			spec:    `[]`,
			errPath: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stored json.RawMessage
			if tt.stored != "" {
				stored = json.RawMessage(tt.stored)
			}
			got, err := sealSpec(json.RawMessage(tt.spec), stored, fakeSeal, fakeGenerate)
			if tt.errPath != "" || tt.want == "" {
				se, ok := errors.AsType[*specError](err)
				if !ok {
					t.Fatalf("err = %v, want a specError at %q", err, tt.errPath)
				}
				if se.path != tt.errPath {
					t.Fatalf("path = %q, want %q (%v)", se.path, tt.errPath, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(got.spec) != tt.want {
				t.Errorf("spec =\n%s\nwant\n%s", got.spec, tt.want)
			}
			if len(got.generated) != 0 || len(tt.generated) != 0 {
				if !reflect.DeepEqual(got.generated, tt.generated) {
					t.Errorf("generated = %v, want %v", got.generated, tt.generated)
				}
			}
			if !reflect.DeepEqual(got.changed, tt.changed) {
				t.Errorf("changed = %v, want %v", got.changed, tt.changed)
			}
		})
	}
}

func TestRedactSpec(t *testing.T) {
	got, err := redactSpec(json.RawMessage(`{"slug":"alpha","installations":[
		{"name":"a","token":{"sealed":"s"},"webhookSecret":{},"gitToken":{"env":"X"}},
		{"name":"b","app":{"clientId":"cid","clientIdFrom":{"file":"/x"},"privateKey":{"sealed":"k"},"webhookSecret":{"sealed":""}}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"installations":[{"gitToken":{"set":true},"name":"a","token":{"set":true},"webhookSecret":{"set":false}},` +
		`{"app":{"clientId":"cid","clientIdFrom":{"set":true},"privateKey":{"set":true},"webhookSecret":{"set":false}},"name":"b"}],"slug":"alpha"}`
	if string(got) != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	for _, leak := range []string{`"s"`, `"k"`, "/x", `"X"`, "sealed", "env"} {
		if strings.Contains(string(got), leak) {
			t.Errorf("redacted spec contains %s", leak)
		}
	}
}

func TestRenderFileTenant(t *testing.T) {
	f := testFile(t)
	tn, _ := f.Tenant("alpha")
	got, err := renderFileTenant(tn)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "KRITIK_TEST_TOKEN") || strings.Contains(string(got), "tok\"") {
		t.Fatalf("file tenant render leaks a secret reference: %s", got)
	}
	var back struct {
		Slug          string `json:"slug"`
		Installations []struct {
			Name          string          `json:"name"`
			Token         map[string]bool `json:"token"`
			WebhookSecret map[string]bool `json:"webhookSecret"`
		} `json:"installations"`
	}
	if err := json.Unmarshal(got, &back); err != nil {
		t.Fatalf("%s: %v", got, err)
	}
	if back.Slug != "alpha" || len(back.Installations) != 1 || back.Installations[0].Name != "alpha-bot" ||
		!back.Installations[0].Token["set"] || !back.Installations[0].WebhookSecret["set"] {
		t.Errorf("render = %s", got)
	}
}

func TestRenderFileTenantDurations(t *testing.T) {
	tn := configfile.Tenant{Slug: "x", Settle: 90e9}
	got, err := renderFileTenant(&tn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"settle":"1m30s"`) {
		t.Errorf("render = %s", got)
	}
}
