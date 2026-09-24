package voice

import (
	"context"
	"github.com/google/uuid"
	"os"
	"testing"
	"time"
)

func TestLiveIncrementalSyntheticRevision(t *testing.T) {
	if os.Getenv("JOURNAL_INCREMENTAL_LIVE_CHECK") != "1" {
		t.Skip("explicit synthetic provider acceptance")
	}
	bid, sid := uuid.NewString(), uuid.NewString()
	s := Snapshot{Blocks: []Block{{ID: bid, Style: "body"}}, WritingStyle: StyleNatural, ActiveBlockIDs: []string{bid}, KnownSourceIDs: []string{sid}, PendingUtterances: []SourceUtterance{{ID: sid, Text: "今天下午我去公园散步，看到一只小狗，心情很好。"}}}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	r, err := (ArkRewriter{BaseURL: os.Getenv("JOURNAL_ARK_BASE_URL"), APIKey: os.Getenv("JOURNAL_ARK_API_KEY"), Model: os.Getenv("JOURNAL_VOICE_MODEL")}).Rewrite(ctx, s, 0)
	if err != nil {
		t.Fatalf("synthetic-only output=%s error=%v", r.OutputText, err)
	}
	if len(r.Revision.BlockEdits) == 0 {
		t.Fatal("no body edit")
	}
}
