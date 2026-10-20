package simulatortest

import (
	"context"
	"database/sql"
	"errors"

	"github.com/tellyouwhat/backend/internal/privacy"
)

// Only the constrained simulator identity uses separate consent rows; real
// devices retain their App Attest foreign keys and existing privacy repository.
type PrivacyRepository struct {
	privacy.Repository
	DB        *sql.DB
	Simulator *Service
}

func (r PrivacyRepository) Ready(ctx context.Context) error {
	if r.DB == nil || r.Simulator == nil || r.Repository == nil {
		return privacy.ErrConsentUnavailable
	}
	rows, err := r.DB.QueryContext(ctx, "SELECT key_id,scope,document_version,granted,recorded_at,expires_at FROM journal_simulator_read_aloud_consents LIMIT 0")
	if err != nil {
		return privacy.ErrConsentUnavailable
	}
	return rows.Close()
}

func (r PrivacyRepository) RecordConsents(ctx context.Context, records []privacy.Record) error {
	simulator := false
	for _, record := range records {
		simulator = simulator || IsKey(record.KeyID)
	}
	if !simulator {
		return r.Repository.RecordConsents(ctx, records)
	}
	if r.DB == nil || len(records) != 1 {
		return privacy.ErrInvalidConsent
	}
	record := records[0]
	if record.Scope != privacy.JournalReadAloudScope || record.DocumentVersion != privacy.JournalReadAloudDocumentVersion || record.DeviceID != record.KeyID {
		return privacy.ErrInvalidConsent
	}
	credential, err := r.Simulator.Lookup(record.KeyID)
	if err != nil {
		return privacy.ErrConsentUnavailable
	}
	_, err = r.DB.ExecContext(ctx, `INSERT INTO journal_simulator_read_aloud_consents
		(key_id,scope,document_version,granted,recorded_at,expires_at) VALUES (?,?,?,?,?,?)
		ON DUPLICATE KEY UPDATE granted=VALUES(granted),recorded_at=VALUES(recorded_at),expires_at=VALUES(expires_at)`,
		record.KeyID, record.Scope, record.DocumentVersion, record.Granted, record.RecordedAt, credential.ExpiresAt)
	return err
}

func (r PrivacyRepository) HasGrantedConsents(ctx context.Context, key string, requirements []privacy.Consent) (bool, error) {
	if !IsKey(key) {
		reader, ok := r.Repository.(privacy.ConsentReader)
		if !ok {
			return false, privacy.ErrConsentUnavailable
		}
		return reader.HasGrantedConsents(ctx, key, requirements)
	}
	if r.DB == nil || len(requirements) != 1 || requirements[0].Scope != privacy.JournalReadAloudScope || requirements[0].DocumentVersion != privacy.JournalReadAloudDocumentVersion {
		return false, privacy.ErrInvalidConsent
	}
	if _, err := r.Simulator.Lookup(key); err != nil {
		return false, privacy.ErrConsentUnavailable
	}
	var granted bool
	err := r.DB.QueryRowContext(ctx, `SELECT granted FROM journal_simulator_read_aloud_consents
		WHERE key_id=? AND scope=? AND document_version=? AND expires_at>?`,
		key, requirements[0].Scope, requirements[0].DocumentVersion, r.Simulator.now()).Scan(&granted)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return granted && err == nil, err
}
