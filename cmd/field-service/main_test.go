package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"field-service/internal/jwks"
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

// A failing issuer, so a warm can be watched failing.
func failingJWKS(t *testing.T, attempts *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(attempts, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
}

// warmJWKS must give up rather than retry for the life of the process. It is
// background work with no request behind it, so a loop with no exit would be a
// goroutine hammering an issuer that is not coming up.
func TestWarmJWKSGivesUpAfterItsWindow(t *testing.T) {
	var attempts int32
	srv := failingJWKS(t, &attempts)
	defer srv.Close()

	cfg := config{jwksURL: srv.URL, jwksPrewarm: 40 * time.Millisecond}
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))

	start := time.Now()
	warmJWKS(context.Background(), jwks.New(srv.URL), cfg, log)

	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("warmJWKS ran for %v against a %v window; it should have stopped",
			elapsed, cfg.jwksPrewarm)
	}
	if got := atomic.LoadInt32(&attempts); got == 0 {
		t.Error("gave up without ever attempting a fetch")
	}
}

// A cancelled context must end the warm at once. Shutting down should not wait
// out a backoff, and a warm that ignores the signal would hold the exit open for
// a fetch nobody is going to read.
func TestWarmJWKSStopsWhenTheContextIsCancelled(t *testing.T) {
	var attempts int32
	srv := failingJWKS(t, &attempts)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cfg := config{jwksURL: srv.URL, jwksPrewarm: time.Minute}
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))

	start := time.Now()
	warmJWKS(ctx, jwks.New(srv.URL), cfg, log)

	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("a cancelled warm still ran for %v", elapsed)
	}
}
