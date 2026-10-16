package development

import (
	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/entitlement"
	"github.com/tellyouwhat/backend/internal/journal/recordinghttp"
	"github.com/tellyouwhat/backend/internal/journal/voice"
	"github.com/tellyouwhat/backend/internal/privacy"
	"net/http"
	"time"
)

type recordingHTTP struct {
	handler *recordinghttp.Handler
	records entitlement.Store
	consent *privacy.Service
	now     func() time.Time
}

func newRecordingHTTP(e *voice.RecordingExecutor, rewriter voice.Rewriter, records entitlement.Store, consent *privacy.Service, now func() time.Time) *recordingHTTP {
	return &recordingHTTP{recordinghttp.New(e, rewriter, now), records, consent, now}
}
func (h *recordingHTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	principal, ok := r.Context().Value(principalContextKey{}).(attestation.Principal)
	if !ok || principal.AppID != "journal" {
		deny(w, 401, "development_access_denied")
		return
	}
	record, ok, err := h.records.Get(r.Context(), principal.KeyID)
	if err != nil || !ok || record.Environment != "development" || record.TransactionID == "" || record.StartedAt.IsZero() {
		deny(w, 403, "managed_subscription_required")
		return
	}
	if r.Method != http.MethodGet && !record.ExpiresAt.After(h.now()) {
		deny(w, 403, "managed_subscription_required")
		return
	}
	granted, err := h.consent.HasRequiredConsents(r.Context(), principal, []string{privacy.ManagedAIScope})
	if err != nil || !granted {
		deny(w, 403, "voice_consent_required")
		return
	}
	h.handler.ServeAuthorized(w, r, record.Environment+":"+record.TransactionID, "")
}
