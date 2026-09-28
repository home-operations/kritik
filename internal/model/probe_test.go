package model

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// jsonError answers 401 with body as JSON, as the providers do.
func jsonError(w http.ResponseWriter, body string) {
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(body))
}

func TestProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authed := r.Header.Get("Authorization") == "Bearer good" || r.Header.Get("X-Api-Key") == "good"
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/or/v1/key":
			if !authed {
				jsonError(w, `{"error":{"message":"No auth credentials found"}}`)
				return
			}
			_, _ = w.Write([]byte(`{"data":{"label":"test"}}`))
		case "/or/v1/models":
			// OpenRouter lists models to anyone.
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"z/big","object":"model"},{"id":"a/small","object":"model"}]}`))
		case "/oa/v1/models":
			if !authed {
				jsonError(w, `{"error":{"message":"Incorrect API key"}}`)
				return
			}
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-x","object":"model"}]}`))
		case "/an/v1/models":
			if !authed {
				jsonError(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
				return
			}
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-x","type":"model"}],"has_more":false}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	for _, tt := range []struct {
		name string
		typ  ProviderType
		base string
		want []string
	}{
		{"openrouter", ProviderOpenRouter, srv.URL + "/or/v1", []string{"a/small", "z/big"}},
		{"openai", ProviderOpenAI, srv.URL + "/oa/v1", []string{"gpt-x"}},
		{"anthropic", ProviderAnthropic, srv.URL + "/an", []string{"claude-x"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Probe(t.Context(), tt.typ, tt.base, "good", srv.Client())
			if err != nil || !slices.Equal(got, tt.want) {
				t.Fatalf("Probe = %v, %v, want %v", got, err, tt.want)
			}
			if _, err := Probe(t.Context(), tt.typ, tt.base, "bad", srv.Client()); err == nil || !strings.Contains(err.Error(), "401") {
				t.Fatalf("a bad key = %v, want a 401", err)
			}
		})
	}
}
