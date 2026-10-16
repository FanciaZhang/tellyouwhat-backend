package recordinghttp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"mime"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/journal/voice"
)

const Prefix = "/v1/journal/voice/recordings/"
const maxRecordingUploadBytes = voice.SessionMilliseconds*64 + 44

func Route(r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, Prefix) {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, Prefix), "/")
	return len(parts) == 1 && (r.Method == http.MethodPut || r.Method == http.MethodGet) ||
		len(parts) == 2 && (parts[1] == "process" || parts[1] == "preview" || parts[1] == "retry") && r.Method == http.MethodPost
}

type Handler struct {
	rewriter  voice.Rewriter
	previews  recordingPreviewCache
	executor  *voice.RecordingExecutor
	now       func() time.Time
	admission chan struct{}
}

func New(executor *voice.RecordingExecutor, rewriter voice.Rewriter, now func() time.Time) *Handler {
	return &Handler{executor: executor, rewriter: rewriter, now: now, admission: make(chan struct{}, 2), previews: recordingPreviewCache{entries: map[string]previewCacheEntry{}}}
}

// ServeAuthorized is called only after the transport authenticates the owner and body digest.
func (h *Handler) ServeAuthorized(w http.ResponseWriter, r *http.Request, owner, digest string) {
	if owner == "" || !Route(r) || h.executor == nil || h.executor.Store == nil {
		deny(w, 503, "recording_analysis_unavailable")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, Prefix), "/")
	parsed, err := uuid.Parse(parts[0])
	if err != nil || len(parts[0]) != 36 {
		deny(w, 422, "recording_invalid_id")
		return
	}
	id := parsed.String()
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
	var retryRevision int
	if len(parts) == 2 && parts[1] == "retry" {
		defer r.Body.Close()
		var input struct {
			Revision int `json:"revision"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil || input.Revision < 1 {
			deny(w, 422, "recording_invalid_retry")
			return
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			deny(w, 422, "recording_invalid_retry")
			return
		}
		retryRevision = input.Revision
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
		var input io.Reader = body
		if digest != "" {
			input = &digestReader{Reader: body, expected: digest, hash: sha256.New()}
		}
		job, err = h.executor.Store.Upload(r.Context(), owner, id, input)
	case http.MethodGet, http.MethodPost:
		// Process and status requests have no document/audio body.
		if r.Body != nil && retryRevision == 0 {
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
			if retryRevision > 0 {
				job, err = h.executor.Retry(r.Context(), owner, id, retryRevision)
			} else {
				job, err = h.executor.Process(r.Context(), owner, id)
			}
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

const DigestHeader = "X-Tellyouwhat-Content-SHA256"

type digestReader struct {
	io.Reader
	expected string
	hash     hash.Hash
}

func (r *digestReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	_, _ = r.hash.Write(p[:n])
	if err == io.EOF && hex.EncodeToString(r.hash.Sum(nil)) != r.expected {
		return n, voice.ErrInvalid
	}
	return n, err
}
func deny(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": "recording request denied", "requestID": ""}})
}
