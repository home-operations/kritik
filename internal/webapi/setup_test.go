package webapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/home-operations/kritik/internal/auth"
	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/configfile/configfiletest"
)

func TestSetupStatus(t *testing.T) {
	t.Setenv("TEST_KEY", "k")
	const conn = `connections:
  - { name: acme-bot, forge: github, accounts: [acme], app: { clientId: Iv1.acme, privateKey: { env: TEST_KEY }, webhookSecret: { env: TEST_KEY } } }
`
	const provider = "providers:\n  p: { type: openai, apiKey: { env: TEST_KEY } }\n"
	base := SetupStatus{WebURL: "https://kritik.example", HooksURL: "https://kritik.example/hooks/", FileConnections: []string{}, Connections: []string{}}
	with := func(edit func(*SetupStatus)) SetupStatus {
		s := base
		edit(&s)
		return s
	}
	for _, tt := range []struct {
		name, doc string
		want      SetupStatus
	}{
		{"a fresh instance", "", base},
		{"a connection alone", conn, with(func(s *SetupStatus) {
			s.FileConnections, s.Connections = []string{"acme-bot"}, []string{"acme-bot"}
		})},
		{"ready", conn + "defaults: { models: { review: p/x } }\n" + provider, with(func(s *SetupStatus) {
			s.FileConnections, s.Connections, s.ReviewModel = []string{"acme-bot"}, []string{"acme-bot"}, "p/x"
		})},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := setupStatus(configfiletest.Load(t, tt.doc), "https://kritik.example/")
			if got.WebURL != tt.want.WebURL || got.HooksURL != tt.want.HooksURL || !slices.Equal(got.FileConnections, tt.want.FileConnections) ||
				!slices.Equal(got.Connections, tt.want.Connections) || got.ReviewModel != tt.want.ReviewModel ||
				got.Embedding != tt.want.Embedding {
				t.Fatalf("setupStatus = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// fakeProvider is an OpenAI-compatible API that takes the key "good": it
// lists one model and embeds at three dimensions.
func fakeProvider(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer good" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"Incorrect API key"}}`))
			return
		}
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-x","object":"model"}]}`))
		case "/v1/embeddings":
			_, _ = w.Write([]byte(`{"object":"list","model":"e","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2,0.3]}],` +
				`"usage":{"prompt_tokens":1,"total_tokens":1}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1"
}

func TestProviderAndEmbeddingTests(t *testing.T) {
	ts := newTestServer(t, "https://kritik.example")
	base := fakeProvider(t)
	t.Setenv("TEST_GOOD", "good")
	ts.srv.current.Set(configfiletest.Load(t, testConfig+"providers:\n  held: { type: openai, baseUrl: "+base+", apiKey: { env: TEST_GOOD } }\n"+
		"embedding: { baseUrl: "+base+", apiKey: { env: TEST_GOOD }, model: e, dims: 3 }\n"))
	admin := &auth.Principal{Operator: true}
	post := func(p *auth.Principal, path string, body any) (int, TestResult, ErrorBody) {
		t.Helper()
		raw, _ := json.Marshal(body)
		w := ts.as(p, mutate("POST", path, string(raw)))
		var res TestResult
		var eb ErrorBody
		if w.Code == http.StatusOK {
			_ = json.Unmarshal(w.Body.Bytes(), &res)
		} else {
			_ = json.Unmarshal(w.Body.Bytes(), &eb)
		}
		return w.Code, res, eb
	}
	value := func(v string) json.RawMessage { return json.RawMessage(`{"value":"` + v + `"}`) }
	keep := json.RawMessage(`{"keep":true}`)

	for _, tt := range []struct {
		name   string
		req    ProviderTestRequest
		ok     bool
		code   int
		errSub string
	}{
		{"a typed key", ProviderTestRequest{Type: configfile.ProviderOpenAI, BaseURL: base, APIKey: value("good")}, true, 200, ""},
		{"a bad key", ProviderTestRequest{Type: configfile.ProviderOpenAI, BaseURL: base, APIKey: value("bad")}, false, 200, "401"},
		{"the held key", ProviderTestRequest{Type: configfile.ProviderOpenAI, BaseURL: base + "/", APIKey: keep, Name: "held"}, true, 200, ""},
		{"the held key elsewhere", ProviderTestRequest{Type: configfile.ProviderOpenAI, BaseURL: "https://elsewhere.example/v1", APIKey: keep, Name: "held"},
			false, http.StatusUnprocessableEntity, "endpoint changed"},
		{"no key", ProviderTestRequest{Type: configfile.ProviderOpenAI, APIKey: keep}, false, http.StatusUnprocessableEntity, "no stored key"},
		{"a bad type", ProviderTestRequest{Type: "gemini", APIKey: value("good")}, false, http.StatusUnprocessableEntity, "type must be"},
	} {
		t.Run("provider "+tt.name, func(t *testing.T) {
			code, res, eb := post(admin, "/api/v1/operator/providers/test", tt.req)
			if code != tt.code || res.OK != tt.ok || !strings.Contains(res.Error+eb.Message, tt.errSub) {
				t.Fatalf("= %d %+v %+v", code, res, eb)
			}
			if tt.ok && !slices.Equal(res.Models, []string{"gpt-x"}) {
				t.Fatalf("models = %v", res.Models)
			}
		})
	}
	for _, tt := range []struct {
		name   string
		req    EmbeddingTestRequest
		ok     bool
		code   int
		errSub string
	}{
		{"the held key", EmbeddingTestRequest{BaseURL: base, Model: "e", Dims: 3, APIKey: keep}, true, 200, ""},
		{"a dimension the model does not give", EmbeddingTestRequest{BaseURL: base, Model: "e", Dims: 8, APIKey: value("good")}, false, 200,
			"returned 3 dimensions"},
		{"too wide", EmbeddingTestRequest{BaseURL: base, Model: "e", Dims: 5000, APIKey: value("good")}, false, http.StatusUnprocessableEntity,
			"dims must be"},
	} {
		t.Run("embedding "+tt.name, func(t *testing.T) {
			code, res, eb := post(admin, "/api/v1/operator/embedding/test", tt.req)
			if code != tt.code || res.OK != tt.ok || !strings.Contains(res.Error+eb.Message, tt.errSub) {
				t.Fatalf("= %d %+v %+v", code, res, eb)
			}
		})
	}
	if code, _, _ := post(memberOf(t, ts.file, "alpha"), "/api/v1/operator/providers/test", ProviderTestRequest{}); code != http.StatusNotFound {
		t.Fatalf("a member's test = %d, want 404", code)
	}
}
