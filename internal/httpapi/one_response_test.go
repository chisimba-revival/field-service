package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A refusal must stop the handler.
//
// problem() returned writeJSON's error, which is nil on success, so "refused and
// already answered" and "carried on" were the same value. A request with the
// wrong content type was answered 415 and then processed anyway, writing a second
// body whose status line Go discarded with a "superfluous WriteHeader" warning —
// leaving the client a truncated response and no way to tell what happened.
//
// This asserts on the number of status lines rather than on the status code,
// because the bug was never visible in the status: it was 415, correctly, and
// then something else.
func TestARefusedRequestIsAnsweredExactlyOnce(t *testing.T) {
	for _, tc := range []struct {
		name        string
		contentType string
		body        string
		want        int
	}{
		{"wrong content type", "text/plain", `{"operations":[]}`, http.StatusUnsupportedMediaType},
		{"unreadable body", "application/json", `{"nope":1}`, http.StatusBadRequest},
		{"empty batch", "application/json", `{"operations":[]}`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &countingWriter{ResponseRecorder: httptest.NewRecorder()}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/sync/push", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", tc.contentType)
			req.Header.Set("Authorization", "Bearer token")

			h := (&Sync{Session: aWorkingSession{}}).pushHandler()
			_ = h(w, req, Caller{Principal: fakePrincipal{
				sub: "user-1", scopes: []string{"field:write"},
				active: "north-reserve", grants: []string{"north-reserve"},
			}})

			if w.statuses != 1 {
				t.Errorf("%d status lines written; want 1. A second one is "+
					"discarded by net/http, so the client gets a truncated body "+
					"with no indication that anything went wrong.", w.statuses)
			}
			if w.Code != tc.want {
				t.Errorf("status %d, want %d", w.Code, tc.want)
			}
		})
	}
}

// countingWriter records how many times a status line was written.
type countingWriter struct {
	*httptest.ResponseRecorder
	statuses int
}

func (c *countingWriter) WriteHeader(n int) {
	c.statuses++
	c.ResponseRecorder.WriteHeader(n)
}
