// Command field-service serves the field guiding API.
//
// This file is where the pieces stop being packages and become a process. It
// exists to make the assembly order explicit and checkable, because that order
// is the difference between the service working and the service appearing to
// work: the pool must exist before the session, the session before the guard,
// and the guard before a single route is registered. Getting that wrong does not
// crash loudly — it produces a service that validates tokens and then reads
// everything, or refuses everything, and both look like working software until
// the first field device is left behind.
//
// Nothing here decides anything about authorisation. Every decision belongs to
// the guard, the session or the database policies, and this file's whole job is
// to wire them together without becoming a place where a decision could
// quietly be taken instead.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"field-service/internal/authn"
	"field-service/internal/competency"
	"field-service/internal/httpapi"
	"field-service/internal/jwks"
	"field-service/internal/pull"
	"field-service/internal/push"
	"field-service/internal/species"
	"field-service/internal/verify"
	"field-service/internal/wiring"
)

func main() {
	if err := run(); err != nil {
		slog.Error("field-service stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := configFromEnv()
	if err != nil {
		return err
	}

	pool, err := pgxpool.New(ctx, cfg.databaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		// Refusing to start without a database is deliberate. A service that
		// answers 500 for every request because it cannot reach Postgres looks
		// exactly like a service under attack, and an operator investigating a
		// suspected intrusion should not be sent to the database by accident.
		return err
	}

	redis := redis.NewClient(&redis.Options{Addr: cfg.redisAddr})
	defer func() { _ = redis.Close() }()
	if err := redis.Ping(ctx).Err(); err != nil {
		// Redis failing closed is a revocation policy decision, not an accident,
		// so the service does not start rather than starting and refusing
		// everything with an unhelpful reason. A denylist that cannot be read
		// cannot be said to have been consulted.
		return err
	}

	cache := jwks.New(cfg.jwksURL, jwks.WithInterval(cfg.jwksRefresh))
	validator, err := authn.NewValidator(cache, cfg.audience, cfg.issuer)
	if err != nil {
		return err
	}
	deny := wiring.NewDenylist(redis)
	session := wiring.NewSession(wiring.NewPgxPool(pool))
	guard := httpapi.NewGuard(
		wiring.NewVerifier(validator), deny, session, logAdapter{log}, cfg.scope,
	)

	// The push service opens its own transaction, so it is built once over the
	// pool. The pull service cannot be: a pull must read inside the transaction
	// carrying the caller's grants, and those settings are transaction-local. So
	// the pull route gets a function and is handed the query handle the session
	// opened, which is why the read boundary has exactly one source.
	pushSvc := push.New(push.NewPgxStore(pool))
	sync := &httpapi.Sync{
		Push:    pushSvc,
		Session: session,
		Pull: func(ctx context.Context, caller, cursor string, q httpapi.Querier) (pull.Page, error) {
			return pull.New(pull.NewPgxStore(q)).Pull(ctx, caller, cursor)
		},
		Log: logAdapter{log},
	}

	// The catalogue is reference data with no context of its own, so it is built
	// over the pool rather than over the session's read handle: opening a
	// transaction to set grants for a table that has no context column would be
	// ceremony implying an isolation that does not exist.
	catalogue := &httpapi.Catalogue{
		Catalogue: species.New(species.NewPgxStore(pool)),
		Log:       logAdapter{log},
	}

	mux := http.NewServeMux()
	mux.Handle("/api/v1/sync/", httpapi.NewSyncRoutes(guard, sync))
	mux.Handle("/api/v1/species", httpapi.NewCatalogueRoutes(guard, catalogue))
	mux.Handle("/api/v1/species/", httpapi.NewCatalogueRoutes(guard, catalogue))

	// The competency catalogue, for the same reason and over the same pool.
	comps := &httpapi.Competencies{
		Competencies: competency.New(competency.NewPgxStore(pool)),
		Log:          logAdapter{log},
	}
	mux.Handle("/api/v1/competencies", httpapi.NewCompetencyRoutes(guard, comps))
	mux.Handle("/api/v1/competencies/", httpapi.NewCompetencyRoutes(guard, comps))

	// The verification door, reached only with a service token. It takes the pool
	// and not the session's read handle: a service token carries no context, so
	// there are no grants to set and every context rule it applies is written in
	// the request rather than inherited from the token.
	verifier := &httpapi.Verification{
		Verify: verify.New(verify.NewPgxStore(pool)),
		Log:    logAdapter{log},
	}
	mux.Handle("/api/v1/log-book/verify", httpapi.NewVerificationRoutes(guard, verifier))
	// Health is the one route outside the guard, deliberately: a liveness probe
	// cannot present a token, and a probe that needs credentials is a probe that
	// fails when the thing it is probing is broken.
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	srv := &http.Server{
		Addr:    cfg.listen,
		Handler: mux,
		// A read timeout so a slow client cannot hold a connection open
		// indefinitely. A write timeout for the same reason on the response side.
		// Idle is short because devices hold connections between syncs and a
		// device that reconnects every few minutes is the expected pattern here.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	errs := make(chan error, 1)
	go func() {
		log.Info("field-service listening",
			"addr", cfg.listen, "audience", cfg.audience, "issuer", cfg.issuer)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}

	// Drain rather than cut: a device mid-push has already been told nothing, and
	// closing the listener underneath it turns a completed batch into a
	// deferral the client will have to reason about for no gain.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	log.Info("draining")
	return srv.Shutdown(shutdownCtx)
}

// config is the service's whole configuration surface.
type config struct {
	listen      string
	databaseURL string
	redisAddr   string
	jwksURL     string
	issuer      string
	audience    string
	scope       string
	jwksRefresh time.Duration
}

// configFromEnv reads configuration, refusing anything missing.
//
// There are no defaults for the issuer, the audience or the JWKS URL, and that
// is the point. A validator built with a blank issuer would accept a token from
// anyone; one built with a blank audience would accept a token minted for a
// different service. Both would pass every check while enforcing nothing, so the
// process declines to start rather than run misconfigured.
func configFromEnv() (config, error) {
	c := config{
		listen:      env("FIELDSVC_LISTEN", ":8080"),
		databaseURL: os.Getenv("FIELDSVC_DATABASE_URL"),
		redisAddr:   env("FIELDSVC_REDIS_ADDR", "127.0.0.1:6379"),
		jwksURL:     os.Getenv("FIELDSVC_JWKS_URL"),
		issuer:      os.Getenv("FIELDSVC_ISSUER"),
		audience:    os.Getenv("FIELDSVC_AUDIENCE"),
		scope:       env("FIELDSVC_REQUIRED_SCOPE", "field:write"),
		jwksRefresh: 15 * time.Minute,
	}
	for name, value := range map[string]string{
		"FIELDSVC_DATABASE_URL": c.databaseURL,
		"FIELDSVC_JWKS_URL":     c.jwksURL,
		"FIELDSVC_ISSUER":       c.issuer,
		"FIELDSVC_AUDIENCE":     c.audience,
	} {
		if value == "" {
			return config{}, errors.New(name + " is required and has no default")
		}
	}
	if raw := os.Getenv("FIELDSVC_JWKS_REFRESH"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return config{}, err
		}
		c.jwksRefresh = d
	}
	return c, nil
}

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// logAdapter sends the guard's refusals to slog.
//
// It logs the reason the guard refuses and the request's identity, and that is
// the only reason anything is ever logged about a refusal: the client is told
// nothing, so the log is the only place the truth exists. A logger that dropped
// the reason would make an opaque refusal safe to send and impossible to
// investigate.
type logAdapter struct{ log *slog.Logger }

var _ httpapi.Logger = logAdapter{}

func (l logAdapter) Refused(r *http.Request, reason string, err error) {
	// Nil-tolerant on purpose. The guard always has a request, but a logger that
	// panics on a nil one takes down the handler that was trying to report a
	// fault — which is how a push failure came to be reported as an empty
	// response with nothing in the log.
	attrs := []any{"reason", reason}
	if r != nil {
		attrs = append(attrs, "method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr)
	} else {
		attrs = append(attrs, "request", "none")
	}
	l.log.Warn("refused", append(attrs, "error", errString(err))...)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
