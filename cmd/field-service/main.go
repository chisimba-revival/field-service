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
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"field-service/internal/authn"
	"field-service/internal/competency"
	"field-service/internal/httpapi"
	"field-service/internal/jwks"
	"field-service/internal/media"
	"field-service/internal/outings"
	"field-service/internal/pull"
	"field-service/internal/push"
	"field-service/internal/signoff"
	"field-service/internal/species"
	"field-service/internal/verify"
	"field-service/internal/wiring"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		healthcheck()
		return
	}
	if err := run(); err != nil {
		slog.Error("field-service stopped", "error", err)
		os.Exit(1)
	}
}

// defaultPrewarmWindow is how long the process keeps trying to load the JWKS
// document in the background before giving up quietly.
//
// It has to outlast a slow Chisimba start, because that is the case it is for:
// the issuer is a separate compose project and no depends_on can order it. Past
// the window the warm stops and the first real key id pays for the fetch, which
// is the behaviour the service had before this existed and is not a failure.
const defaultPrewarmWindow = 60 * time.Second

// prewarmBackoff is the pause between background JWKS attempts. Fixed rather
// than configured because there is nothing to tune: the loop is bounded by the
// window, and an operator who wants fewer attempts wants a shorter window.
const prewarmBackoff = 2 * time.Second

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

	cache := jwks.New(cfg.jwksURL,
		jwks.WithInterval(cfg.jwksRefresh),
		jwks.WithTimeout(cfg.jwksTimeout))
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
	// Sync is reached by both doors: a device presents the person's own token
	// and Chisimba's module presents a service token, so the route dispatches on
	// the token's type rather than being wired to a single door.
	mux.Handle("/api/v1/sync/", httpapi.NewSyncDispatchRoutes(guard, sync, cfg.scope))
	// Sign-offs are written through the ordinary person door, which does set
	// grants, so the write context is the caller's active context, read from the
	// authenticated Caller by the handler — the same rule that governs a sighting.
	// The service door is reserved for verification and is refused here by
	// construction, not by a rule someone has to remember.
	signoffSvc := signoff.New(
		signoff.NewPgxStore(pool),
		signoff.NewPgCatalogue(pool),
		signoff.NewPgOutings(pool).InContext,
	)
	// The programme administrator's group, so the sign-off read rule can widen
	// to an administrator in the same context. Left unset it widens to nobody,
	// which is the safe default but not the intended one.
	signoffSvc.AdminRole = "Admin"
	signoffs := &httpapi.Signoffs{
		Signoffs: signoffSvc,
		Log:      logAdapter{log},
	}

	// Sign-off routes use Go 1.22+ method-based routing (POST /api/v1/signoffs,
	// GET /api/v1/signoffs/{id}, etc.) registered on a sub-mux.
	mux.Handle("/api/v1/signoffs", httpapi.NewSignoffRoutes(guard, signoffs))
	mux.Handle("/api/v1/signoffs/", httpapi.NewSignoffRoutes(guard, signoffs))
	mux.Handle("/api/v1/species", httpapi.NewCatalogueRoutes(guard, catalogue))
	mux.Handle("/api/v1/species/", httpapi.NewCatalogueRoutes(guard, catalogue))

	// The competency catalogue, for the same reason and over the same pool.
	comps := &httpapi.Competencies{
		Competencies: competency.New(competency.NewPgxStore(pool)),
		Log:          logAdapter{log},
	}
	mux.Handle("/api/v1/competencies", httpapi.NewCompetencyRoutes(guard, comps))
	mux.Handle("/api/v1/competencies/", httpapi.NewCompetencyRoutes(guard, comps))

	// Outings are reference data scoped to the caller's context grants.
	// The Serve() handler uses method-based routing internally.
	outingsStore := outings.NewStore(pool)
	mux.Handle("/api/v1/outings", httpapi.NewOutingsRoutes(guard, outingsStore).Serve())
	mux.Handle("/api/v1/outings/", httpapi.NewOutingsRoutes(guard, outingsStore).Serve())

	// Media upload URLs. The backend is chosen from config: chisimba (the
	// default) points devices at the gateway's own file API, s3 points them
	// directly at an object store. The service is built once here because
	// a target request does not touch the database.
	var mediaBackend media.Backend
	switch cfg.mediaBackend {
	case "s3":
		mediaBackend = media.NewS3(nil, cfg.mediaBucket, cfg.mediaPrefix)
	default: // "chisimba" and anything unrecognised
		mediaBackend = media.NewChisimba(cfg.mediaGatewayURL)
	}
	mediaRoutes := &httpapi.Media{Media: media.New(mediaBackend), Log: logAdapter{log}}
	mux.Handle("/api/v1/media/", httpapi.NewMediaRoutes(guard, mediaRoutes))
	// and not the session's read handle: a service token carries no context, so
	// there are no grants to set and every context rule it applies is written in
	// the request rather than inherited from the token.
	verifier := &httpapi.Verification{
		Verify: verify.New(verify.NewPgxStore(pool)),
		Log:    logAdapter{log},
	}
	// Verification is a mentor/administrator action, so it requires the
	// field:verify scope rather than the field:write every other person route
	// carries.
	verifyGuard := httpapi.NewGuard(
		wiring.NewVerifier(validator), deny, session, logAdapter{log}, "field:verify",
	)
	mux.Handle("/api/v1/log-book/verify", httpapi.NewVerificationRoutes(verifyGuard, verifier))
	// Health is the one route outside the guard, deliberately: a liveness probe
	// cannot present a token, and a probe that needs credentials is a probe that
	// fails when the thing it is probing is broken.
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	// Default handler for unmatched routes - returns standardized 404 error
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not_found","message":"That endpoint does not exist.","timestamp":"` + time.Now().UTC().Format(time.RFC3339) + `"}`))
			return
		}
		// Root endpoint returns service info
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"service":"field-service","version":"1.0.0","status":"operational"}`))
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

	// Pre-warming the JWKS document is background work, and deliberately cannot
	// hold start-up up: the issuer is a separate deployment nobody can order into
	// being up first, so making it a requirement would turn "Chisimba is still
	// booting" into "field-service will not start". It retries quietly and, if it
	// never succeeds, the first real key id pays for the fetch instead — which is
	// exactly what happened before this existed. It also never gates /health,
	// which is a liveness probe about this process alone.
	if cfg.jwksPrewarm > 0 {
		go warmJWKS(ctx, cache, cfg, log)
	}

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

// warmJWKS loads the JWKS document in the background for a bounded while.
//
// The whole shape of this function is dictated by one fact: the issuer is in a
// different deployment, so nothing this process does can guarantee it is serving
// yet. Retrying quietly for a bounded window and then getting out of the way is
// therefore the correct amount of effort — enough to cover a slow Chisimba start,
// and no more, because ResolveKey already covers the case where it never happens.
func warmJWKS(ctx context.Context, c *jwks.Cache, cfg config, log *slog.Logger) {
	deadline := time.Now().Add(cfg.jwksPrewarm)
	for {
		err := c.Warm(ctx)
		if err == nil {
			log.Info("jwks pre-warmed", "url", cfg.jwksURL)
			return
		}
		// Shutting down: a warm that survives its process would hold the exit open
		// for the rest of the fetch timeout to learn nothing.
		if ctx.Err() != nil {
			return
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			// Deliberately a Warn and not an error, and deliberately not fatal.
			// Getting here means "the first token will fetch it instead", which is
			// the behaviour this service had before pre-warming existed.
			log.Warn("jwks pre-warm did not succeed; the first token will fetch it",
				"url", cfg.jwksURL, "tried_for", cfg.jwksPrewarm, "error", err)
			return
		}
		log.Debug("jwks pre-warm failed, retrying", "error", err)
		if remaining > prewarmBackoff {
			remaining = prewarmBackoff
		}
		timer := time.NewTimer(remaining)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// config is the service's whole configuration surface.
type config struct {
	listen          string
	databaseURL     string
	redisAddr       string
	jwksURL         string
	issuer          string
	audience        string
	scope           string
	jwksRefresh     time.Duration
	jwksTimeout     time.Duration
	jwksPrewarm     time.Duration
	mediaBackend    string
	mediaGatewayURL string
	mediaBucket     string
	mediaPrefix     string
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
		listen:          env("FIELDSVC_LISTEN", ":8080"),
		databaseURL:     os.Getenv("FIELDSVC_DATABASE_URL"),
		redisAddr:       env("FIELDSVC_REDIS_ADDR", "127.0.0.1:6379"),
		jwksURL:         os.Getenv("FIELDSVC_JWKS_URL"),
		issuer:          os.Getenv("FIELDSVC_ISSUER"),
		audience:        os.Getenv("FIELDSVC_AUDIENCE"),
		scope:           env("FIELDSVC_REQUIRED_SCOPE", "field:write"),
		jwksRefresh:     15 * time.Minute,
		jwksTimeout:     jwks.DefaultTimeout,
		jwksPrewarm:     defaultPrewarmWindow,
		mediaBackend:    env("FIELDSVC_MEDIA_BACKEND", "chisimba"),
		mediaGatewayURL: env("FIELDSVC_MEDIA_GATEWAY_URL", "http://web/api/v1"),
		mediaBucket:     env("FIELDSVC_MEDIA_BUCKET", ""),
		mediaPrefix:     env("FIELDSVC_MEDIA_PREFIX", "media/"),
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
	// Bounds one attempt to read the issuer's document, whether that attempt is a
	// warm at start-up or a refresh behind a token. The document is small and this
	// service is offline-first, so a slow read is a fault rather than a condition
	// to wait out.
	if raw := os.Getenv("FIELDSVC_JWKS_TIMEOUT"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return config{}, fmt.Errorf("FIELDSVC_JWKS_TIMEOUT: %w", err)
		}
		c.jwksTimeout = d
	}
	// How long to keep pre-warming before falling back to lazy fetching. Zero or
	// negative disables it, which is a legitimate choice for a deployment that
	// would rather not talk to the issuer until a token forces it.
	if raw := os.Getenv("FIELDSVC_JWKS_PREWARM"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return config{}, fmt.Errorf("FIELDSVC_JWKS_PREWARM: %w", err)
		}
		c.jwksPrewarm = d
	}
	return c, nil
}

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// healthcheck is how this image answers "is the process serving".
//
// A container HEALTHCHECK cannot ask the question itself: the runtime image is
// distroless, which carries no shell and no curl, so there is no command that
// could probe the listener. The binary answers it instead, calling its own
// /health route over loopback. That route sits outside the auth guard
// deliberately — a probe that must present a token is a probe that fails when
// the service is healthy.
//
// This is a liveness probe, not a readiness probe. It reports only that the
// listener accepts connections, and deliberately says nothing about Postgres,
// Redis or Chisimba, each of which can be unreachable while the process is
// still worth keeping alive. Claiming readiness on their behalf would be a
// judgement this process has no basis to make, and would take the container
// down for a problem that does not need it taken down.
func healthcheck() {
	listen := env("FIELDSVC_LISTEN", ":8080")
	if strings.HasPrefix(listen, ":") {
		// A bare port binds every interface; a probe belongs on loopback.
		listen = "127.0.0.1" + listen
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + listen + "/health")
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: /health returned %d\n", resp.StatusCode)
		os.Exit(1)
	}
	os.Exit(0)
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
