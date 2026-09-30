package webapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
)

// metaPath is the one API route served without a session.
const metaPath = "/api/v1/meta"

// maxBodyBytes bounds a request body.
const maxBodyBytes = 1 << 20

var errForbidden = errStatus(http.StatusForbidden, CodeForbidden, "this needs an admin")

func (s *Server) getMeta(w http.ResponseWriter, _ *http.Request) error {
	writeJSON(w, http.StatusOK, Meta{Version: s.version, WebURL: s.webURL.String()})
	return nil
}

// readBody strictly decodes one JSON document into v.
func readBody(r *http.Request, v any) error {
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxBodyBytes))
	if err != nil {
		return errBadRequest(CodeBadRequest, "request body is too large or unreadable")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errBadRequest(CodeBadRequest, "request body is not valid JSON: "+err.Error())
	}
	if dec.More() {
		return errBadRequest(CodeBadRequest, "request body must hold one JSON document")
	}
	return nil
}
