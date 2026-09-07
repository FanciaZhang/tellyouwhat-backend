// journaldev grants bounded development access to one existing, authenticated
// Journal device. It is an operator command, never an HTTP bypass or app secret.
package main

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
)

func validateGrant(fingerprint string, days int) error {
	decoded, err := hex.DecodeString(fingerprint)
	if err != nil || len(decoded) != 32 || fingerprint != strings.ToLower(fingerprint) || days < 1 || days > 30 {
		return errors.New("provide the exact lowercase SHA-256 device fingerprint and 1-30 days")
	}
	return nil
}

func run() error {
	fingerprint := flag.String("device", "", "exact SHA-256 of the attested key ID")
	days := flag.Int("days", 30, "development grant duration, at most 30 days")
	grant := flag.Bool("grant", false, "apply a development grant; default only lists authenticated Journal devices")
	flag.Parse()
	if *grant {
		if err := validateGrant(*fingerprint, *days); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := mysqlstore.Open(ctx, os.Getenv("DATABASE_DSN"))
	if err != nil {
		return errors.New("database connection failed")
	}
	defer db.Close()
	if !*grant {
		rows, err := db.QueryContext(ctx, `SELECT SHA2(key_id,256),updated_at FROM app_attest_keys WHERE app_id='journal' AND assertion_counter>0 ORDER BY updated_at DESC`)
		if err != nil {
			return errors.New("device lookup failed")
		}
		defer rows.Close()
		for rows.Next() {
			var fingerprint string
			var updated time.Time
			if err := rows.Scan(&fingerprint, &updated); err != nil {
				return err
			}
			if err := json.NewEncoder(os.Stdout).Encode(map[string]any{"device": fingerprint, "last_authenticated": updated}); err != nil {
				return err
			}
		}
		return rows.Err()
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return errors.New("cannot begin grant")
	}
	defer tx.Rollback()
	var keyID string
	err = tx.QueryRowContext(ctx, `SELECT key_id FROM app_attest_keys WHERE app_id='journal' AND assertion_counter>0 AND SHA2(key_id,256)=? FOR UPDATE`, *fingerprint).Scan(&keyID)
	if err != nil {
		return errors.New("no authenticated Journal device matches this fingerprint")
	}
	var environment, transaction string
	var anchor sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT environment,original_transaction_id,started_at FROM managed_entitlements WHERE app_id='journal' AND key_id=? FOR UPDATE`, keyID).Scan(&environment, &transaction, &anchor)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return errors.New("entitlement lookup failed")
	}
	if err == nil && (environment != "development" || transaction != "") {
		return errors.New("refusing to replace a StoreKit entitlement")
	}
	now := time.Now().UTC()
	started := now
	if anchor.Valid {
		started = anchor.Time
	}
	expires := now.Add(time.Duration(*days) * 24 * time.Hour)
	_, err = tx.ExecContext(ctx, `INSERT INTO managed_entitlements(app_id,key_id,original_transaction_id,environment,expires_at,started_at,updated_at) VALUES('journal',?,'','development',?,?,?) ON DUPLICATE KEY UPDATE expires_at=VALUES(expires_at),started_at=VALUES(started_at),updated_at=VALUES(updated_at)`, keyID, expires, started, now)
	if err != nil {
		return errors.New("development grant failed")
	}
	if err = tx.Commit(); err != nil {
		return errors.New("grant commit failed")
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"app": "journal", "device": *fingerprint, "environment": "development", "expires_at": expires, "started_at": started})
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
