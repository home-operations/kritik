package model

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAIEmbedderBatchesAndChecksDims(t *testing.T) {
	var batches [][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input      []string `json:"input"`
			Dimensions int      `json:"dimensions"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		batches = append(batches, req.Input)
		data := make([]string, 0, len(req.Input))
		for i := range req.Input {
			data = append(data, `{"object":"embedding","index":`+strconvInt(i)+`,"embedding":[0.1,0.2,0.3]}`)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","model":"emb","data":[` + strings.Join(data, ",") + `],"usage":{"prompt_tokens":1,"total_tokens":1}}`))
	}))
	defer srv.Close()
	e := NewOpenAIEmbedder(srv.URL, "k", "emb", 3)
	e.MaxBatch = 2
	e.MaxItemChars = 5
	vecs, tokens, err := e.Embed(t.Context(), []string{"a", "b", "c", "dddddddddd"})
	if err != nil {
		t.Fatal(err)
	}
	if tokens != 2 {
		t.Fatalf("tokens = %d, want the two batches' usage summed", tokens)
	}
	if len(vecs) != 4 || len(vecs[0]) != 3 || len(batches) != 2 || len(batches[0]) != 2 || batches[1][1] != "ddddd" {
		t.Fatalf("vecs=%d batches=%v", len(vecs), batches)
	}
	wrong := NewOpenAIEmbedder(srv.URL, "k", "emb", 8)
	if _, _, err := wrong.Embed(t.Context(), []string{"x"}); err == nil {
		t.Fatal("a dimension mismatch must be an error")
	}
}

func strconvInt(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}
