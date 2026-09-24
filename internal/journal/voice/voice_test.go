package voice

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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
		utterance.AcousticEmotion != "happy" || utterance.Volume == nil || *utterance.Volume != 7.5 || utterance.SpeechRate == nil || *utterance.SpeechRate != 3.25 || len(utterance.Words) != 1 {
		t.Fatalf("provider evidence was dropped: %+v", utterance)
	}
}

func TestRewriteInputIsIncrementalAndBounded(t *testing.T) {
	old := uuid.NewString()
	active := uuid.NewString()
	source := uuid.NewString()
	snapshot := Snapshot{
		Revision:   9,
		Blocks:     []Block{{ID: old, Text: "HEAD_SENTINEL" + strings.Repeat("旧正文", 3000)}, {ID: active, Text: "最后一段"}},
		Transcript: strings.Repeat("FULL_TRANSCRIPT_SENTINEL", 500),
		PendingUtterances: []SourceUtterance{{ID: source, Text: "这是本轮新口述。", Speaker: "segment:1", Person: "小林",
			StartMilliseconds: 120, EndMilliseconds: 860, AcousticEmotion: "happy", Volume: 7.5, SpeechRate: 3.25}},
	}
	raw, err := rewriteModelInput(snapshot, 12)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if strings.Contains(text, "FULL_TRANSCRIPT_SENTINEL") || strings.Contains(text, "HEAD_SENTINEL") ||
		!strings.Contains(text, "这是本轮新口述") || !strings.Contains(text, "小林") ||
		!strings.Contains(text, `"acousticEmotion":"happy"`) || !strings.Contains(text, `"startMilliseconds":120`) || len(raw) > 20_000 {
		t.Fatalf("rewrite input is not bounded incremental context: bytes=%d body=%s", len(raw), text)
	}
}

func TestRevisionAcceptsOnlyAcousticallyGroundedIncrementalEmotion(t *testing.T) {
	block := uuid.NewString()
	source := uuid.NewString()
	snapshot := Snapshot{Revision: 2, Blocks: []Block{{ID: block, Text: "原文"}}, PendingUtterances: []SourceUtterance{{
		ID: source, Text: "走到桥边时，我有一点害怕。", StartMilliseconds: 0, EndMilliseconds: 2_000, AcousticEmotion: "fearful",
	}}}
	revision := Revision{
		BaseRevision: 2, TranscriptRevision: 3,
		BlockEdits:        []BlockEdit{{Kind: "replace", ID: block, Text: "走到桥边时，我有一点害怕。", Style: "body"}},
		Passages:          []Passage{{BlockID: block, SourceIDs: []string{source}}},
		ConsumedSourceIDs: []string{source}, Questions: []string{},
		Emotions:       []Emotion{{BlockID: block, AnchorText: "有一点害怕", SourceID: source, Kind: "nervous"}},
		OverallEmotion: "worried",
	}
	if err := revision.Validate(snapshot); err != nil {
		t.Fatal(err)
	}
	withoutEvidence := snapshot
	withoutEvidence.PendingUtterances[0].AcousticEmotion = ""
	if revision.Validate(withoutEvidence) == nil {
		t.Fatal("accepted an emotion inferred without acoustic evidence")
	}
	duplicateAnchor := revision
	duplicateAnchor.BlockEdits[0].Text = "害怕，仍然害怕。"
	duplicateAnchor.Emotions[0].AnchorText = "害怕"
	if duplicateAnchor.Validate(snapshot) == nil {
		t.Fatal("accepted an ambiguous emotion anchor")
	}
}
func TestRevisionRejectsUnknownBlocksAndManualEdits(t *testing.T) {
	block, inserted := uuid.NewString(), uuid.NewString()
	s := Snapshot{Revision: 4, Blocks: []Block{{block, "原文", ""}}, EditedBlockIDs: []string{block}}
	r := Revision{BaseRevision: 3}
	if !errors.Is(r.Validate(s), ErrConflict) {
		t.Fatal("accepted stale revision")
	}
	r.BaseRevision = 4
	r.BlockEdits = []BlockEdit{{Kind: "replace", ID: block, Text: "错误覆盖", Style: "body"}}
	if r.Validate(s) == nil {
		t.Fatal("overwrote manual edit")
	}
	r.BlockEdits = []BlockEdit{{Kind: "insert", ID: inserted, Text: "新增", AfterID: uuid.NewString(), Style: "body"}}
	if r.Validate(s) == nil {
		t.Fatal("unknown anchor")
	}
	r.BlockEdits = []BlockEdit{{Kind: "insert", ID: "not-a-uuid", Text: "新增", AfterID: block, Style: "body"}}
	if r.Validate(s) == nil {
		t.Fatal("accepted an ID the iOS client cannot apply")
	}
	source := uuid.NewString()
	s.PendingUtterances = []SourceUtterance{{ID: source, Text: "新增"}}
	r.BlockEdits = []BlockEdit{{Kind: "insert", ID: inserted, Text: "新增", AfterID: block, Style: "body"}}
	r.Passages = []Passage{{BlockID: inserted, SourceIDs: []string{source}}}
	r.ConsumedSourceIDs = []string{source}
	if err := r.Validate(s); err != nil {
		t.Fatal(err)
	}
}

func TestRevisionUsesLaterEvidenceForTargetedPronounCorrection(t *testing.T) {
	oldSource, newSource := uuid.NewString(), uuid.NewString()
	oldBlock, activeBlock := uuid.NewString(), uuid.NewString()
	entityID, mentionID := uuid.NewString(), uuid.NewString()
	snapshot := Snapshot{
		Revision:       3,
		Blocks:         []Block{{ID: oldBlock, Text: "今天在公园遇见他了。", Style: "body"}, {ID: activeBlock, Text: "后来才想起来。", Style: "body"}},
		ActiveBlockIDs: []string{activeBlock}, KnownSourceIDs: []string{oldSource},
		PendingUtterances: []SourceUtterance{{ID: newSource, Text: "我说的是那只小狗，还给它喂了水。"}},
		SemanticState: SemanticState{
			Entities:           []Entity{{ID: entityID, Kind: "unknown", Name: "公园遇见的对象", Reference: "unknown", EvidenceSourceIDs: []string{oldSource}}},
			UnresolvedMentions: []Mention{{ID: mentionID, BlockID: oldBlock, Text: "他", EntityID: entityID, SourceIDs: []string{oldSource}}},
		},
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatal(err)
	}
	raw, err := rewriteModelInput(snapshot, 4)
	if err != nil || !strings.Contains(string(raw), "今天在公园遇见他了") || !strings.Contains(string(raw), mentionID) {
		t.Fatalf("unresolved mention context was not selected: %v %s", err, raw)
	}
	revision := Revision{
		BaseRevision: 3, TranscriptRevision: 4,
		Corrections:       []TextCorrection{{BlockID: oldBlock, MentionID: mentionID, ExpectedText: "他", Replacement: "它", EvidenceSourceIDs: []string{newSource}}},
		ConsumedSourceIDs: []string{newSource},
		SemanticState: SemanticState{Entities: []Entity{{
			ID: entityID, Kind: "animal", Name: "小狗", Reference: "it",
			Aliases: []string{"公园遇见的对象"}, EvidenceSourceIDs: []string{oldSource, newSource},
		}}},
	}
	if err := revision.Validate(snapshot); err != nil {
		t.Fatal(err)
	}
	withoutEvidence := revision
	withoutEvidence.Corrections[0].EvidenceSourceIDs = nil
	if withoutEvidence.Validate(snapshot) == nil {
		t.Fatal("accepted a correction without new evidence")
	}
	stillUnresolved := revision
	stillUnresolved.SemanticState.UnresolvedMentions = snapshot.SemanticState.UnresolvedMentions
	if stillUnresolved.Validate(snapshot) == nil {
		t.Fatal("accepted a corrected mention that remained unresolved")
	}
}

func TestRevisionBuildsExplicitOrderedStructureWithSourceProvenance(t *testing.T) {
	heading, first, second, third := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	sources := []string{uuid.NewString(), uuid.NewString(), uuid.NewString()}
	snapshot := Snapshot{
		Revision: 1, Blocks: []Block{{ID: heading, Text: "待整理", Style: "body"}}, ActiveBlockIDs: []string{heading},
		PendingUtterances: []SourceUtterance{
			{ID: sources[0], Text: "第一，要把材料准备好。"},
			{ID: sources[1], Text: "第二，明天确认时间。"},
			{ID: sources[2], Text: "第三，出门前再检查一遍。"},
		},
	}
	revision := Revision{
		BaseRevision: 1, TranscriptRevision: 2,
		BlockEdits: []BlockEdit{
			{Kind: "replace", ID: heading, Text: "接下来要做的三件事", Style: "heading2"},
			{Kind: "insert", ID: first, AfterID: heading, Text: "把材料准备好。", Style: "orderedListItem"},
			{Kind: "insert", ID: second, AfterID: first, Text: "明天确认时间。", Style: "orderedListItem"},
			{Kind: "insert", ID: third, AfterID: second, Text: "出门前再检查一遍。", Style: "orderedListItem"},
		},
		Passages: []Passage{
			{BlockID: heading, SourceIDs: sources},
			{BlockID: first, SourceIDs: []string{sources[0]}},
			{BlockID: second, SourceIDs: []string{sources[1]}},
			{BlockID: third, SourceIDs: []string{sources[2]}},
		},
		ConsumedSourceIDs: sources,
		SemanticState: SemanticState{Outline: []OutlineElement{
			{ID: uuid.NewString(), Role: "topic", Title: "接下来要做的三件事", BlockIDs: []string{heading}, SourceIDs: sources},
			{ID: uuid.NewString(), Role: "point", BlockIDs: []string{first}, SourceIDs: []string{sources[0]}},
			{ID: uuid.NewString(), Role: "point", BlockIDs: []string{second}, SourceIDs: []string{sources[1]}},
			{ID: uuid.NewString(), Role: "point", BlockIDs: []string{third}, SourceIDs: []string{sources[2]}},
		}},
	}
	if err := revision.Validate(snapshot); err != nil {
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

func TestAppliedRevisionCannotDiscardManualFactWithoutLaterSpeech(t *testing.T) {
	id := uuid.NewString()
	transcript := "检查原来约在下周三。"
	snapshot := Snapshot{
		Revision:   4,
		Transcript: transcript,
		Blocks:     []Block{{ID: id, Text: "手动确认为下周五，项目可以退款。"}},
		ManualEdits: []ManualEdit{{
			BlockID: id, Before: "下周三", After: "下周五",
			TranscriptOffset: utf8.RuneCountInString(transcript),
		}},
	}
	stale := Revision{BaseRevision: 4, Patches: []Patch{{
		ID: id, Text: "改成了下周二，项目可以退款。",
	}}}
	if _, err := ApplyRevision(snapshot, stale); !errors.Is(err, ErrConflict) {
		t.Fatal("discarded the user's exact later choice", err)
	}

	deleted := snapshot
	deleted.Blocks[0].Text = "项目可以退款。"
	deleted.ManualEdits[0] = ManualEdit{
		BlockID: id, Before: "检查原来约在下周三。", After: "",
		TranscriptOffset: utf8.RuneCountInString(transcript),
	}
	restored := stale
	restored.Patches[0].Text = "检查原来约在下周三。项目可以退款。"
	if _, err := ApplyRevision(deleted, restored); !errors.Is(err, ErrConflict) {
		t.Fatal("restored a fact the user explicitly deleted", err)
	}

	withLaterSpeech := snapshot
	withLaterSpeech.Transcript += "我刚才又确认了，应该改回下周三。"
	if _, err := ApplyRevision(withLaterSpeech, stale); err != nil {
		t.Fatal("an explicit later correction was blocked", err)
	}
}
