package voice

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestDynamicVoiceQuotaAndPauseBeforeConnection(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	limit := 60_000
	paused := false
	s := &Service{Store: store, Secret: make([]byte, 32), ResolveLimit: func(context.Context) (int, error) {
		if paused {
			return 0, errors.New("paused")
		}
		return limit, nil
	}}
	id := Identity{Owner: "fixture", Anchor: time.Now().AddDate(0, -1, 0), ExpiresAt: time.Now().Add(time.Hour)}
	session := uuid.NewString()
	first, err := s.Issue(ctx, id, session)
	if err != nil {
		t.Fatal(err)
	}
	limit = 120_000
	second, err := s.Issue(ctx, id, session)
	if err != nil {
		t.Fatal(err)
	}
	if first.RemainingMilliseconds != 60_000 || second.RemainingMilliseconds != 120_000 {
		t.Fatalf("limits: %+v %+v", first, second)
	}
	paused = true
	if _, err = s.Issue(ctx, id, session); err == nil {
		t.Fatal("paused ticket issued")
	}
	req := httptest.NewRequest("GET", "/voice", nil)
	req.Header.Set("Authorization", "Bearer "+second.Token)
	res := httptest.NewRecorder()
	s.Serve(res, req, session)
	if res.Code != 503 {
		t.Fatalf("pre-pause ticket connected: %d", res.Code)
	}
}
