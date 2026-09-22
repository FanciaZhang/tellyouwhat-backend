package voice

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestAnchoredMonthDoesNotDriftAfterFebruary(t *testing.T) {
	anchor := time.Date(2024, 1, 31, 12, 0, 0, 0, time.UTC)
	tests := []struct{ now, start, end string }{{"2024-02-29T11:59:00Z", "2024-01-31T12:00:00Z", "2024-02-29T12:00:00Z"}, {"2024-02-29T12:00:00Z", "2024-02-29T12:00:00Z", "2024-03-31T12:00:00Z"}, {"2025-03-30T12:00:00Z", "2025-02-28T12:00:00Z", "2025-03-31T12:00:00Z"}}
	for _, tt := range tests {
		now, _ := time.Parse(time.RFC3339, tt.now)
		start, end := Period(anchor, now)
		if start.Format(time.RFC3339) != tt.start || end.Format(time.RFC3339) != tt.end {
			t.Fatalf("%s: %v %v", tt.now, start, end)
		}
	}
}
func TestReceiptsAreIdempotentAndSubscriptionScoped(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	if err := s.Lock(ctx, "subscription", "lease"); err != nil {
		t.Fatal(err)
	}
	if err := s.Lock(ctx, "subscription", "other-device"); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	receipt := Receipt{SegmentID: uuid.NewString(), SHA256: "a", Text: "你好", Milliseconds: 1000}
	for range 2 {
		remaining, err := s.Commit(ctx, "subscription", "session", "month", "lease", receipt, 1200)
		if err != nil || remaining != 200 {
			t.Fatalf("%d %v", remaining, err)
		}
	}
	receipt.SHA256 = "b"
	if _, err := s.Commit(ctx, "subscription", "session", "month", "lease", receipt, 1200); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	receipt.SegmentID = uuid.NewString()
	if _, err := s.Commit(ctx, "subscription", "session", "month", "lease", receipt, 1200); !errors.Is(err, ErrQuota) {
		t.Fatal(err)
	}
	if r, _ := s.Receipt(ctx, "other-subscription", "session", receipt.SegmentID); r != nil {
		t.Fatal("cross-subscription receipt")
	}
	s.Unlock(ctx, "subscription", "lease")
	if _, err := s.Commit(ctx, "subscription", "session", "month", "lease", receipt, 1200); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
}
func TestFailedSegmentDoesNotChargeAndQuotaResetsByPeriod(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	s.Lock(ctx, "owner", "lease")
	// Provider failure produces no Commit; another attempt retains the allowance.
	n, _ := s.Remaining(ctx, "owner", "old", 1000)
	if n != 1000 {
		t.Fatal(n)
	}
	s.Commit(ctx, "owner", "s", "old", "lease", Receipt{SegmentID: "a", SHA256: "hash", Text: "text", Milliseconds: 1000}, 1000)
	n, _ = s.Remaining(ctx, "owner", "new", 1000)
	if n != 1000 {
		t.Fatal(n)
	}
}
func TestExpiredTranscriptDoesNotRechargeInNextMonth(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	s.Lock(ctx, "owner", "lease")
	r := Receipt{SegmentID: "segment", SHA256: "audio-hash", Text: "原始转写", Milliseconds: 1000}
	if _, err := s.Commit(ctx, "owner", "session", "old", "lease", r, 1200); err != nil {
		t.Fatal(err)
	}
	key := "owner" + "session" + "segment"
	s.receipts[key] = memoryReceipt{r, time.Now().Add(-time.Hour)}
	if cached, _ := s.Receipt(ctx, "owner", "session", "segment"); cached != nil {
		t.Fatal("expired content retained")
	}
	remaining, err := s.Commit(ctx, "owner", "session", "new", "lease", r, 1200)
	if err != nil || remaining != 1200 || s.sessionUsed["ownersession"] != 1000 {
		t.Fatalf("recharged retry: %d %v", remaining, err)
	}
	r.SHA256 = "different-audio"
	if _, err = s.Commit(ctx, "owner", "session", "new", "lease", r, 1200); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
}
func TestASRPacketFinalFlagAndMalformedPackets(t *testing.T) {
	p := asrPacket(2, true, []byte{1, 2})
	if p[1] != 0x22 || binary.BigEndian.Uint32(p[4:8]) != 2 {
		t.Fatal(p)
	}
	payload, _ := json.Marshal(map[string]any{"result": map[string]any{"text": "许知远", "utterances": []map[string]any{{"text": "许知远", "definite": true}}}})
	packet := asrPacket(9, true, payload)
	packet[2] = 0x10
	result, err := parseASR(packet)
	if err != nil || result.Stable != "许知远" || !result.Final {
		t.Fatalf("%+v %v", result, err)
	}
	for _, bad := range [][]byte{{}, packet[:3], packet[:len(packet)-1], {0x11, 0xf0, 0x10, 0, 0, 0, 0, 0}} {
		if _, err := parseASR(bad); err == nil {
			t.Fatal("accepted malformed packet")
		}
	}
}

func TestASRPreservesTimedSpeakerEvidence(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{"result": map[string]any{
		"text": "你好。",
		"utterances": []map[string]any{{
			"text": "你好。", "definite": true, "start_time": 120, "end_time": 860,
			"additions": map[string]any{"speaker": "2", "emotion": "happy", "volume": 7.5, "speech_rate": "3.25"},
			"words":     []map[string]any{{"text": "你好", "start_time": 120, "end_time": 700}},
		}},
	}})
	packet := asrPacket(9, true, payload)
	packet[2] = 0x10
	result, err := parseASR(packet)
	if err != nil || len(result.Utterances) != 1 {
		t.Fatalf("%+v %v", result, err)
	}
	utterance := result.Utterances[0]
	if utterance.StartMilliseconds != 120 || utterance.EndMilliseconds != 860 || utterance.Speaker != "2" ||
		utterance.AcousticEmotion != "happy" || utterance.Volume != 7.5 || utterance.SpeechRate != 3.25 || len(utterance.Words) != 1 {
		t.Fatalf("provider evidence was dropped: %+v", utterance)
	}
}

func TestRewriteInputIsIncrementalAndBounded(t *testing.T) {
	old := uuid.NewString()
	active := uuid.NewString()
	source := uuid.NewString()
	snapshot := Snapshot{
		Revision:          9,
		Blocks:            []Block{{ID: old, Text: "HEAD_SENTINEL" + strings.Repeat("旧正文", 3000)}, {ID: active, Text: "最后一段"}},
		Transcript:        strings.Repeat("FULL_TRANSCRIPT_SENTINEL", 500),
		PendingUtterances: []SourceUtterance{{ID: source, Text: "这是本轮新口述。", Speaker: "segment:1", Person: "小林"}},
	}
	raw, err := rewriteModelInput(snapshot, 12)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if strings.Contains(text, "FULL_TRANSCRIPT_SENTINEL") || strings.Contains(text, "HEAD_SENTINEL") ||
		!strings.Contains(text, "这是本轮新口述") || !strings.Contains(text, "小林") || len(raw) > 20_000 {
		t.Fatalf("rewrite input is not bounded incremental context: bytes=%d body=%s", len(raw), text)
	}
}
func TestRevisionRejectsUnknownBlocksAndManualEdits(t *testing.T) {
	block, inserted := uuid.NewString(), uuid.NewString()
	s := Snapshot{Revision: 4, Blocks: []Block{{block, "原文"}}, EditedBlockIDs: []string{block}}
	r := Revision{BaseRevision: 3}
	if !errors.Is(r.Validate(s), ErrConflict) {
		t.Fatal("accepted stale revision")
	}
	r.BaseRevision = 4
	r.Patches = []Patch{{ID: block, Text: "错误覆盖"}}
	if r.Validate(s) == nil {
		t.Fatal("overwrote manual edit")
	}
	r.Patches = []Patch{{ID: inserted, Text: "新增", AfterID: uuid.NewString()}}
	if r.Validate(s) == nil {
		t.Fatal("unknown anchor")
	}
	r.Patches = []Patch{{ID: "not-a-uuid", Text: "新增", AfterID: block}}
	if r.Validate(s) == nil {
		t.Fatal("accepted an ID the iOS client cannot apply")
	}
	r.Patches = []Patch{{ID: inserted, Text: "新增", AfterID: block}}
	if err := r.Validate(s); err != nil {
		t.Fatal(err)
	}
}
func TestTicketsCannotChangeSessionOrOwner(t *testing.T) {
	s := Service{Store: NewMemoryStore(), Secret: make([]byte, 32)}
	id := uuid.NewString()
	ticket, err := s.Issue(context.Background(), Identity{Owner: "paid", Anchor: time.Now().AddDate(0, -1, 0), ExpiresAt: time.Now().Add(time.Hour)}, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.claim(ticket.Token, id); err != nil {
		t.Fatal(err)
	}
	if _, err = s.claim(ticket.Token, uuid.NewString()); err == nil {
		t.Fatal("cross-session ticket")
	}
	if _, err = s.claim(ticket.Token+"tampered", id); err == nil {
		t.Fatal("invalid signature")
	}
}
