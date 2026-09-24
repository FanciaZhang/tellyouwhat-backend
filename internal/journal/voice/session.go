package voice

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/tellyouwhat/backend/internal/lifecycle"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"golang.org/x/net/websocket"
)

type Identity struct {
	Owner, KeyID      string
	Anchor, ExpiresAt time.Time
}
type Ticket struct {
	SessionID             string    `json:"sessionID"`
	Token                 string    `json:"token"`
	RemainingMilliseconds int       `json:"remainingMilliseconds"`
	MaximumMilliseconds   int       `json:"maximumMilliseconds"`
	ResetsAt              time.Time `json:"resetsAt"`
}
type ticketClaim struct {
	Identity  Identity
	SessionID string
	ExpiresAt time.Time
	Nonce     string
}
type Service struct {
	ResolveLimit func(context.Context) (int, error)
	Store        Store
	Speech       Speech
	Model        Rewriter
	Secret       []byte
	Limit        int
	Logger       *slog.Logger
	// Records metadata only; never transcript, body, or vocabulary.
	Usage func(context.Context, Identity, int, int)
}

func (s *Service) limit() int {
	if s.Limit > 0 {
		return s.Limit
	}
	return MonthlyMilliseconds
}
func (s *Service) Issue(ctx context.Context, id Identity, session string) (Ticket, error) {
	if s.ResolveLimit != nil {
		limit, err := s.ResolveLimit(ctx)
		if err != nil {
			return Ticket{}, err
		}
		copy := *s
		copy.ResolveLimit = nil
		copy.Limit = limit
		return copy.Issue(ctx, id, session)
	}
	if _, err := uuid.Parse(session); err != nil || id.Owner == "" || id.Anchor.IsZero() || !id.ExpiresAt.After(time.Now()) || len(s.Secret) < 32 {
		return Ticket{}, ErrInvalid
	}
	start, end := Period(id.Anchor, time.Now())
	remaining, err := s.Store.Remaining(ctx, id.Owner, start.Format(time.RFC3339), s.limit())
	if err != nil {
		return Ticket{}, err
	}
	claim := ticketClaim{id, session, time.Now().Add(30 * time.Second), uuid.NewString()}
	raw, _ := json.Marshal(claim)
	mac := hmac.New(sha256.New, s.Secret)
	mac.Write(raw)
	token := base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return Ticket{session, token, remaining, SessionMilliseconds, end}, nil
}
func (s *Service) claim(token, session string) (ticketClaim, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return ticketClaim{}, ErrInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return ticketClaim{}, ErrInvalid
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ticketClaim{}, ErrInvalid
	}
	mac := hmac.New(sha256.New, s.Secret)
	mac.Write(raw)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return ticketClaim{}, ErrInvalid
	}
	var c ticketClaim
	if json.Unmarshal(raw, &c) != nil || c.SessionID != session || !time.Now().Before(c.ExpiresAt) {
		return ticketClaim{}, ErrInvalid
	}
	return c, nil
}
func (s *Service) Serve(w http.ResponseWriter, r *http.Request, session string) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	c, err := s.claim(token, session)
	if err != nil {
		http.Error(w, "invalid voice ticket", 401)
		return
	}
	if s.ResolveLimit != nil {
		limit, err := s.ResolveLimit(r.Context())
		if err != nil {
			http.Error(w, "voice service temporarily unavailable", 503)
			return
		}
		copy := *s
		copy.ResolveLimit = nil
		copy.Limit = limit
		copy.Serve(w, r, session)
		return
	}
	// The ticket is consumed even if upgrade fails. Request another authenticated
	// ticket to reconnect; a different replica can verify and redeem it.
	if err = s.Store.Lock(r.Context(), "ticket:"+c.Nonce, c.Nonce); err != nil {
		http.Error(w, "ticket already used", 409)
		return
	}
	fence := uuid.NewString()
	if err = s.Store.Lock(r.Context(), c.Identity.Owner, fence); err != nil {
		code := "voice_storage_unavailable"
		if errors.Is(err, ErrBusy) {
			code = "voice_session_busy"
		}
		// URLSessionWebSocketTask cannot decode a failed upgrade body. Deliver
		// an authenticated, structured refusal without starting any provider work.
		websocket.Server{Handshake: func(*websocket.Config, *http.Request) error { return nil }, Handler: func(ws *websocket.Conn) {
			defer ws.Close()
			ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
			_ = websocket.JSON.Send(ws, Event{Type: "error", Code: code})
		}}.ServeHTTP(w, r)
		return
	}
	defer s.Store.Unlock(context.WithoutCancel(r.Context()), c.Identity.Owner, fence)
	wsServer := websocket.Server{Handshake: func(config *websocket.Config, request *http.Request) error { return nil }, Handler: func(ws *websocket.Conn) { s.run(ws, c, fence) }}
	wsServer.ServeHTTP(w, r)
}

type receivedFrame struct {
	frame Frame
	err   error
}
type speechResult struct {
	segment string
	value   Transcript
	err     error
}
type rewriteResult struct {
	value      RewriteResult
	err        error
	generation int
	source     string
	attempt    int
	startedAt  time.Time
	finalizing bool
}

func (s *Service) run(ws *websocket.Conn, claim ticketClaim, fence string) {
	ctx, cancel := context.WithCancel(costcontrol.WithAccess(ws.Request().Context(), claim.Identity.KeyID, true))
	defer cancel()
	defer ws.Close()
	voiceTraceID := newVoiceTraceID()
	streamStartedAt := time.Now()
	rewriteAttempts, completedRewrites, failedRewrites := 0, 0, 0
	if s.Logger != nil {
		s.Logger.InfoContext(ctx, "journal voice stream started", "voice_trace_id", voiceTraceID)
	}
	defer func() {
		if s.Logger != nil {
			s.Logger.InfoContext(context.WithoutCancel(ctx), "journal voice stream closed",
				"voice_trace_id", voiceTraceID,
				"duration_ms", time.Since(streamStartedAt).Milliseconds(),
				"rewrite_attempts", rewriteAttempts,
				"completed_rewrites", completedRewrites,
				"failed_rewrites", failedRewrites,
			)
		}
	}()
	ws.MaxPayloadBytes = 512 << 10
	incoming := make(chan receivedFrame, 8)
	speech := make(chan speechResult, 32)
	rewrites := make(chan rewriteResult, 1)
	go func() {
		for {
			var frame Frame
			err := websocket.JSON.Receive(ws, &frame)
			select {
			case incoming <- receivedFrame{frame, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	emit := func(e Event) bool {
		ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return websocket.JSON.Send(ws, e) == nil
	}
	failWithCause := func(code, stage string, cause error) {
		if s.Logger != nil {
			s.Logger.WarnContext(ctx, "journal voice stream failure",
				"voice_trace_id", voiceTraceID,
				"stage", stage,
				"client_error_code", code,
				"error_class", errorClass(cause),
			)
		}
		emit(Event{Type: "error", Code: code})
	}
	fail := func(code string) { failWithCause(code, "protocol", nil) }
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	var asr SpeechConnection
	defer func() {
		if asr != nil {
			asr.Close()
		}
	}()
	var snapshot Snapshot
	hasSnapshot := false
	committedSegments := map[string]bool{}
	var segment string
	var pcm []byte
	var duplicate *Receipt
	var billedHash string
	var inputFinal bool
	var transcriptBase, segmentText string
	var generation, tr, lastSubmitted int
	var dirty, running, finishing, failed bool
	awaitingRevision := -1
	var awaitingSources []string
	var segmentPeriod string
	var remaining int
	rewriteTimer := time.NewTimer(time.Hour)
	rewriteTimer.Stop()
	defer rewriteTimer.Stop()
	var rewriteC <-chan time.Time
	var rewriteAt, nextRewrite time.Time
	quotaFailure := func() {
		emit(Event{Type: "error", Code: "voice_quota_exhausted", SegmentID: segment, RemainingMilliseconds: remaining})
	}
	maxEnd := minTime(time.Now().Add(31*time.Minute), claim.Identity.ExpiresAt)
	launch := func() {
		if running || awaitingRevision >= 0 || !dirty || len(snapshot.Blocks) == 0 || len(snapshot.PendingUtterances) == 0 {
			return
		}
		current := snapshot
		current.Transcript = transcriptBase + segmentText
		current.PendingUtterances = pendingRewriteBatch(current.PendingUtterances)
		if current.Validate() != nil {
			fail("voice_context_too_large")
			failed = true
			cancel()
			return
		}
		rewriteTimer.Stop()
		rewriteC = nil
		nextRewrite = time.Now().Add(2 * time.Second)
		dirty = false
		running = true
		lastSubmitted = tr
		g := generation
		targetTR := tr
		rewriteAttempts++
		attempt := rewriteAttempts
		startedAt := time.Now()
		finalizing := finishing
		if s.Logger != nil {
			bodyCharacters := 0
			for _, block := range current.Blocks {
				bodyCharacters += utf8.RuneCountInString(block.Text)
			}
			recordingMode := ""
			if current.RecordingContext != nil {
				recordingMode = safeDiagnosticToken(current.RecordingContext.Mode)
			}
			s.Logger.InfoContext(ctx, "journal voice rewrite started",
				"voice_trace_id", voiceTraceID,
				"rewrite_attempt", attempt,
				"finalizing", finalizing,
				"document_revision", current.Revision,
				"transcript_revision", targetTR,
				"generation", g,
				"block_count", len(current.Blocks),
				"body_character_count", bodyCharacters,
				"transcript_character_count", utf8.RuneCountInString(current.Transcript),
				"manual_edit_count", len(current.ManualEdits),
				"edited_block_count", len(current.EditedBlockIDs),
				"media_only_block_count", len(current.MediaOnlyBlockIDs),
				"vocabulary_count", len(current.Words),
				"has_recording_context", current.RecordingContext != nil,
				"recording_mode", recordingMode,
			)
		}
		complete := lifecycle.Track(ctx)
		go func() {
			defer complete()
			work, stop := context.WithTimeout(ctx, 840*time.Second)
			defer stop()
			work = withRewriteTrace(work, voiceTraceID, attempt)
			result, err := s.Model.Rewrite(work, current, targetTR)
			select {
			case rewrites <- rewriteResult{result, err, g, current.Transcript, attempt, startedAt, finalizing}:
			case <-ctx.Done():
			}
		}()
	}
	// Interim ASR is revisable source text, not a committed receipt. Coalesce
	// updates and keep one model call in flight; finalization bypasses pacing.
	schedule := func(immediate bool) {
		if running || awaitingRevision >= 0 || !dirty || len(snapshot.Blocks) == 0 || len(snapshot.PendingUtterances) == 0 {
			return
		}
		due := time.Now()
		if !immediate {
			due = due.Add(250 * time.Millisecond)
		}
		if nextRewrite.After(due) {
			due = nextRewrite
		}
		if rewriteC != nil && !due.Before(rewriteAt) {
			return
		}
		rewriteAt = due
		rewriteTimer.Reset(max(time.Until(due), 0))
		rewriteC = rewriteTimer.C
	}
	emit(Event{Type: "ready"})
	for {
		select {
		case <-ctx.Done():
			return
		case <-rewriteC:
			rewriteC = nil
			launch()
		case <-ticker.C:
			if time.Now().After(maxEnd) {
				fail("voice_session_expired")
				return
			}
			if err := s.Store.Renew(ctx, claim.Identity.Owner, fence); err != nil {
				failWithCause("voice_session_busy", "renew_session_lease", err)
				return
			}
			schedule(true)
		case message := <-incoming:
			if message.err != nil {
				if s.Logger != nil {
					s.Logger.InfoContext(ctx, "journal voice socket receive ended", "voice_trace_id", voiceTraceID, "error_class", errorClass(message.err))
				}
				return
			}
			f := message.frame
			switch f.Type {
			case "snapshot":
				if f.Snapshot == nil {
					failWithCause("voice_invalid_request", "missing_snapshot", ErrInvalid)
					return
				}
				if err := f.Snapshot.Validate(); err != nil {
					failWithCause("voice_invalid_request", "validate_snapshot", err)
					return
				}
				next := *f.Snapshot
				hadSnapshot := hasSnapshot
				// A document ACK may have been sent before the latest receipt
				// reached the client. It cannot roll back server-confirmed speech.
				// Only the initial snapshot seeds prior-session transcript text.
				if hasSnapshot {
					next.Transcript = transcriptBase
				}
				hasSnapshot = true
				// Local typing can produce the expected ACK revision without
				// applying our patch. Version equality alone is not an ACK.
				wasAcknowledgement := acknowledgesIncrementalSources(next, awaitingRevision, awaitingSources)
				if wasAcknowledgement {
					awaitingRevision = -1
					next.PendingUtterances = mergePendingUtterances(
						removePendingUtterances(snapshot.PendingUtterances, awaitingSources),
						removePendingUtterances(next.PendingUtterances, awaitingSources),
					)
					awaitingSources = nil
				} else if hadSnapshot {
					// A snapshot sent before the latest receipt cannot erase source
					// utterances that the server has already committed.
					next.PendingUtterances = mergePendingUtterances(snapshot.PendingUtterances, next.PendingUtterances)
					if awaitingRevision >= 0 && next.Revision >= awaitingRevision {
						awaitingRevision = -1
						awaitingSources = nil
						dirty = true
					}
				}
				if len(next.PendingUtterances) > 0 && (!hadSnapshot || !slices.Equal(snapshot.PendingUtterances, next.PendingUtterances)) {
					dirty = true
				}
				// Repeated receipt acknowledgements do not invalidate a model
				// call that already uses the same base. Real edits still do.
				if snapshot.Revision != next.Revision || snapshot.Transcript != next.Transcript ||
					!slices.Equal(snapshot.Blocks, next.Blocks) || !slices.Equal(snapshot.EditedBlockIDs, next.EditedBlockIDs) ||
					!slices.Equal(snapshot.MediaOnlyBlockIDs, next.MediaOnlyBlockIDs) || !slices.Equal(snapshot.PendingUtterances, next.PendingUtterances) ||
					!slices.Equal(snapshot.Words, next.Words) || snapshot.WritingStyle != next.WritingStyle {
					generation++
				}
				snapshot = next
				transcriptBase = snapshot.Transcript
				if finishing && segment == "" && !running {
					launch()
					if !failed && !running && awaitingRevision < 0 {
						_ = s.Store.Forget(ctx, claim.Identity.Owner, claim.SessionID)
						emit(Event{Type: "finished"})
						return
					}
				}
				if !finishing {
					schedule(true)
				}
			case "audio":
				if finishing || f.SegmentID == "" || len(f.PCM) > 6400 || len(f.PCM)%2 != 0 {
					fail("voice_invalid_request")
					return
				}
				if segment == "" {
					if _, err := uuid.Parse(f.SegmentID); err != nil {
						fail("voice_invalid_request")
						return
					}
					segment = f.SegmentID
					inputFinal = false
					pcm = nil
					segmentText = ""
					var err error
					duplicate, err = s.Store.Receipt(ctx, claim.Identity.Owner, claim.SessionID, segment)
					if err != nil {
						failWithCause("voice_storage_unavailable", "read_segment_receipt", err)
						return
					}
					billedHash, err = s.Store.BilledHash(ctx, claim.Identity.Owner, claim.SessionID, segment)
					if err != nil {
						failWithCause("voice_storage_unavailable", "read_billed_segment", err)
						return
					}
					start, _ := Period(claim.Identity.Anchor, time.Now())
					segmentPeriod = start.Format(time.RFC3339)
					remaining, err = s.Store.Remaining(ctx, claim.Identity.Owner, segmentPeriod, s.limit())
					if err != nil {
						failWithCause("voice_storage_unavailable", "read_voice_allowance", err)
						return
					}
					if duplicate == nil {
						if remaining <= 0 && billedHash == "" {
							quotaFailure()
							return
						}
						asr, err = s.Speech.Open(ctx, snapshot.Words)
						if err != nil {
							failWithCause("voice_speech_unavailable", "open_speech_provider", err)
							return
						}
						conn, id := asr, segment
						go func() {
							for {
								result, err := conn.Receive()
								select {
								case speech <- speechResult{id, result, err}:
								case <-ctx.Done():
									return
								}
								if err != nil || result.Final {
									return
								}
							}
						}()
					}
				}
				if segment != f.SegmentID || inputFinal || len(pcm)+len(f.PCM) > MaxSegmentBytes {
					fail("voice_invalid_request")
					return
				}
				pcm = append(pcm, f.PCM...)
				if duplicate == nil && billedHash == "" && (len(pcm)+31)/32 > remaining {
					quotaFailure()
					return
				}
				inputFinal = f.Final
				if f.Final && billedHash != "" && billedHash != hash(string(pcm)) {
					fail("voice_revision_conflict")
					return
				}
				if duplicate != nil {
					if f.Final {
						if duplicate.SHA256 != hash(string(pcm)) {
							fail("voice_revision_conflict")
							return
						}
						if len(duplicate.Utterances) == 0 && duplicate.Text != "" {
							duplicate.Utterances = identifiedUtterances(duplicate.SegmentID, duplicate.Text, nil, duplicate.Milliseconds)
						}
						if !committedSegments[segment] {
							committedSegments[segment] = true
							transcriptBase += duplicate.Text
							snapshot.Transcript = transcriptBase
							snapshot.PendingUtterances = mergePendingUtterances(snapshot.PendingUtterances, sourceUtterances(duplicate.SegmentID, duplicate.Utterances))
							tr++
							dirty = len(snapshot.PendingUtterances) > 0
						}
						emit(Event{Type: "receipt", Receipt: duplicate, RemainingMilliseconds: remaining})
						segment = ""
						pcm = nil
						duplicate = nil
					}
				} else if err := asr.Send(f.PCM, f.Final); err != nil {
					failWithCause("voice_speech_unavailable", "send_speech_audio", err)
					return
				}
			case "finish":
				finishing = true
				if segment == "" {
					launch()
					if !failed && !running && awaitingRevision < 0 {
						_ = s.Store.Forget(ctx, claim.Identity.Owner, claim.SessionID)
						emit(Event{Type: "finished"})
						return
					}
				}
			case "ping":
				emit(Event{Type: "pong"})
			default:
				fail("voice_invalid_request")
				return
			}
		case result := <-speech:
			if result.segment != segment {
				continue
			}
			if result.err != nil {
				failWithCause("voice_speech_unavailable", "receive_speech_result", result.err)
				return
			}
			v := result.value
			wireUtterances := identifiedUtterances(segment, v.Text, incrementalUtterances(v.Utterances, (len(pcm)+31)/32), (len(pcm)+31)/32)
			emit(Event{Type: "transcript", SegmentID: segment, Text: v.Text, Stable: v.Stable, Utterances: wireUtterances})
			if v.Text != segmentText {
				segmentText = v.Text
				tr++
			}
			if v.Final {
				if !inputFinal || len(pcm) == 0 {
					fail("voice_invalid_request")
					return
				}
				receipt := Receipt{SegmentID: segment, SHA256: hash(string(pcm)), Text: v.Text,
					Milliseconds: (len(pcm) + 31) / 32, Utterances: wireUtterances}
				var err error
				remaining, err = s.Store.Commit(ctx, claim.Identity.Owner, claim.SessionID, segmentPeriod, fence, receipt, s.limit())
				if err != nil {
					if errors.Is(err, ErrQuota) {
						fail("voice_quota_exhausted")
					} else {
						failWithCause("voice_storage_unavailable", "commit_speech_receipt", err)
					}
					return
				}
				committedSegments[segment] = true
				transcriptBase += v.Text
				segmentText = ""
				snapshot.Transcript = transcriptBase
				snapshot.PendingUtterances = mergePendingUtterances(snapshot.PendingUtterances, sourceUtterances(segment, wireUtterances))
				dirty = len(snapshot.PendingUtterances) > 0
				emit(Event{Type: "receipt", Receipt: &receipt, RemainingMilliseconds: remaining})
				asr.Close()
				asr = nil
				segment = ""
				pcm = nil
				if finishing {
					dirty = dirty || (lastSubmitted != tr && len(snapshot.PendingUtterances) > 0)
					launch()
					if !failed && !running && awaitingRevision < 0 {
						_ = s.Store.Forget(ctx, claim.Identity.Owner, claim.SessionID)
						emit(Event{Type: "finished"})
						return
					}
				}
			}
			if !finishing && v.Final {
				// The client can attach a live person assignment in the receipt ACK
				// before this short coalescing window closes.
				schedule(false)
			}
		case result := <-rewrites:
			running = false
			diagnostics := rewriteDiagnostics(result.value, result.err)
			if result.err != nil {
				failedRewrites++
			} else {
				completedRewrites++
			}
			if s.Logger != nil {
				log := s.Logger.InfoContext
				if result.err != nil {
					log = s.Logger.WarnContext
				}
				log(ctx, "journal voice rewrite completed",
					"voice_trace_id", voiceTraceID,
					"rewrite_attempt", result.attempt,
					"finalizing", result.finalizing,
					"outcome", map[bool]string{true: "failed", false: "completed"}[result.err != nil],
					"duration_ms", time.Since(result.startedAt).Milliseconds(),
					"stage", diagnostics.Stage,
					"http_status", diagnostics.HTTPStatus,
					"provider_request_id", diagnostics.ProviderRequestID,
					"provider_error_code", diagnostics.ProviderErrorCode,
					"provider_status", diagnostics.ProviderStatus,
					"input_token_count", result.value.InputTokens,
					"output_token_count", result.value.OutputTokens,
					"patch_count", len(result.value.Revision.Patches),
					"question_count", len(result.value.Revision.Questions),
					"emotion_count", len(result.value.Revision.Emotions),
					"has_overall_emotion", result.value.Revision.OverallEmotion != "",
					"error_class", errorClass(result.err),
				)
			}
			if result.err != nil {
				failWithCause("voice_rewrite_unavailable", "rewrite_result", result.err)
				dirty = true
				if finishing {
					return
				}
			} else {
				if s.Usage != nil {
					s.Usage(ctx, claim.Identity, result.value.InputTokens, result.value.OutputTokens)
				}
				if result.generation == generation && result.value.Revision.BaseRevision == snapshot.Revision {
					awaitingRevision = result.value.Revision.BaseRevision + 1
					awaitingSources = append([]string(nil), result.value.Revision.ConsumedSourceIDs...)
					emit(Event{Type: "revision", Revision: &result.value.Revision})
					// Do not start another round until the client acknowledges the new base
					// with a snapshot. This avoids repeatedly proposing the same insertion.
				} else {
					// An intervening receipt/snapshot can invalidate an in-flight
					// result without changing the text. Finalization still needs
					// a revision that the client has actually applied.
					dirty = true
				}
			}
			if !finishing {
				schedule(true)
			}
			if finishing && segment == "" && awaitingRevision < 0 {
				if dirty {
					launch()
				} else {
					_ = s.Store.Forget(ctx, claim.Identity.Owner, claim.SessionID)
					emit(Event{Type: "finished"})
					return
				}
			}
		}
	}
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func acknowledgesIncrementalSources(next Snapshot, revision int, sources []string) bool {
	if revision < 0 || next.Revision < revision || len(sources) == 0 {
		return false
	}
	for _, source := range sources {
		if !slices.Contains(next.KnownSourceIDs, source) {
			return false
		}
		for _, pending := range next.PendingUtterances {
			if pending.ID == source {
				return false
			}
		}
	}
	return true
}

func incrementalUtterances(provider []StreamUtterance, milliseconds int) []Utterance {
	result := make([]Utterance, 0, len(provider))
	for _, u := range boundedStreamUtterances(provider, milliseconds) {
		missing := u.ProviderStartUnavailable
		value := Utterance{Text: u.Text, StartMilliseconds: u.StartMilliseconds, EndMilliseconds: u.EndMilliseconds,
			ProviderEndMilliseconds: u.ProviderEndMilliseconds, ProviderStartUnavailable: &missing,
			Definite: u.Definite, Speaker: u.Speaker, AcousticEmotion: u.AcousticEmotion}
		if u.Volume != nil {
			value.Volume = *u.Volume
		}
		if u.SpeechRate != nil {
			value.SpeechRate = *u.SpeechRate
		}
		for _, w := range u.Words {
			value.Words = append(value.Words, Word{Text: w.Text, StartMilliseconds: w.StartMilliseconds, EndMilliseconds: w.EndMilliseconds})
		}
		result = append(result, value)
	}
	return result
}

func identifiedUtterances(segment, text string, provider []Utterance, milliseconds int) []Utterance {
	joined := ""
	for _, utterance := range provider {
		joined += utterance.Text
	}
	pieces := provider
	if text != joined {
		pieces = nil
		if joined != "" && strings.HasSuffix(text, joined) && len(provider) > 0 && provider[0].StartMilliseconds > 0 {
			prefixEnd := min(milliseconds, provider[0].StartMilliseconds)
			pieces = append(pieces, Utterance{Text: strings.TrimSuffix(text, joined), StartMilliseconds: 0, EndMilliseconds: prefixEnd, Definite: true})
			pieces = append(pieces, provider...)
		} else if text != "" {
			pieces = []Utterance{{Text: text, StartMilliseconds: 0, EndMilliseconds: max(0, milliseconds), Definite: true}}
		}
	}
	result := make([]Utterance, 0, len(pieces))
	for index, utterance := range pieces {
		if strings.TrimSpace(utterance.Text) == "" {
			continue
		}
		utterance.StartMilliseconds = min(max(0, utterance.StartMilliseconds), max(0, milliseconds))
		utterance.EndMilliseconds = min(max(utterance.StartMilliseconds, utterance.EndMilliseconds), max(0, milliseconds))
		digest := sha256.Sum256([]byte(segment + "/" + fmt.Sprint(index) + "/" + utterance.Text))
		id, _ := uuid.FromBytes(digest[:16])
		utterance.ID = id.String()
		result = append(result, utterance)
	}
	return result
}

func sourceUtterances(segment string, utterances []Utterance) []SourceUtterance {
	result := make([]SourceUtterance, 0, len(utterances))
	for _, utterance := range utterances {
		speaker := ""
		if utterance.Speaker != "" {
			speaker = segment + ":" + utterance.Speaker
		}
		result = append(result, SourceUtterance{
			ID: utterance.ID, Text: utterance.Text, Speaker: speaker,
			StartMilliseconds: utterance.StartMilliseconds,
			EndMilliseconds:   utterance.EndMilliseconds,
			AcousticEmotion:   utterance.AcousticEmotion,
			Volume:            utterance.Volume,
			SpeechRate:        utterance.SpeechRate,
		})
	}
	return result
}

func mergePendingUtterances(base, updates []SourceUtterance) []SourceUtterance {
	result := append([]SourceUtterance(nil), base...)
	indices := map[string]int{}
	for index, utterance := range result {
		indices[utterance.ID] = index
	}
	for _, utterance := range updates {
		if index, exists := indices[utterance.ID]; exists {
			result[index] = utterance
		} else {
			indices[utterance.ID] = len(result)
			result = append(result, utterance)
		}
	}
	return result
}

func removePendingUtterances(source []SourceUtterance, removed []string) []SourceUtterance {
	ids := map[string]bool{}
	for _, id := range removed {
		ids[id] = true
	}
	result := make([]SourceUtterance, 0, len(source))
	for _, utterance := range source {
		if !ids[utterance.ID] {
			result = append(result, utterance)
		}
	}
	return result
}

func pendingRewriteBatch(source []SourceUtterance) []SourceUtterance {
	result := make([]SourceUtterance, 0, len(source))
	characters := 0
	for _, utterance := range source {
		length := len([]rune(utterance.Text))
		if len(result) > 0 && characters+length > MaxRewriteSourceCharacters {
			break
		}
		result = append(result, utterance)
		characters += length
	}
	return result
}
