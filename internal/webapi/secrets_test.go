package webapi

import (
	"cmp"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/home-operations/kritik/internal/configfile"
)

func fakeSeal(b []byte) (string, error) { return "sealed:" + string(b), nil }

func fakeGenerate() (string, error) { return "g3n", nil }

const storedSpec = `{"slug":"alpha","installations":[
	{"name":"alpha-bot","forge":"github","accounts":["alpha"],
	 "app":{"clientId":"cid","privateKey":{"sealed":"old-key"},"webhookSecret":{"sealed":"old-hook"}}},
	{"name":"alpha-gh","forge":"github","accounts":["alpha"],
	 "app":{"clientId":"cid2","privateKey":{"sealed":"old-gh-key"},"webhookSecret":{"sealed":"old-app-hook"}}}]}`

func TestSealSpec(t *testing.T) {
	tests := []struct {
		name      string
		spec      string
		stored    string
		want      string
		generated map[string]string
		changed   []string
		errPath   string
		errCode   ErrorCode
	}{
		{
			name:    "value is sealed",
			spec:    `{"slug":"alpha","installations":[{"name":"alpha-bot","app":{"privateKey":{"value":"k3y"},"webhookSecret":{"value":"h00k"}}}]}`,
			want:    `{"installations":[{"app":{"privateKey":{"sealed":"sealed:k3y"},"webhookSecret":{"sealed":"sealed:h00k"}},"name":"alpha-bot"}],"slug":"alpha"}`,
			changed: []string{"installations[alpha-bot].app.privateKey", "installations[alpha-bot].app.webhookSecret"},
		},
		{
			name:   "keep copies the stored sealed value by installation name",
			stored: storedSpec,
			spec: `{"slug":"alpha","installations":[{"name":"alpha-gh","forge":"github","accounts":["Alpha"],` +
				`"app":{"clientId":"cid2","privateKey":{"keep":true},"webhookSecret":{"keep":true}}},` +
				`{"name":"alpha-bot","forge":"github","accounts":["alpha"],` +
				`"app":{"clientId":"cid","privateKey":{"keep":true},"webhookSecret":{"value":"new"}}}]}`,
			want: `{"installations":[{"accounts":["Alpha"],"app":{"clientId":"cid2","privateKey":{"sealed":"old-gh-key"},` +
				`"webhookSecret":{"sealed":"old-app-hook"}},"forge":"github","name":"alpha-gh"},` +
				`{"accounts":["alpha"],"app":{"clientId":"cid","privateKey":{"sealed":"old-key"},` +
				`"webhookSecret":{"sealed":"sealed:new"}},"forge":"github","name":"alpha-bot"}],"slug":"alpha"}`,
			changed: []string{"installations[alpha-bot].app.webhookSecret"},
		},
		{
			name:   "a webhook secret stays keepable when the accounts change",
			stored: storedSpec,
			spec:   `{"slug":"alpha","installations":[{"name":"alpha-bot","forge":"github","accounts":["beta"],"app":{"webhookSecret":{"keep":true}}}]}`,
			want:   `{"installations":[{"accounts":["beta"],"app":{"webhookSecret":{"sealed":"old-hook"}},"forge":"github","name":"alpha-bot"}],"slug":"alpha"}`,
		},
		{
			name:    "a private key is not kept onto another account",
			stored:  storedSpec,
			spec:    `{"slug":"alpha","installations":[{"name":"alpha-bot","forge":"github","accounts":["beta"],"app":{"privateKey":{"keep":true}}}]}`,
			errPath: "installations[0].app.privateKey", errCode: CodeReenterSecret,
		},
		{
			name:    "a private key is not kept onto an added account",
			stored:  storedSpec,
			spec:    `{"slug":"alpha","installations":[{"name":"alpha-bot","forge":"github","accounts":["alpha","beta"],"app":{"privateKey":{"keep":true}}}]}`,
			errPath: "installations[0].app.privateKey", errCode: CodeReenterSecret,
		},
		{
			name:    "a private key is not kept onto another forge",
			stored:  storedSpec,
			spec:    `{"slug":"alpha","installations":[{"name":"alpha-gh","forge":"gitlab","accounts":["alpha"],"app":{"privateKey":{"keep":true}}}]}`,
			errPath: "installations[0].app.privateKey", errCode: CodeReenterSecret,
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
			spec:    `{"slug":"alpha","installations":[{"name":"a","app":{"privateKey":{"env":"HOME"}}}]}`,
			errPath: "installations[0].app.privateKey",
		},
		{
			name:    "file is rejected",
			spec:    `{"slug":"alpha","installations":[{"name":"a","app":{"webhookSecret":{"file":"/etc/passwd"}}}]}`,
			errPath: "installations[0].app.webhookSecret",
		},
		{
			name:    "sealed is rejected",
			spec:    `{"slug":"alpha","installations":[{"name":"a","app":{"privateKey":{"sealed":"stolen"}}}]}`,
			errPath: "installations[0].app.privateKey",
		},
		{
			name:    "set is not a write form",
			spec:    `{"slug":"alpha","installations":[{"name":"a","app":{"privateKey":{"set":true}}}]}`,
			errPath: "installations[0].app.privateKey",
		},
		{
			name:    "two forms at once",
			spec:    `{"slug":"alpha","installations":[{"name":"a","app":{"privateKey":{"value":"x","keep":true}}}]}`,
			errPath: "installations[0].app.privateKey",
		},
		{
			name:    "empty value",
			spec:    `{"slug":"alpha","installations":[{"name":"a","app":{"privateKey":{"value":""}}}]}`,
			errPath: "installations[0].app.privateKey",
		},
		{
			name:    "keep false",
			spec:    `{"slug":"alpha","installations":[{"name":"a","app":{"privateKey":{"keep":false}}}]}`,
			errPath: "installations[0].app.privateKey",
		},
		{
			name:    "generate only for webhook secrets",
			spec:    `{"slug":"alpha","installations":[{"name":"a","app":{"privateKey":{"generate":true}}}]}`,
			errPath: "installations[0].app.privateKey",
		},
		{
			name:    "keep with nothing stored",
			spec:    `{"slug":"alpha","installations":[{"name":"new-bot","app":{"privateKey":{"keep":true}}}]}`,
			stored:  storedSpec,
			errPath: "installations[0].app.privateKey",
		},
		{
			name:    "keep on create",
			spec:    `{"slug":"alpha","installations":[{"name":"alpha-bot","app":{"privateKey":{"keep":true}}}]}`,
			errPath: "installations[0].app.privateKey",
		},
		{
			name:    "a provider key is sealed",
			spec:    `{"slug":"alpha","providers":{"mine":{"type":"openai","apiKey":{"value":"sk-1"}}}}`,
			want:    `{"providers":{"mine":{"apiKey":{"sealed":"sealed:sk-1"},"type":"openai"}},"slug":"alpha"}`,
			changed: []string{"providers.mine.apiKey"},
		},
		{
			name:   "a provider key is kept while its type and endpoint are",
			stored: `{"slug":"alpha","providers":{"mine":{"type":"openai","baseUrl":"https://llm.example/v1/","apiKey":{"sealed":"old-sk"}}}}`,
			spec:   `{"slug":"alpha","providers":{"mine":{"type":"openai","baseUrl":"https://LLM.example/v1","apiKey":{"keep":true}}}}`,
			want:   `{"providers":{"mine":{"apiKey":{"sealed":"old-sk"},"baseUrl":"https://LLM.example/v1","type":"openai"}},"slug":"alpha"}`,
		},
		{
			name:    "a provider key is not kept onto another endpoint",
			stored:  `{"slug":"alpha","providers":{"mine":{"type":"openai","apiKey":{"sealed":"old-sk"}}}}`,
			spec:    `{"slug":"alpha","providers":{"mine":{"type":"openai","baseUrl":"https://elsewhere.example","apiKey":{"keep":true}}}}`,
			errPath: "providers.mine.apiKey", errCode: CodeReenterSecret,
		},
		{
			name:    "a provider key is not generated",
			spec:    `{"slug":"alpha","providers":{"mine":{"type":"openai","apiKey":{"generate":true}}}}`,
			errPath: "providers.mine.apiKey",
		},
		{
			name:    "a plain string is not a secret form",
			spec:    `{"slug":"alpha","installations":[{"name":"a","app":{"privateKey":"k3y"}}]}`,
			errPath: "installations[0].app.privateKey",
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
				if want := cmp.Or(tt.errCode, CodeInvalidSpec); se.errorCode() != want {
					t.Fatalf("code = %q, want %q", se.errorCode(), want)
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
		{"name":"a","app":{"privateKey":{"env":"X"},"webhookSecret":{}}},
		{"name":"b","app":{"clientId":"cid","clientIdFrom":{"file":"/x"},"privateKey":{"sealed":"k"},"webhookSecret":{"sealed":""}}}],
		"providers":{"mine":{"type":"openai","apiKey":{"sealed":"sk"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"installations":[{"app":{"privateKey":{"set":true},"webhookSecret":{"set":false}},"name":"a"},` +
		`{"app":{"clientId":"cid","clientIdFrom":{"set":true},"privateKey":{"set":true},"webhookSecret":{"set":false}},"name":"b"}],` +
		`"providers":{"mine":{"apiKey":{"set":true},"type":"openai"}},"slug":"alpha"}`
	if string(got) != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	for _, leak := range []string{`"s"`, `"k"`, `"sk"`, "/x", `"X"`, "sealed", "env"} {
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
			Name string `json:"name"`
			App  struct {
				PrivateKey    map[string]bool `json:"privateKey"`
				WebhookSecret map[string]bool `json:"webhookSecret"`
			} `json:"app"`
		} `json:"installations"`
	}
	if err := json.Unmarshal(got, &back); err != nil {
		t.Fatalf("%s: %v", got, err)
	}
	if back.Slug != "alpha" || len(back.Installations) != 1 || back.Installations[0].Name != "alpha-bot" ||
		!back.Installations[0].App.PrivateKey["set"] || !back.Installations[0].App.WebhookSecret["set"] {
		t.Errorf("render = %s", got)
	}
}

func TestRenderFileTenantDurations(t *testing.T) {
	tn := configfile.Tenant{Slug: "x", Settle: new(90 * time.Second)}
	got, err := renderFileTenant(&tn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"settle":"1m30s"`) {
		t.Errorf("render = %s", got)
	}
}
