package voice

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
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
	Store  Store
	Speech Speech
	Model  Rewriter
	Secret []byte
	Limit  int
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
	// The ticket is consumed even if upgrade fails. Request another authenticated
	// ticket to reconnect; a different replica can verify and redeem it.
	if err = s.Store.Lock(r.Context(), "ticket:"+c.Nonce, c.Nonce); err != nil {
		http.Error(w, "ticket already used", 409)
		return
	}
	fence := uuid.NewString()
	if err = s.Store.Lock(r.Context(), c.Identity.Owner, fence); err != nil {
		http.Error(w, "voice session busy", 409)
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
}

func (s *Service) run(ws *websocket.Conn, claim ticketClaim, fence string) {
	ctx, cancel := context.WithCancel(ws.Request().Context())
	defer cancel()
	defer ws.Close()
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
	fail := func(code string) { emit(Event{Type: "error", Code: code}) }
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	var asr SpeechConnection
	defer func() {
		if asr != nil {
			asr.Close()
		}
	}()
	var snapshot Snapshot
	var segment string
	var pcm []byte
	var duplicate *Receipt
	var inputFinal bool
	var transcriptBase, segmentStable string
	var generation, tr, lastSubmitted int
	var dirty, running, finishing, failed bool
	awaitingRevision := -1
	var segmentPeriod string
	var remaining int
	maxEnd := minTime(time.Now().Add(31*time.Minute), claim.Identity.ExpiresAt)
	launch := func() {
		if running || awaitingRevision >= 0 || !dirty || len(snapshot.Blocks) == 0 {
			return
		}
		current := snapshot
		current.Transcript = transcriptBase + segmentStable
		if current.Validate() != nil {
			fail("voice_context_too_large")
			failed = true
			cancel()
			return
		}
		dirty = false
		running = true
		lastSubmitted = tr
		g := generation
		targetTR := tr
		go func() {
			work, stop := context.WithTimeout(ctx, 60*time.Second)
			defer stop()
			result, err := s.Model.Rewrite(work, current, targetTR)
			select {
			case rewrites <- rewriteResult{result, err, g}:
			case <-ctx.Done():
			}
		}()
	}
	emit(Event{Type: "ready"})
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if time.Now().After(maxEnd) {
				fail("voice_session_expired")
				return
			}
			if s.Store.Renew(ctx, claim.Identity.Owner, fence) != nil {
				fail("voice_session_busy")
				return
			}
			launch()
		case message := <-incoming:
			if message.err != nil {
				return
			}
			f := message.frame
			switch f.Type {
			case "snapshot":
				if f.Snapshot == nil || f.Snapshot.Validate() != nil {
					fail("voice_invalid_request")
					return
				}
				wasAcknowledgement := f.Snapshot.Revision == awaitingRevision
				if wasAcknowledgement {
					awaitingRevision = -1
				}
				if f.Snapshot.Transcript != transcriptBase || (!wasAcknowledgement && f.Snapshot.Revision != snapshot.Revision) {
					dirty = true
				}
				if len(snapshot.Blocks) == 0 && f.Snapshot.Transcript != "" {
					dirty = true
				}
				snapshot = *f.Snapshot
				transcriptBase = snapshot.Transcript
				generation++
				if finishing && segment == "" && !running {
					launch()
					if !failed && !running && awaitingRevision < 0 {
						_ = s.Store.Forget(ctx, claim.Identity.Owner, claim.SessionID)
						emit(Event{Type: "finished"})
						return
					}
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
					segmentStable = ""
					var err error
					duplicate, err = s.Store.Receipt(ctx, claim.Identity.Owner, claim.SessionID, segment)
					if err != nil {
						fail("voice_storage_unavailable")
						return
					}
					start, _ := Period(claim.Identity.Anchor, time.Now())
					segmentPeriod = start.Format(time.RFC3339)
					remaining, err = s.Store.Remaining(ctx, claim.Identity.Owner, segmentPeriod, s.limit())
					if err != nil {
						fail("voice_storage_unavailable")
						return
					}
					if duplicate == nil {
						if remaining <= 0 {
							fail("voice_quota_exhausted")
							return
						}
						asr, err = s.Speech.Open(ctx, snapshot.Words)
						if err != nil {
							fail("voice_speech_unavailable")
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
				if duplicate == nil && (len(pcm)+31)/32 > remaining {
					fail("voice_quota_exhausted")
					return
				}
				inputFinal = f.Final
				if duplicate != nil {
					if f.Final {
						if duplicate.SHA256 != hash(string(pcm)) {
							fail("voice_revision_conflict")
							return
						}
						emit(Event{Type: "receipt", Receipt: duplicate, RemainingMilliseconds: remaining})
						segment = ""
						pcm = nil
						duplicate = nil
					}
				} else if err := asr.Send(f.PCM, f.Final); err != nil {
					fail("voice_speech_unavailable")
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
				fail("voice_speech_unavailable")
				return
			}
			v := result.value
			emit(Event{Type: "transcript", SegmentID: segment, Text: v.Text, Stable: v.Stable})
			if v.Stable != segmentStable {
				segmentStable = v.Stable
				tr++
				dirty = true
			}
			if v.Final {
				if !inputFinal || len(pcm) == 0 {
					fail("voice_invalid_request")
					return
				}
				receipt := Receipt{segment, hash(string(pcm)), v.Text, (len(pcm) + 31) / 32}
				var err error
				remaining, err = s.Store.Commit(ctx, claim.Identity.Owner, claim.SessionID, segmentPeriod, fence, receipt, s.limit())
				if err != nil {
					if errors.Is(err, ErrQuota) {
						fail("voice_quota_exhausted")
					} else {
						fail("voice_storage_unavailable")
					}
					return
				}
				transcriptBase += v.Text
				segmentStable = ""
				snapshot.Transcript = transcriptBase
				emit(Event{Type: "receipt", Receipt: &receipt, RemainingMilliseconds: remaining})
				asr.Close()
				asr = nil
				segment = ""
				pcm = nil
				if finishing {
					dirty = dirty || lastSubmitted != tr
					launch()
					if !failed && !running && awaitingRevision < 0 {
						_ = s.Store.Forget(ctx, claim.Identity.Owner, claim.SessionID)
						emit(Event{Type: "finished"})
						return
					}
				}
			}
		case result := <-rewrites:
			running = false
			if result.err != nil {
				fail("voice_rewrite_unavailable")
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
