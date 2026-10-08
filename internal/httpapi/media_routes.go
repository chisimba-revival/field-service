package httpapi

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"field-service/internal/media"
)

// MaxMediaBody bounds a media upload-target request.
//
// Smaller than a sign-off because the request is just an id and a content
// type — the actual bytes go to whichever backend answered, never through
// this service.
const MaxMediaBody = 16 << 10

// Media holds what the media routes need.
type Media struct {
	// Media is the upload-target service. A field rather than a parameter
	// because the tests need to substitute a fake that never touches the
	// network, and the real backend is chosen at startup from config.
	Media *media.Service
	Log   Logger
}

// NewMediaRoutes builds the media routes, already wrapped in the guard.
func NewMediaRoutes(g *Guard, m *Media) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /api/v1/media/upload", g.Serve(m.uploadHandler()))
	return mux
}

// uploadRequest is what the client sends to get an upload target.
type uploadRequest struct {
	ContentType string `json:"content_type"`
	// MediaID is optional. A client may supply its own id — typically a
	// uuid it generated while offline — and field-service will use it. If
	// omitted, one is generated here.
	MediaID string `json:"media_id,omitempty"`
}

// uploadHandler returns an upload target for a new media upload.
//
// The id is generated here rather than by the client unless the client
// supplied one, so the same id can serve as the object key for s3 or the
// client-side filename for chisimba. A client supplying its own id is the
// ordinary case: it picked one while offline and needs the upload to land
// under it.
func (m *Media) uploadHandler() Handler {
	return func(w http.ResponseWriter, r *http.Request, c Caller) error {
		var req uploadRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxMediaBody))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				return problem(w, http.StatusRequestEntityTooLarge, "body_too_large",
					"That request is too long to accept.")
			}
			return problem(w, http.StatusBadRequest, "unreadable_body",
				"That request could not be read. The field names must match exactly.")
		}
		if dec.More() {
			return problem(w, http.StatusBadRequest, "unreadable_body",
				"That body carried more than one request.")
		}

		if req.MediaID == "" {
			req.MediaID = generateUUID()
		}

		t, err := m.Media.Target(r.Context(), req.MediaID, req.ContentType)
		if errors.Is(err, media.ErrNotConfigured) {
			return problem(w, http.StatusServiceUnavailable, "media_not_configured",
				"Media upload is not configured on this installation.")
		}
		if err != nil {
			return m.internal(w, r, err)
		}

		return writeJSON(w, http.StatusOK, map[string]any{"data": t})
	}
}

// internal reports a fault to the log and answers without describing it.
func (m *Media) internal(w http.ResponseWriter, r *http.Request, err error) error {
	if m.Log != nil {
		m.Log.Refused(r, "handler_failed", err)
	}
	return problem(w, http.StatusInternalServerError, "internal_error",
		"That did not go through. Nothing was changed.")
}

// generateUUID returns a new random UUID. It is here rather than in the
// media package because the HTTP layer is the only place that needs it:
// the media package accepts an id from its caller, and the caller is the
// one that decided whether to generate one.
func generateUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	// Set version 4 and variant bits, per RFC 4122.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
