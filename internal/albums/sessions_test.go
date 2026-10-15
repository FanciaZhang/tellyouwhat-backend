package albums

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestSessionCredentialParsingAndRedaction(t *testing.T) {
	token := "als1_" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{13}, 32))
	digest, err := sessionDigest(token)
	if err != nil || digest != sha256.Sum256([]byte(token)) {
		t.Fatal("invalid hash", err)
	}
	for _, value := range []string{"", token + "=", token + " ", "other" + token[5:], "als1_" + strings.Repeat("_", 43)} {
		if _, err := sessionDigest(value); !errors.Is(err, ErrSession) {
			t.Fatalf("accepted invalid token %q", value)
		}
	}
	for _, headers := range [][]string{{}, {"Bearer "}, {"Basic " + token}, {"Bearer  " + token}, {"Bearer " + token + ", Bearer " + token}, {"Bearer " + token, "Bearer " + token}} {
		r := httptest.NewRequest("GET", "https://albums.example.test/", nil)
		for _, header := range headers {
			r.Header.Add("Authorization", header)
		}
		if _, err := requestSessionToken(r); !errors.Is(err, ErrSession) {
			t.Fatal("accepted ambiguous authorization")
		}
	}
	r := httptest.NewRequest("GET", "https://albums.example.test/?token="+token, nil)
	if _, err := requestSessionToken(r); err == nil {
		t.Fatal("query token accepted")
	}
	r.Header.Set("Authorization", "bearer "+token)
	if got, err := requestSessionToken(r); err != nil || got != token {
		t.Fatal("bearer rejected")
	}
	session := AlbumSession{Token: token}
	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("session", "value", session)
	if strings.Contains(fmt.Sprintf("%v %+v %#v", session, session, session)+logs.String(), token) {
		t.Fatal("token leaked in logs")
	}
	encoded, err := json.Marshal(session)
	if err != nil || !bytes.Contains(encoded, []byte(token)) {
		t.Fatal("login response omitted credential")
	}
	if _, err := NewMySQLSessions(nil, nil); err == nil {
		t.Fatal("nil database accepted")
	}
}

// Called with real accounts/grants from the MySQL account integration fixture.
func checkMySQLSessionLifecycle(t *testing.T, ctx context.Context, db *sql.DB, first, second AccountLogin) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	sessions, err := NewMySQLSessions(db, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	one, err := sessions.Issue(ctx, first)
	if err != nil {
		t.Fatal("issue first", err)
	}
	two, err := sessions.Issue(ctx, second)
	if err != nil {
		t.Fatal("issue second", err)
	}
	if one.Token == two.Token {
		t.Fatal("shared device token")
	}
	request := httptest.NewRequest("GET", "https://albums.example.test/", nil)
	request.Header.Set("Authorization", "Bearer "+one.Token)
	principal, err := sessions.Authenticate(ctx, request)
	if err != nil || principal.AccountID != first.AccountID || principal.AppID != "albums" {
		t.Fatal("authentication", err)
	}
	digest, _ := sessionDigest(one.Token)
	var stored []byte
	if err := db.QueryRowContext(ctx, `SELECT token_sha256 FROM album_sessions WHERE token_sha256=?`, digest[:]).Scan(&stored); err != nil || !bytes.Equal(stored, digest[:]) {
		t.Fatal("hash persistence", err)
	}
	// Rebuilding the service must not lose authentication or revocations.
	sessions, _ = NewMySQLSessions(db, func() time.Time { return now })
	if _, err := sessions.Authenticate(ctx, request); err != nil {
		t.Fatal("restart lost session")
	}
	if _, err := sessions.Issue(ctx, AccountLogin{AccountID: uuid.NewString(), GrantID: first.GrantID}); err == nil {
		t.Fatal("cross-account grant accepted")
	}
	now = one.ExpiresAt
	if _, err := sessions.Authenticate(ctx, request); !errors.Is(err, ErrSession) {
		t.Fatal("expired session accepted")
	}
	now = now.Add(-time.Hour)
	for range 2 {
		if err := sessions.Revoke(ctx, one.Token); err != nil {
			t.Fatal("logout", err)
		}
	}
	if _, err := sessions.Authenticate(ctx, request); err == nil {
		t.Fatal("logged out session accepted")
	}
	request.Header.Set("Authorization", "Bearer "+two.Token)
	if _, err := sessions.Authenticate(ctx, request); err != nil {
		t.Fatal("logout affected other device")
	}
	if _, err := db.ExecContext(ctx, `UPDATE album_accounts SET state='disabled' WHERE account_id=?`, second.AccountID); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Authenticate(ctx, request); err == nil {
		t.Fatal("disabled account accepted")
	}
	if _, err := sessions.Issue(ctx, second); err == nil {
		t.Fatal("disabled account issued session")
	}
	if _, err := db.ExecContext(ctx, `UPDATE album_accounts SET state='active' WHERE account_id=?`, second.AccountID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE album_apple_grants SET revoked_at=? WHERE grant_id=?`, now, second.GrantID); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Authenticate(ctx, request); err == nil {
		t.Fatal("revoked provider grant accepted")
	}
	if _, err := sessions.Issue(ctx, second); err == nil {
		t.Fatal("revoked grant issued session")
	}
	// Restore fixture so its own revocation assertions remain independent.
	if _, err := db.ExecContext(ctx, `UPDATE album_apple_grants SET revoked_at=NULL WHERE grant_id=?`, second.GrantID); err != nil {
		t.Fatal(err)
	}
}
