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
	s.Commit(ctx, "owner", "s", "old", "lease", Receipt{"a", "hash", "text", 1000}, 1000)
	n, _ = s.Remaining(ctx, "owner", "new", 1000)
	if n != 1000 {
		t.Fatal(n)
	}
}
func TestExpiredTranscriptDoesNotRechargeInNextMonth(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	s.Lock(ctx, "owner", "lease")
	r := Receipt{"segment", "audio-hash", "原始转写", 1000}
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
func TestRevisionAllowsManualParagraphCorrectionButRejectsStaleAndMediaChanges(t *testing.T) {
	block, inserted := uuid.NewString(), uuid.NewString()
	s := Snapshot{Revision: 4, Blocks: []Block{{block, "原文"}}, EditedBlockIDs: []string{block}}
	r := Revision{BaseRevision: 3}
	if !errors.Is(r.Validate(s), ErrConflict) {
		t.Fatal("accepted stale revision")
	}
	r.BaseRevision = 4
	r.Patches = []Patch{{ID: block, Text: "依据后续口述局部纠正"}}
	if err := r.Validate(s); err != nil {
		t.Fatal("manual editing must not permanently lock a paragraph", err)
	}
	s.MediaOnlyBlockIDs = []string{block}
	if r.Validate(s) == nil {
		t.Fatal("overwrote media-only composition")
	}
	s.MediaOnlyBlockIDs = nil
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

func TestSnapshotRejectsUnknownMediaAndManualReferences(t *testing.T) {
	for _, media := range []bool{false, true} {
		s := Snapshot{Blocks: []Block{{uuid.NewString(), "正文"}}}
		if media {
			s.MediaOnlyBlockIDs = []string{uuid.NewString()}
		} else {
			s.EditedBlockIDs = []string{uuid.NewString()}
		}
		if s.Validate() == nil {
			t.Fatal("accepted metadata referring to an absent block")
		}
	}
}

func TestSnapshotRejectsInvalidManualEditHints(t *testing.T) {
	id := uuid.NewString()
	for _, edits := range [][]ManualEdit{
		{{BlockID: uuid.NewString(), Before: "甲", After: "乙"}},
		{{BlockID: id, Before: "甲", After: "甲"}},
		{{BlockID: id, Before: strings.Repeat("字", 4097), After: "乙"}},
		{{BlockID: id, Before: "甲", After: "乙", TranscriptOffset: -1}},
		{{BlockID: id, Before: "甲", After: "乙", TranscriptOffset: 1}},
	} {
		if (Snapshot{Blocks: []Block{{id, "当前正文"}}, ManualEdits: edits}).Validate() == nil {
			t.Fatal("accepted invalid or unbounded manual-edit context")
		}
	}
}
func TestEditorialTranscriptPreservesUnicodeAndManualEditOrder(t *testing.T) {
	old, later := "旧纠正👩🏽‍🦱。", "后来再次纠正。"
	boundary := len([]rune(old))
	snapshot := Snapshot{Transcript: old + later, ManualEdits: []ManualEdit{{TranscriptOffset: boundary}, {TranscriptOffset: 0}, {TranscriptOffset: boundary}, {TranscriptOffset: len([]rune(old + later))}}}
	raw, err := json.Marshal(editorialDocument(snapshot))
	if err != nil {
		t.Fatal(err)
	}
	var document rewriteDocument
	if err = json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Transcript) != 2 || document.Transcript[0].Text != old || document.Transcript[1].Text != later || document.Transcript[1].Start != boundary {
		t.Fatalf("lost speech order: %s", raw)
	}
	if len(document.ManualEdits) != 4 || !document.ManualEdits[0].HasLaterSpeech || document.ManualEdits[3].HasLaterSpeech {
		t.Fatal("lost explicit before/after edit relationship")
	}
	if strings.Count(string(raw), old) != 1 || strings.Count(string(raw), later) != 1 {
		t.Fatal("transcript was duplicated for each manual edit")
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

func TestPendingEarlierSpeechCannotOverrideManualEdit(t *testing.T) {
	snapshot := Snapshot{Transcript: "前面说错了，是十元。", ManualEdits: []ManualEdit{{Before: "十", After: "十五", PendingEarlierSpeech: true}}}
	document := editorialDocument(snapshot)
	if document.ManualEdits[0].HasLaterSpeech || document.ManualEdits[0].TranscriptOffset != len([]rune(snapshot.Transcript)) {
		t.Fatal("late ASR became later speech")
	}
	if snapshot.ManualEdits[0].TranscriptOffset != 0 {
		t.Fatal("rewriter mutated session snapshot")
	}
	snapshot.ManualEdits[0].PendingEarlierSpeech = false
	snapshot.ManualEdits[0].TranscriptOffset = len([]rune(snapshot.Transcript))
	snapshot.Transcript += "刚才手改错了，应当是十二元。"
	if !editorialDocument(snapshot).ManualEdits[0].HasLaterSpeech {
		t.Fatal("explicit later correction was blocked")
	}
}
