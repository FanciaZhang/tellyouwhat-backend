package development

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/entitlement"
	"github.com/tellyouwhat/backend/internal/journal/voice"
	"github.com/tellyouwhat/backend/internal/privacy"
)

const recordingPrefix = "/v1/journal/voice/recordings/"
const maxRecordingUploadBytes = voice.SessionMilliseconds*64 + 44

func recordingRoute(r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, recordingPrefix) {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, recordingPrefix), "/")
	return len(parts) == 1 && (r.Method == http.MethodPut || r.Method == http.MethodGet) ||
		len(parts) == 2 && (parts[1] == "process" || parts[1] == "preview") && r.Method == http.MethodPost
}

type recordingHTTP struct {
	rewriter     voice.Rewriter
	previews     recordingPreviewCache
	executor     *voice.RecordingExecutor
	entitlements entitlement.Store
	consent      *privacy.Service
	now          func() time.Time
	// Bound disk/HTTP work as well as billed provider work. Each upload streams
	// at most one session; neither gateway middleware nor this handler buffers it.
	admission chan struct{}
}

func newRecordingHTTP(e *voice.RecordingExecutor, records entitlement.Store, consent *privacy.Service, now func() time.Time) *recordingHTTP {
	return &recordingHTTP{executor: e, entitlements: records, consent: consent, now: now, admission: make(chan struct{}, 2), previews: recordingPreviewCache{entries: map[string]previewCacheEntry{}}}
}

func (h *recordingHTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	principal, ok := r.Context().Value(principalContextKey{}).(attestation.Principal)
	if !ok || principal.AppID != "journal" {
		deny(w, 401, "development_access_denied")
		return
	}
	record, ok, err := h.entitlements.Get(r.Context(), principal.KeyID)
	if err != nil || !ok || record.Environment != "development" || record.TransactionID == "" || record.StartedAt.IsZero() {
		deny(w, 403, "managed_subscription_required")
		return
	}
	// Reading already-produced results does not require renewing a subscription.
	if r.Method != http.MethodGet && !record.ExpiresAt.After(h.now()) {
		deny(w, 403, "managed_subscription_required")
		return
	}
	granted, err := h.consent.HasRequiredConsents(r.Context(), principal, []string{privacy.ManagedAIScope})
	if err != nil || !granted {
		deny(w, 403, "voice_consent_required")
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, recordingPrefix), "/")
	parsed, err := uuid.Parse(parts[0])
	if err != nil || len(parts[0]) != 36 {
		deny(w, 422, "recording_invalid_id")
		return
	}
	id := parsed.String()
	owner := record.Environment + ":" + record.TransactionID
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	r = r.WithContext(ctx)
	connection := http.NewResponseController(w)
	_ = connection.SetReadDeadline(time.Now().Add(3 * time.Minute))
	_ = connection.SetWriteDeadline(time.Now().Add(4 * time.Minute))
	defer func() { _ = connection.SetReadDeadline(time.Time{}); _ = connection.SetWriteDeadline(time.Time{}) }()
	select {
	case h.admission <- struct{}{}:
		defer func() { <-h.admission }()
	default:
		w.Header().Set("Retry-After", "3")
		deny(w, 429, "recording_busy")
		return
	}
	if len(parts) == 2 && parts[1] == "preview" {
		h.preview(w, r, owner, id)
		return
	}
	var job voice.RecordingJob
	switch r.Method {
	case http.MethodPut:
		mediaType, _, parseErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if parseErr != nil || mediaType != "audio/wav" {
			deny(w, 415, "recording_wav_required")
			return
		}
		if r.ContentLength > maxRecordingUploadBytes {
			deny(w, 413, "recording_too_large")
			return
		}
		body := http.MaxBytesReader(w, r.Body, maxRecordingUploadBytes)
		defer body.Close()
		job, err = h.executor.Store.Upload(r.Context(), owner, id, body)
	case http.MethodGet, http.MethodPost:
		// Process and status requests have no document/audio body.
		if r.Body != nil {
			defer r.Body.Close()
			body, readErr := io.ReadAll(io.LimitReader(r.Body, 1))
			if readErr != nil || len(body) != 0 {
				deny(w, 422, "unexpected_body")
				return
			}
		}
		job, err = h.executor.Store.Get(owner, id)
		if err == nil && (h.now().Sub(job.UpdatedAt) >= 24*time.Hour || job.ErrorCode == "analysis_result_expired") {
			deny(w, 410, "analysis_result_expired")
			return
		}
		if err == nil && r.Method == http.MethodPost {
			job, err = h.executor.Process(r.Context(), owner, id)
		}
	}
	if err != nil {
		status, code := 503, "recording_analysis_unavailable"
		var large *http.MaxBytesError
		switch {
		case errors.As(err, &large):
			status, code = 413, "recording_too_large"
		case errors.Is(err, os.ErrNotExist):
			status, code = 404, "recording_not_found"
		case errors.Is(err, voice.ErrInvalid):
			status, code = 422, "recording_invalid"
		case errors.Is(err, voice.ErrConflict):
			status, code = 409, "recording_conflict"
		case errors.Is(err, costcontrol.ErrBudgetExceeded):
			status, code = 429, "recording_budget_exceeded"
		case errors.Is(err, costcontrol.ErrConcurrencyExceeded):
			status, code = 429, "recording_busy"
		}
		if status == 429 || status == 503 {
			w.Header().Set("Retry-After", "3")
		}
		deny(w, status, code)
		return
	}
	// Duplicate PUT requests must not expose expired cached content either.
	if h.now().Sub(job.UpdatedAt) >= 24*time.Hour || job.ErrorCode == "analysis_result_expired" {
		deny(w, 410, "analysis_result_expired")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Journal-Recording-Protocol", voice.RecordingAnalysisVersion)
	if job.State != voice.RecordingCompleted && job.State != voice.RecordingFailed {
		w.WriteHeader(http.StatusAccepted)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	_ = json.NewEncoder(w).Encode(job)
}
