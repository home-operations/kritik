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

const storedSpec = `{"slug":"alpha","connections":[
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
			spec:    `{"slug":"alpha","connections":[{"name":"alpha-bot","app":{"privateKey":{"value":"k3y"},"webhookSecret":{"value":"h00k"}}}]}`,
			want:    `{"connections":[{"app":{"privateKey":{"sealed":"sealed:k3y"},"webhookSecret":{"sealed":"sealed:h00k"}},"name":"alpha-bot"}],"slug":"alpha"}`,
			changed: []string{"connections[alpha-bot].app.privateKey", "connections[alpha-bot].app.webhookSecret"},
		},
		{
			name:   "keep copies the stored sealed value by connection name",
			stored: storedSpec,
			spec: `{"slug":"alpha","connections":[{"name":"alpha-gh","forge":"github","accounts":["Alpha"],` +
				`"app":{"clientId":"cid2","privateKey":{"keep":true},"webhookSecret":{"keep":true}}},` +
				`{"name":"alpha-bot","forge":"github","accounts":["alpha"],` +
				`"app":{"clientId":"cid","privateKey":{"keep":true},"webhookSecret":{"value":"new"}}}]}`,
			want: `{"connections":[{"accounts":["Alpha"],"app":{"clientId":"cid2","privateKey":{"sealed":"old-gh-key"},` +
				`"webhookSecret":{"sealed":"old-app-hook"}},"forge":"github","name":"alpha-gh"},` +
				`{"accounts":["alpha"],"app":{"clientId":"cid","privateKey":{"sealed":"old-key"},` +
				`"webhookSecret":{"sealed":"sealed:new"}},"forge":"github","name":"alpha-bot"}],"slug":"alpha"}`,
			changed: []string{"connections[alpha-bot].app.webhookSecret"},
		},
		{
			name:   "a webhook secret stays keepable when the accounts change",
			stored: storedSpec,
			spec:   `{"slug":"alpha","connections":[{"name":"alpha-bot","forge":"github","accounts":["beta"],"app":{"webhookSecret":{"keep":true}}}]}`,
			want:   `{"connections":[{"accounts":["beta"],"app":{"webhookSecret":{"sealed":"old-hook"}},"forge":"github","name":"alpha-bot"}],"slug":"alpha"}`,
		},
		{
			name:    "a private key is not kept onto another account",
			stored:  storedSpec,
			spec:    `{"slug":"alpha","connections":[{"name":"alpha-bot","forge":"github","accounts":["beta"],"app":{"privateKey":{"keep":true}}}]}`,
			errPath: "connections[0].app.privateKey", errCode: CodeReenterSecret,
		},
		{
			name:    "a private key is not kept onto an added account",
			stored:  storedSpec,
			spec:    `{"slug":"alpha","connections":[{"name":"alpha-bot","forge":"github","accounts":["alpha","beta"],"app":{"privateKey":{"keep":true}}}]}`,
			errPath: "connections[0].app.privateKey", errCode: CodeReenterSecret,
		},
		{
			name:    "a private key is not kept onto another forge",
			stored:  storedSpec,
			spec:    `{"slug":"alpha","connections":[{"name":"alpha-gh","forge":"gitlab","accounts":["alpha"],"app":{"privateKey":{"keep":true}}}]}`,
			errPath: "connections[0].app.privateKey", errCode: CodeReenterSecret,
		},
		{
			name:      "generate an app webhook secret",
			spec:      `{"slug":"alpha","connections":[{"name":"gh","app":{"webhookSecret":{"generate":true}}}]}`,
			want:      `{"connections":[{"app":{"webhookSecret":{"sealed":"sealed:g3n"}},"name":"gh"}],"slug":"alpha"}`,
			generated: map[string]string{"connections[gh].app.webhookSecret": "g3n"},
			changed:   []string{"connections[gh].app.webhookSecret"},
		},
		{
			name: "numbers and other keys pass through",
			spec: `{"slug":"alpha","limits":{"tokensPerMonth":12345678901234},"connections":[]}`,
			want: `{"connections":[],"limits":{"tokensPerMonth":12345678901234},"slug":"alpha"}`,
		},
		{
			name:    "env is rejected",
			spec:    `{"slug":"alpha","connections":[{"name":"a","app":{"privateKey":{"env":"HOME"}}}]}`,
			errPath: "connections[0].app.privateKey",
		},
		{
			name:    "file is rejected",
			spec:    `{"slug":"alpha","connections":[{"name":"a","app":{"webhookSecret":{"file":"/etc/passwd"}}}]}`,
			errPath: "connections[0].app.webhookSecret",
		},
		{
			name:    "sealed is rejected",
			spec:    `{"slug":"alpha","connections":[{"name":"a","app":{"privateKey":{"sealed":"stolen"}}}]}`,
			errPath: "connections[0].app.privateKey",
		},
		{
			name:    "set is not a write form",
			spec:    `{"slug":"alpha","connections":[{"name":"a","app":{"privateKey":{"set":true}}}]}`,
			errPath: "connections[0].app.privateKey",
		},
		{
			name:    "two forms at once",
			spec:    `{"slug":"alpha","connections":[{"name":"a","app":{"privateKey":{"value":"x","keep":true}}}]}`,
			errPath: "connections[0].app.privateKey",
		},
		{
			name:    "empty value",
			spec:    `{"slug":"alpha","connections":[{"name":"a","app":{"privateKey":{"value":""}}}]}`,
			errPath: "connections[0].app.privateKey",
		},
		{
			name:    "keep false",
			spec:    `{"slug":"alpha","connections":[{"name":"a","app":{"privateKey":{"keep":false}}}]}`,
			errPath: "connections[0].app.privateKey",
		},
		{
			name:    "generate only for webhook secrets",
			spec:    `{"slug":"alpha","connections":[{"name":"a","app":{"privateKey":{"generate":true}}}]}`,
			errPath: "connections[0].app.privateKey",
		},
		{
			name:    "keep with nothing stored",
			spec:    `{"slug":"alpha","connections":[{"name":"new-bot","app":{"privateKey":{"keep":true}}}]}`,
			stored:  storedSpec,
			errPath: "connections[0].app.privateKey",
		},
		{
			name:    "keep on create",
			spec:    `{"slug":"alpha","connections":[{"name":"alpha-bot","app":{"privateKey":{"keep":true}}}]}`,
			errPath: "connections[0].app.privateKey",
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
			spec:    `{"slug":"alpha","connections":[{"name":"a","app":{"privateKey":"k3y"}}]}`,
			errPath: "connections[0].app.privateKey",
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
	got, err := redactSpec(json.RawMessage(`{"slug":"alpha","connections":[
		{"name":"a","app":{"privateKey":{"env":"X"},"webhookSecret":{}}},
		{"name":"b","app":{"clientId":"cid","clientIdFrom":{"file":"/x"},"privateKey":{"sealed":"k"},"webhookSecret":{"sealed":""}}}],
		"providers":{"mine":{"type":"openai","apiKey":{"sealed":"sk"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"connections":[{"app":{"privateKey":{"set":true},"webhookSecret":{"set":false}},"name":"a"},` +
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

func TestRenderFileAccount(t *testing.T) {
	f := testFile(t)
	tn, _ := f.Account("alpha")
	got, err := renderFileAccount(tn)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "KRITIK_TEST_TOKEN") || strings.Contains(string(got), "tok\"") {
		t.Fatalf("file account render leaks a secret reference: %s", got)
	}
	var back struct {
		Slug        string `json:"slug"`
		Connections []struct {
			Name string `json:"name"`
			App  struct {
				PrivateKey    map[string]bool `json:"privateKey"`
				WebhookSecret map[string]bool `json:"webhookSecret"`
			} `json:"app"`
		} `json:"connections"`
	}
	if err := json.Unmarshal(got, &back); err != nil {
		t.Fatalf("%s: %v", got, err)
	}
	if back.Slug != "alpha" || len(back.Connections) != 1 || back.Connections[0].Name != "alpha-bot" ||
		!back.Connections[0].App.PrivateKey["set"] || !back.Connections[0].App.WebhookSecret["set"] {
		t.Errorf("render = %s", got)
	}
}

func TestRenderFileAccountDurations(t *testing.T) {
	tn := configfile.Account{Slug: "x", Settle: new(90 * time.Second)}
	got, err := renderFileAccount(&tn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"settle":"1m30s"`) {
		t.Errorf("render = %s", got)
	}
}
