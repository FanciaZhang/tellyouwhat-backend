package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/journal/readaloud"
	"github.com/tellyouwhat/backend/internal/journalhttpapi"
	"github.com/tellyouwhat/backend/internal/privacy"
)

type ReadAloudAccess interface {
	Grant(context.Context, string, string) (readaloud.Grant, error)
	Subject(context.Context, string) (readaloud.Subject, error)
	Consent(context.Context, string, string) error
}

func speechFailure(err error, id string) *apiFailure {
	switch {
	case errors.Is(err, readaloud.ErrConsentRequired):
		return newAPIFailure(403, "read_aloud_consent_required", "Cloud read-aloud consent is required", id)
	case errors.Is(err, readaloud.ErrSubscriptionRequired):
		return newAPIFailure(403, "managed_subscription_required", "Subscription required", id)
	case errors.Is(err, readaloud.ErrSubscriptionSync):
		return newAPIFailure(409, "read_aloud_subscription_sync_required", "Restore the subscription to synchronize its period", id)
	case errors.Is(err, readaloud.ErrVoiceChanged):
		return newAPIFailure(409, "read_aloud_voice_changed", "Choose an available voice and supported settings", id)
	case errors.Is(err, readaloud.ErrRequestConflict):
		return newAPIFailure(409, "read_aloud_request_conflict", "The request identity is already bound to another synthesis", id)
	case errors.Is(err, readaloud.ErrRequestNotFound):
		return newAPIFailure(404, "not_found", "Read-aloud request not found", id)
	case errors.Is(err, readaloud.ErrAudioUnavailable):
		return newAPIFailure(410, "read_aloud_audio_unavailable", "The original audio can no longer be recovered; a new synthesis requires an explicit request", id)
	case errors.Is(err, readaloud.ErrQuotaExceeded):
		return newAPIFailure(429, "read_aloud_quota_exceeded", "Cloud read-aloud character allowance exhausted", id)
	case errors.Is(err, readaloud.ErrSessionBusy), errors.Is(err, costcontrol.ErrConcurrencyExceeded):
		return newAPIFailure(429, "read_aloud_busy", "Read-aloud service is busy", id)
	case errors.Is(err, costcontrol.ErrBudgetExceeded), errors.Is(err, costcontrol.ErrProtectionActive):
		return newAPIFailure(503, "read_aloud_budget_protected", "Cloud read-aloud spending protection is active", id)
	case errors.Is(err, readaloud.ErrInput):
		return newAPIFailure(422, "contract_violation", "Invalid read-aloud request", id)
	default:
		return newAPIFailure(503, "read_aloud_unavailable", "Cloud read-aloud is temporarily unavailable", id)
	}
}
func (s *Server) speechPrincipal(ctx context.Context, id uuid.UUID) (Principal, *apiFailure) {
	if s.readAloudSessions == nil || s.readAloudAccess == nil {
		return Principal{}, speechFailure(readaloud.ErrAccessUnavailable, id.String())
	}
	strictGinContext(ctx).Header("Cache-Control", "no-store")
	return s.apiAuthenticate(ctx, id)
}
func speechResponse(info readaloud.SessionInfo) journalhttpapi.ReadAloudSession {
	id, _ := uuid.Parse(info.ID)
	r := journalhttpapi.ReadAloudSession{Id: id, Purpose: journalhttpapi.ReadAloudSessionPurpose(info.Purpose), State: journalhttpapi.ReadAloudSessionState(info.State), AudioBytes: info.AudioBytes, LastSequence: info.LastSequence, BufferAvailable: info.BufferAvailable, ExpiresAt: info.ExpiresAt, SampleRate: 24000, Channels: 1, Encoding: "pcm_s16le"}
	if info.Problem != "" {
		problem := info.Problem
		r.Problem = &problem
	}
	return r
}
func (s *Server) CreateJournalReadAloudSynthesis(ctx context.Context, r journalhttpapi.CreateJournalReadAloudSynthesisRequestObject) (journalhttpapi.CreateJournalReadAloudSynthesisResponseObject, error) {
	id := r.Params.XTellyouwhatRequestID
	deny := func(f *apiFailure) (journalhttpapi.CreateJournalReadAloudSynthesisResponseObject, error) {
		return journalhttpapi.CreateJournalReadAloudSynthesisdefaultJSONResponse{StatusCode: f.status, Body: journalErrorResponse(f)}, nil
	}
	p, f := s.speechPrincipal(ctx, id)
	if f != nil {
		return deny(f)
	}
	if r.Body == nil || r.Body.RequestID != id {
		return deny(speechFailure(readaloud.ErrInput, id.String()))
	}
	b := r.Body
	purpose := string(b.Purpose)
	if purpose == "journal" && b.ConsentVersion != privacy.JournalReadAloudDocumentVersion {
		return deny(speechFailure(readaloud.ErrConsentRequired, id.String()))
	}
	grant, err := s.readAloudAccess.Grant(apiRequestContext(ctx), p.KeyID, purpose)
	if err != nil {
		return deny(speechFailure(err, id.String()))
	}
	info, err := s.readAloudSessions.Create(apiRequestContext(ctx), grant, readaloud.SessionCommand{Synthesis: readaloud.Synthesis{RequestID: id.String(), Text: b.Text, VoiceID: b.VoiceID, Pitch: b.Pitch, Style: string(b.Style), Dialect: b.Dialect, Language: b.Language}, VoiceRevision: b.VoiceRevision, Purpose: purpose, AccessKey: p.KeyID})
	if err != nil {
		return deny(speechFailure(err, id.String()))
	}
	return journalhttpapi.CreateJournalReadAloudSynthesis200JSONResponse(speechResponse(info)), nil
}
func (s *Server) GetJournalReadAloudSynthesis(ctx context.Context, r journalhttpapi.GetJournalReadAloudSynthesisRequestObject) (journalhttpapi.GetJournalReadAloudSynthesisResponseObject, error) {
	id := r.Params.XTellyouwhatRequestID
	deny := func(f *apiFailure) (journalhttpapi.GetJournalReadAloudSynthesisResponseObject, error) {
		return journalhttpapi.GetJournalReadAloudSynthesisdefaultJSONResponse{StatusCode: f.status, Body: journalErrorResponse(f)}, nil
	}
	p, f := s.speechPrincipal(ctx, id)
	if f != nil {
		return deny(f)
	}
	subject, err := s.readAloudAccess.Subject(apiRequestContext(ctx), p.KeyID)
	if err != nil {
		return deny(speechFailure(err, id.String()))
	}
	info, err := s.readAloudSessions.Get(apiRequestContext(ctx), subject, r.SynthesisID.String())
	if err != nil {
		return deny(speechFailure(err, id.String()))
	}
	return journalhttpapi.GetJournalReadAloudSynthesis200JSONResponse(speechResponse(info)), nil
}
func (s *Server) CancelJournalReadAloudSynthesis(ctx context.Context, r journalhttpapi.CancelJournalReadAloudSynthesisRequestObject) (journalhttpapi.CancelJournalReadAloudSynthesisResponseObject, error) {
	id := r.Params.XTellyouwhatRequestID
	deny := func(f *apiFailure) (journalhttpapi.CancelJournalReadAloudSynthesisResponseObject, error) {
		return journalhttpapi.CancelJournalReadAloudSynthesisdefaultJSONResponse{StatusCode: f.status, Body: journalErrorResponse(f)}, nil
	}
	p, f := s.speechPrincipal(ctx, id)
	if f != nil {
		return deny(f)
	}
	subject, err := s.readAloudAccess.Subject(apiRequestContext(ctx), p.KeyID)
	if err != nil {
		return deny(speechFailure(err, id.String()))
	}
	info, err := s.readAloudSessions.Cancel(apiRequestContext(ctx), subject, r.SynthesisID.String())
	if err != nil {
		return deny(speechFailure(err, id.String()))
	}
	return journalhttpapi.CancelJournalReadAloudSynthesis200JSONResponse(speechResponse(info)), nil
}
func (s *Server) GetJournalReadAloudAllowance(ctx context.Context, r journalhttpapi.GetJournalReadAloudAllowanceRequestObject) (journalhttpapi.GetJournalReadAloudAllowanceResponseObject, error) {
	id := r.Params.XTellyouwhatRequestID
	deny := func(f *apiFailure) (journalhttpapi.GetJournalReadAloudAllowanceResponseObject, error) {
		return journalhttpapi.GetJournalReadAloudAllowancedefaultJSONResponse{StatusCode: f.status, Body: journalErrorResponse(f)}, nil
	}
	p, f := s.speechPrincipal(ctx, id)
	if f != nil {
		return deny(f)
	}
	purpose := "journal"
	if r.Params.Purpose != nil {
		purpose = string(*r.Params.Purpose)
	}
	g, err := s.readAloudAccess.Grant(apiRequestContext(ctx), p.KeyID, purpose)
	if err != nil {
		return deny(speechFailure(err, id.String()))
	}
	usage, err := s.readAloudSessions.Usage(apiRequestContext(ctx), g)
	if err != nil {
		return deny(speechFailure(err, id.String()))
	}
	return journalhttpapi.GetJournalReadAloudAllowance200JSONResponse{Purpose: journalhttpapi.ReadAloudAllowancePurpose(purpose), Limit: usage.Limit, Used: usage.Used, Reserved: usage.Reserved, Remaining: usage.Remaining, PeriodEnd: usage.PeriodEnd}, nil
}
func (s *Server) StreamJournalReadAloudSynthesis(ctx context.Context, r journalhttpapi.StreamJournalReadAloudSynthesisRequestObject) (journalhttpapi.StreamJournalReadAloudSynthesisResponseObject, error) {
	id := r.Params.XTellyouwhatRequestID
	deny := func(f *apiFailure) (journalhttpapi.StreamJournalReadAloudSynthesisResponseObject, error) {
		return journalhttpapi.StreamJournalReadAloudSynthesisdefaultJSONResponse{StatusCode: f.status, Body: journalErrorResponse(f)}, nil
	}
	p, f := s.speechPrincipal(ctx, id)
	if f != nil {
		return deny(f)
	}
	subject, err := s.readAloudAccess.Subject(apiRequestContext(ctx), p.KeyID)
	if err != nil {
		return deny(speechFailure(err, id.String()))
	}
	info, err := s.readAloudSessions.Get(apiRequestContext(ctx), subject, r.SynthesisID.String())
	if err != nil {
		return deny(speechFailure(err, id.String()))
	}
	if err = s.readAloudAccess.Consent(apiRequestContext(ctx), p.KeyID, info.Purpose); err != nil {
		return deny(speechFailure(err, id.String()))
	}
	purpose := info.Purpose
	select {
	case s.readAloudStreams <- struct{}{}:
		defer func() { <-s.readAloudStreams }()
	default:
		return deny(speechFailure(readaloud.ErrSessionBusy, id.String()))
	}
	after := 0
	if r.Params.After != nil {
		after = *r.Params.After
	}
	info, frames, changed, err := s.readAloudSessions.Read(apiRequestContext(ctx), subject, r.SynthesisID.String(), after)
	if err != nil {
		return deny(speechFailure(err, id.String()))
	}
	c := strictGinContext(ctx)
	c.Header("Content-Type", "text/event-stream")
	c.Header("X-Accel-Buffering", "no")
	c.Header("Cache-Control", "no-store")
	writer := http.NewResponseController(c.Writer)
	defer writer.SetWriteDeadline(time.Time{})
	send := func(event journalhttpapi.ReadAloudEvent) error {
		data, err := json.Marshal(event)
		if err != nil {
			return err
		}
		_ = writer.SetWriteDeadline(time.Now().Add(15 * time.Second))
		if _, err = fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", event.Kind, data); err != nil {
			return err
		}
		c.Writer.Flush()
		return nil
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	if send(journalhttpapi.ReadAloudEvent{Kind: "heartbeat", SessionID: r.SynthesisID, Sequence: after}) != nil {
		return nil, nil
	}
	for {
		if apiRequestContext(ctx).Err() != nil {
			return nil, nil
		}
		if err = s.readAloudAccess.Consent(apiRequestContext(ctx), p.KeyID, info.Purpose); err != nil {
			if apiRequestContext(ctx).Err() != nil {
				return nil, nil
			}
			info.State = "failed"
			info.Problem = "authorization_unavailable"
			if errors.Is(err, readaloud.ErrConsentRequired) {
				info.State = "cancelled"
				info.Problem = "consent_revoked"
				s.readAloudSessions.CancelAccessKey(p.KeyID)
			}
			info.BufferAvailable = false
			info.AudioBytes = 0
			info.LastSequence = after
			frames = nil
		}
		for _, frame := range frames {
			data, offset := frame.PCM, frame.Offset
			if send(journalhttpapi.ReadAloudEvent{Kind: "audio", SessionID: r.SynthesisID, Sequence: frame.Sequence, Offset: &offset, Pcm: &data}) != nil {
				return nil, nil
			}
			after = frame.Sequence
		}
		if info.State != "preparing" && info.State != "reserved" && info.State != "running" && after >= info.LastSequence {
			body := speechResponse(info)
			_ = send(journalhttpapi.ReadAloudEvent{Kind: "state", SessionID: r.SynthesisID, Sequence: after, Session: &body})
			return nil, nil
		}
		if after >= info.LastSequence {
			select {
			case <-apiRequestContext(ctx).Done():
				return nil, nil
			case <-changed:
			case <-ticker.C:
				if send(journalhttpapi.ReadAloudEvent{Kind: "heartbeat", SessionID: r.SynthesisID, Sequence: after}) != nil {
					return nil, nil
				}
			}
		}
		info, frames, changed, err = s.readAloudSessions.Read(apiRequestContext(ctx), subject, r.SynthesisID.String(), after)
		if err != nil {
			if apiRequestContext(ctx).Err() != nil {
				return nil, nil
			}
			info = readaloud.SessionInfo{ID: r.SynthesisID.String(), Purpose: purpose, State: "failed", Problem: "audio_unavailable", LastSequence: after, ExpiresAt: s.now()}
			body := speechResponse(info)
			_ = send(journalhttpapi.ReadAloudEvent{Kind: "state", SessionID: r.SynthesisID, Sequence: after, Session: &body})
			return nil, nil
		}
	}
}
