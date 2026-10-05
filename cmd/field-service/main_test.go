package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The logger is handed a request by the guard, but internal() once passed nil
// because it had only been given the writer — and a logger that reads r.Method
// then panicked, killing the handler mid-response. The client got nothing, so the
// one error the fault path exists to report was the one thing never reported.
//
// Nil tolerance is therefore a property of the logger, not a courtesy, and it is
// asserted here rather than assumed: a panic in a test is a loud failure, where
// in production it was a silent empty response.
func TestTheLoggerSurvivesBeingHandedNoRequest(t *testing.T) {
	l := logAdapter{log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	l.Refused(nil, "handler_failed", io.ErrUnexpectedEOF)

	// And still logs something useful when it does have one.
	l.Refused(httptest.NewRequest(http.MethodPost, "/api/v1/sync/push", nil),
		"no_grants", nil)
}
