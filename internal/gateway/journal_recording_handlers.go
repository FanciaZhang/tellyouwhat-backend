package gateway

import (
	"bytes"
	"context"
	"encoding/hex"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/journalhttpapi"
	"github.com/tellyouwhat/backend/internal/privacy"
)

func (s *Server) serveRecording(ctx context.Context, requestID, recordingID uuid.UUID, digest string) {
	c := strictGinContext(ctx)
	c.Header("Cache-Control", "no-store")
	fail := func(status int, code string) { writeTransportError(c, status, code, "recording request denied") }
	if s.journalRecordings == nil {
		fail(503, "recording_not_enabled")
		return
	}
	if requestID == uuid.Nil || recordingID == uuid.Nil {
		fail(422, "recording_invalid_id")
		return
	}
	proof := apiRequestProof(ctx, requestID)
	if c.Request.Method == http.MethodPut {
		value, err := hex.DecodeString(digest)
		if err != nil || len(value) != 32 || digest != strings.ToLower(digest) {
			fail(422, "recording_digest_required")
			return
		}
		proof.BodySHA256 = digest
	}
	p, f := s.apiAuthenticateProof(ctx, requestID, proof)
	if f != nil {
		fail(f.status, f.code)
		return
	}
	if c.Request.Method != http.MethodGet {
		if f = s.apiRequireManagedEntitlement(ctx, p, requestID.String()); f != nil {
			fail(f.status, f.code)
			return
		}
		operation := "journal.voice.recording"
		if strings.HasSuffix(c.Request.URL.Path, "/preview") {
			operation = "journal.voice.rewrite"
		}
		if f = s.operationsFailure(ctx, operation, requestID.String()); f != nil {
			fail(f.status, f.code)
			return
		}
	}
	if f = s.apiRequireConsents(ctx, p, []string{privacy.ManagedAIScope}, requestID.String()); f != nil {
		fail(f.status, f.code)
		return
	}
	if c.Request.Method != http.MethodPut {
		// Strict JSON binding consumed its reader; reuse the authenticated bytes.
		c.Request.Body = io.NopCloser(bytes.NewReader(rawRequestBody(c)))
	}
	s.journalRecordings.ServeAuthorized(c.Writer, c.Request, p.KeyID, digest)
}

func (s *Server) UploadJournalRecording(ctx context.Context, r journalhttpapi.UploadJournalRecordingRequestObject) (journalhttpapi.UploadJournalRecordingResponseObject, error) {
	s.serveRecording(ctx, r.Params.XTellyouwhatRequestID, r.RecordingID, r.Params.XTellyouwhatContentSHA256)
	return nil, nil
}
func (s *Server) GetJournalRecording(ctx context.Context, r journalhttpapi.GetJournalRecordingRequestObject) (journalhttpapi.GetJournalRecordingResponseObject, error) {
	s.serveRecording(ctx, r.Params.XTellyouwhatRequestID, r.RecordingID, "")
	return nil, nil
}
func (s *Server) ProcessJournalRecording(ctx context.Context, r journalhttpapi.ProcessJournalRecordingRequestObject) (journalhttpapi.ProcessJournalRecordingResponseObject, error) {
	s.serveRecording(ctx, r.Params.XTellyouwhatRequestID, r.RecordingID, "")
	return nil, nil
}
func (s *Server) RetryJournalRecording(ctx context.Context, r journalhttpapi.RetryJournalRecordingRequestObject) (journalhttpapi.RetryJournalRecordingResponseObject, error) {
	s.serveRecording(ctx, r.Params.XTellyouwhatRequestID, r.RecordingID, "")
	return nil, nil
}
func (s *Server) PreviewJournalRecording(ctx context.Context, r journalhttpapi.PreviewJournalRecordingRequestObject) (journalhttpapi.PreviewJournalRecordingResponseObject, error) {
	s.serveRecording(ctx, r.Params.XTellyouwhatRequestID, r.RecordingID, "")
	return nil, nil
}
