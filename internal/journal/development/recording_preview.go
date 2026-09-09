package development

import (
	"crypto/sha256"
	"encoding/json"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/journal/voice"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

type recordingPreviewRequest struct {
	RequestID      string         `json:"requestID"`
	AudioHash      string         `json:"audioHash"`
	ReviewRevision int            `json:"reviewRevision"`
	Snapshot       voice.Snapshot `json:"snapshot"`
}
type recordingPreviewResponse struct {
	RequestID      string         `json:"requestID"`
	ReviewRevision int            `json:"reviewRevision"`
	Revision       voice.Revision `json:"revision"`
}
type previewCacheEntry struct {
	digest   [32]byte
	response *recordingPreviewResponse
	expires  time.Time
}
type recordingPreviewCache struct {
	mu      sync.Mutex
	entries map[string]previewCacheEntry
}

// The source's owner/hash remains verifiable after its temporary ASR result
// expires. Rewrites use the existing token budget, never recording-minute quota.
func (h *recordingHTTP) preview(w http.ResponseWriter, r *http.Request, owner, archiveID string) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		deny(w, 413, "recording_preview_too_large")
		return
	}
	defer r.Body.Close()
	var input recordingPreviewRequest
	if json.Unmarshal(body, &input) != nil || input.ReviewRevision < 0 || input.ReviewRevision > 1_000_000 {
		deny(w, 422, "recording_preview_invalid")
		return
	}
	if _, err := uuid.Parse(input.RequestID); err != nil || len(input.RequestID) != 36 || input.Snapshot.RecordingContext == nil || input.Snapshot.Validate() != nil {
		deny(w, 422, "recording_preview_invalid")
		return
	}
	job, err := h.executor.Store.Get(owner, archiveID)
	if os.IsNotExist(err) {
		deny(w, 404, "recording_not_found")
		return
	}
	if err != nil {
		deny(w, 503, "recording_analysis_unavailable")
		return
	}
	context := input.Snapshot.RecordingContext
	if job.AudioHash != input.AudioHash || job.ProviderTaskID != context.Analysis.TaskID || job.Milliseconds != context.Analysis.Milliseconds || job.State != voice.RecordingCompleted {
		deny(w, 409, "recording_preview_source_changed")
		return
	}
	key := owner + "/" + archiveID + "/" + input.RequestID
	digest := sha256.Sum256(body)
	h.previews.mu.Lock()
	for k, value := range h.previews.entries {
		if value.response != nil && !value.expires.After(h.now()) {
			delete(h.previews.entries, k)
		}
	}
	if cached, ok := h.previews.entries[key]; ok {
		h.previews.mu.Unlock()
		if cached.digest != digest {
			deny(w, 409, "recording_preview_request_changed")
			return
		}
		if cached.response == nil {
			deny(w, 409, "recording_preview_processing")
			return
		}
		writePreview(w, *cached.response)
		return
	}
	if len(h.previews.entries) >= 16 {
		h.previews.mu.Unlock()
		deny(w, 429, "recording_preview_busy")
		return
	}
	h.previews.entries[key] = previewCacheEntry{digest: digest}
	h.previews.mu.Unlock()
	result, err := h.rewriter.Rewrite(r.Context(), input.Snapshot, input.ReviewRevision)
	if err == nil && (result.Revision.TranscriptRevision != input.ReviewRevision || result.Revision.Validate(input.Snapshot) != nil || voice.ValidateRecordingDialogueRevision(input.Snapshot, result.Revision) != nil) {
		err = voice.ErrInvalid
	}
	response := recordingPreviewResponse{input.RequestID, input.ReviewRevision, result.Revision}
	h.previews.mu.Lock()
	if err != nil {
		delete(h.previews.entries, key)
	} else {
		h.previews.entries[key] = previewCacheEntry{digest: digest, response: &response, expires: h.now().Add(5 * time.Minute)}
	}
	h.previews.mu.Unlock()
	if err != nil {
		deny(w, 503, "recording_preview_unavailable")
		return
	}
	writePreview(w, response)
}
func writePreview(w http.ResponseWriter, result recordingPreviewResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Journal-Recording-Protocol", voice.RecordingAnalysisVersion)
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(result)
}
