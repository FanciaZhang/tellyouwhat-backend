package albums

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var errAppleNotification = errors.New("album Apple notification rejected")

type appleAccountEvent struct {
	Type      string `json:"type"`
	Subject   string `json:"sub"`
	EventTime int64  `json:"event_time"`
}

// Notifications are a separate JWT purpose: no nonce, exp is optional, and the
// subject is inside events. Reuse only Apple's fixed-endpoint key cache.
func (v *AppleIdentityVerifier) verifyAccountEvent(ctx context.Context, encoded string) (appleAccountEvent, error) {
	var empty appleAccountEvent
	if len(encoded) == 0 || len(encoded) > 32<<10 {
		return empty, errAppleNotification
	}
	claims := struct {
		jwt.RegisteredClaims
		Events json.RawMessage `json:"events"`
	}{}
	now := v.now().UTC()
	token, err := jwt.ParseWithClaims(encoded, &claims, func(token *jwt.Token) (any, error) {
		kid, ok := token.Header["kid"].(string)
		if token.Method != jwt.SigningMethodRS256 || !ok || kid == "" || len(kid) > 128 {
			return nil, errAppleNotification
		}
		return v.key(ctx, kid)
	}, jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuer(appleIssuer), jwt.WithAudience(albumAppleAudience),
		jwt.WithIssuedAt(), jwt.WithTimeFunc(func() time.Time { return now }))
	if errors.Is(err, errAppleKeyUnavailable) {
		return empty, errAppleKeyUnavailable
	}
	if err != nil || token == nil || !token.Valid || claims.IssuedAt == nil || len(claims.Audience) != 1 ||
		claims.ID == "" || len(claims.ID) > 255 || claims.IssuedAt.Unix() <= 0 {
		return empty, errAppleNotification
	}
	data := claims.Events
	// Accept the events object directly or encoded as a JSON string.
	if len(data) > 0 && data[0] == '"' {
		var value string
		if json.Unmarshal(data, &value) != nil {
			return empty, errAppleNotification
		}
		data = []byte(value)
	}
	var event appleAccountEvent
	if len(data) > 8<<10 || json.Unmarshal(data, &event) != nil || event.Subject == "" || len(event.Subject) > 255 ||
		event.EventTime <= 0 || event.EventTime > claims.IssuedAt.Unix() || event.EventTime > now.Unix() {
		return empty, errAppleNotification
	}
	switch event.Type {
	case "consent-revoked", "account-deleted", "email-enabled", "email-disabled":
		return event, nil
	default:
		return empty, errAppleNotification
	}
}

func lockAppleIdentity(ctx context.Context, tx *sql.Tx, hash string) (revoked, deleted sql.NullTime, err error) {
	_, err = tx.ExecContext(ctx, `INSERT INTO album_apple_identity_state(subject_sha256) VALUES (?) ON DUPLICATE KEY UPDATE subject_sha256=subject_sha256`, hash)
	if err != nil {
		return
	}
	err = tx.QueryRowContext(ctx, `SELECT revoked_before,deleted_at FROM album_apple_identity_state WHERE subject_sha256=? FOR UPDATE`, hash).Scan(&revoked, &deleted)
	return
}

func (r *MySQLAccounts) applyAppleEvent(ctx context.Context, event appleAccountEvent) error {
	if event.Type == "email-enabled" || event.Type == "email-disabled" {
		return nil
	} // This app does not collect relay email.
	if (event.Type != "consent-revoked" && event.Type != "account-deleted") || event.Subject == "" || len(event.Subject) > 255 || event.EventTime <= 0 {
		return errAppleNotification
	}
	hash := appleSubjectHash(event.Subject)
	at := time.Unix(event.EventTime, 0).UTC()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	before, deleted, err := lockAppleIdentity(ctx, tx, hash)
	if err != nil {
		return err
	}
	// A monotonic fence makes repeated/out-of-order events harmless even before
	// the first login. No raw JWT, subject, email, or unbounded receipt ledger.
	if !before.Valid || at.After(before.Time) {
		before = sql.NullTime{Time: at, Valid: true}
	}
	if event.Type == "account-deleted" && (!deleted.Valid || at.Before(deleted.Time)) {
		deleted = sql.NullTime{Time: at, Valid: true}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE album_apple_identity_state SET revoked_before=?,deleted_at=? WHERE subject_sha256=?`, before, deleted, hash); err != nil {
		return err
	}
	var account string
	err = tx.QueryRowContext(ctx, `SELECT account_id FROM album_accounts WHERE apple_subject_sha256=? FOR UPDATE`, hash).Scan(&account)
	if errors.Is(err, sql.ErrNoRows) {
		return tx.Commit()
	}
	if err != nil {
		return err
	}
	if deleted.Valid {
		// Stop access; physical media/account deletion has a separate audited workflow.
		if _, err = tx.ExecContext(ctx, `UPDATE album_accounts SET state='disabled' WHERE account_id=? AND state='active'`, account); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE album_apple_grants SET revoked_at=COALESCE(revoked_at,?) WHERE account_id=?`, before.Time, account)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE album_apple_grants SET revoked_at=COALESCE(revoked_at,?) WHERE account_id=? AND (identity_issued_at IS NULL OR identity_issued_at<=?)`, before.Time, account, before.Time)
	}
	if err != nil {
		return err
	}
	// Both bearer authentication and push target lookup already join grant state.
	return tx.Commit()
}

type appleAccountNotifications struct {
	verifier *AppleIdentityVerifier
	accounts *MySQLAccounts
}

func (s *appleAccountNotifications) receive(ctx context.Context, payload string) error {
	event, err := s.verifier.verifyAccountEvent(ctx, payload)
	if err != nil {
		return err
	}
	return s.accounts.applyAppleEvent(ctx, event)
}
