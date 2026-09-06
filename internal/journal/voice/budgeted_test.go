package voice

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/costcontrol"
)

type voiceBudgetStore struct {
	attempt costcontrol.Attempt
	actual  int64
	known   bool
	deny    error
}

func (store *voiceBudgetStore) Reserve(_ context.Context, attempt costcontrol.Attempt, _ costcontrol.Limits) error {
	store.attempt = attempt
	return store.deny
}

func (store *voiceBudgetStore) Settle(_ context.Context, _ string, actual int64, known bool, _ time.Time) error {
	store.actual, store.known = actual, known
	return nil
}

func voiceCostController(t *testing.T, store costcontrol.Store) *costcontrol.Controller {
	t.Helper()
	controller, err := costcontrol.New(store, costcontrol.Limits{
		MonthlyBudgetNanos: 1_000_000_000, MaxConcurrent: 2, LeaseDuration: time.Minute,
	}, func() time.Time { return time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC) })
	if err != nil {
		t.Fatal(err)
	}
	return controller
}

type rewriteStub struct {
	calls  int
	result RewriteResult
	err    error
}

func (stub *rewriteStub) Rewrite(context.Context, Snapshot, int) (RewriteResult, error) {
	stub.calls++
	return stub.result, stub.err
}

func TestBudgetedRewriteSettlesMeteredFailureAndBlocksBeforeProvider(t *testing.T) {
	store := &voiceBudgetStore{}
	price := costcontrol.TokenPrice{InputNanosPerMillionTokens: 1_000_000_000, OutputNanosPerMillionTokens: 2_000_000_000}
	next := &rewriteStub{result: RewriteResult{InputTokens: 10, OutputTokens: 20}, err: ErrInvalid}
	rewriter := NewBudgetedRewriter(next, voiceCostController(t, store), "journal", price)
	snapshot := Snapshot{Revision: 1, Blocks: []Block{{ID: uuid.NewString(), Text: "正文"}}, Transcript: "口述"}
	if _, err := rewriter.Rewrite(context.Background(), snapshot, 1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("rewrite error = %v", err)
	}
	wantActual, _ := price.Cost(10, 20)
	if next.calls != 1 || store.attempt.Operation != "journal.voice.rewrite" || !store.known || store.actual != wantActual {
		t.Fatalf("calls=%d attempt=%+v actual=%d known=%v", next.calls, store.attempt, store.actual, store.known)
	}
	store.deny = costcontrol.ErrBudgetExceeded
	if _, err := rewriter.Rewrite(context.Background(), snapshot, 1); !errors.Is(err, costcontrol.ErrBudgetExceeded) {
		t.Fatalf("budget error = %v", err)
	}
	if next.calls != 1 {
		t.Fatal("rewrite provider was called after budget rejection")
	}
}

type speechStub struct {
	opens int
	conn  *speechConnectionStub
	err   error
}

func (stub *speechStub) Open(context.Context, []string) (SpeechConnection, error) {
	stub.opens++
	return stub.conn, stub.err
}

type speechConnectionStub struct{ sends, closes int }

func (stub *speechConnectionStub) Send([]byte, bool) error      { stub.sends++; return nil }
func (stub *speechConnectionStub) Receive() (Transcript, error) { return Transcript{}, nil }
func (stub *speechConnectionStub) Close() error                 { stub.closes++; return nil }

func TestBudgetedSpeechReservesSegmentAndSettlesSentDuration(t *testing.T) {
	store := &voiceBudgetStore{}
	price := costcontrol.DurationPrice{NanosPerHour: 3_600_000_000}
	next := &speechStub{conn: &speechConnectionStub{}}
	speech := NewBudgetedSpeech(next, voiceCostController(t, store), "journal", price)
	connection, err := speech.Open(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	wantReserved, _ := price.Cost(MaxSegmentBytes / 32)
	if store.attempt.ReservedNanos != wantReserved || store.attempt.Operation != "journal.voice.speech" {
		t.Fatalf("attempt=%+v", store.attempt)
	}
	if err := connection.Send(make([]byte, 32_000), true); err != nil {
		t.Fatal(err)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	wantActual, _ := price.Cost(1_000)
	if next.opens != 1 || next.conn.sends != 1 || next.conn.closes != 1 || !store.known || store.actual != wantActual {
		t.Fatalf("opens=%d sends=%d closes=%d actual=%d known=%v", next.opens, next.conn.sends, next.conn.closes, store.actual, store.known)
	}
	if err := connection.Close(); err != nil || next.conn.closes != 1 {
		t.Fatal("speech connection close was not idempotent")
	}

	store.deny = costcontrol.ErrBudgetExceeded
	if _, err := speech.Open(context.Background(), nil); !errors.Is(err, costcontrol.ErrBudgetExceeded) {
		t.Fatalf("budget error = %v", err)
	}
	if next.opens != 1 {
		t.Fatal("speech provider was opened after budget rejection")
	}
}

func TestArkRewriterRetainsUsageWhenStructuredResultIsInvalid(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"completed","usage":{"input_tokens":11,"output_tokens":22},"output":[{"content":[{"type":"output_text","text":"{}"}]}]}`))
	}))
	defer server.Close()
	snapshot := Snapshot{Revision: 1, Blocks: []Block{{ID: uuid.NewString(), Text: "正文"}}}
	result, err := (ArkRewriter{BaseURL: server.URL, APIKey: "secret", Model: "model", HTTP: server.Client()}).Rewrite(context.Background(), snapshot, 1)
	if err == nil || result.InputTokens != 11 || result.OutputTokens != 22 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

var _ Rewriter = (*rewriteStub)(nil)
var _ Speech = (*speechStub)(nil)
