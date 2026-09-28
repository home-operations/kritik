package github

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewManifest(t *testing.T) {
	m := NewManifest("kritik-org-1", "https://kritik.example/", "org-1-bot", true)
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{
		"url":          "https://kritik.example",
		"redirect_url": "https://kritik.example/app/callback",
		"setup_url":    "https://kritik.example/app/installed",
		"public":       true,
	} {
		if got[key] != want {
			t.Errorf("%s = %v, want %v", key, got[key], want)
		}
	}
	hook := got["hook_attributes"].(map[string]any)
	if hook["url"] != "https://kritik.example/hooks/org-1-bot" || hook["active"] != true {
		t.Errorf("hook_attributes = %v", hook)
	}
	if cb := got["callback_urls"].([]any); len(cb) != 1 || cb[0] != "https://kritik.example/auth/callback/github" {
		t.Errorf("callback_urls = %v", cb)
	}
	if perms := got["default_permissions"].(map[string]any); perms["issues"] != "read" || perms["members"] != "read" {
		t.Errorf("default_permissions = %v", perms)
	}
}

func TestRegisterURL(t *testing.T) {
	for _, tt := range []struct{ org, want string }{
		{"", "https://github.com/settings/apps/new?state=a%2Bb"},
		{"org-1", "https://github.com/organizations/org-1/settings/apps/new?state=a%2Bb"},
	} {
		if got := RegisterURL(tt.org, "a+b"); got != tt.want {
			t.Errorf("RegisterURL(%q) = %s, want %s", tt.org, got, tt.want)
		}
	}
}

func TestConvertManifest(t *testing.T) {
	complete := `{"slug":"kritik-org-1","client_id":"Iv1.x","client_secret":"cs","webhook_secret":"wh","pem":"pem","owner":{"login":"org-1"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("the conversion must be sent without credentials, got %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/api/v3/app-manifests/good/conversions":
			_, _ = w.Write([]byte(complete))
		case "/api/v3/app-manifests/partial/conversions":
			_, _ = w.Write([]byte(`{"slug":"kritik-org-1"}`))
		default:
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	creds, err := ConvertManifest(t.Context(), srv.URL+"/api/v3", "good")
	want := AppCredentials{Slug: "kritik-org-1", ClientID: "Iv1.x", ClientSecret: "cs", WebhookSecret: "wh", PEM: "pem", Owner: "org-1"}
	if err != nil || creds != want {
		t.Fatalf("ConvertManifest = %+v, %v", creds, err)
	}
	for _, code := range []string{"partial", "expired"} {
		if _, err := ConvertManifest(t.Context(), srv.URL+"/api/v3", code); err == nil || !strings.Contains(err.Error(), "convert App manifest") {
			t.Errorf("ConvertManifest(%s) = %v, want an error", code, err)
		}
	}
}
