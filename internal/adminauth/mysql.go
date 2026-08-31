package adminauth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
)

var (
	ErrBootstrapInvalid = errors.New("admin bootstrap token is invalid or expired")
	ErrOwnerExists      = errors.New("admin owner already exists")
)

type MySQLRepository struct{ database *sql.DB }

func NewMySQLRepository(database *sql.DB) *MySQLRepository {
	return &MySQLRepository{database: database}
}

func (repository *MySQLRepository) Owner(ctx context.Context) (User, bool, error) {
	var user User
	err := repository.database.QueryRowContext(ctx, `
        SELECT id, webauthn_id, display_name
        FROM admin_users
        WHERE role = 'owner' AND status = 'active'
        LIMIT 1`).Scan(&user.ID, &user.Handle, &user.DisplayName)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, false, nil
	}
	if err != nil {
		return User{}, false, err
	}
	credentials, err := repository.credentials(ctx, repository.database, user.ID)
	if err != nil {
		return User{}, false, err
	}
	user.Credentials = credentials
	return user, true, nil
}

func (repository *MySQLRepository) CreateBootstrapToken(ctx context.Context, hash [32]byte, expiresAt time.Time) error {
	_, err := repository.database.ExecContext(ctx, `
        INSERT INTO admin_bootstrap_tokens (token_hash, expires_at)
        VALUES (?, ?)`, hash[:], expiresAt.UTC())
	return err
}

func (repository *MySQLRepository) BootstrapTokenValid(ctx context.Context, hash [32]byte, now time.Time) (bool, error) {
	var count int
	err := repository.database.QueryRowContext(ctx, `
        SELECT COUNT(*)
        FROM admin_bootstrap_tokens
        WHERE token_hash = ? AND consumed_at IS NULL AND expires_at > ?`, hash[:], now.UTC()).Scan(&count)
	return count == 1, err
}

func (repository *MySQLRepository) CompleteBootstrap(
	ctx context.Context,
	hash [32]byte,
	user User,
	credential webauthn.Credential,
	recoveryHashes []string,
	now time.Time,
) error {
	transaction, err := repository.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	var existingOwnerID string
	err = transaction.QueryRowContext(ctx, `
		SELECT id FROM admin_users WHERE role = 'owner' LIMIT 1 FOR UPDATE`).Scan(&existingOwnerID)
	if err == nil {
		return ErrOwnerExists
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var expiresAt time.Time
	var consumedAt sql.NullTime
	if err := transaction.QueryRowContext(ctx, `
		SELECT expires_at, consumed_at FROM admin_bootstrap_tokens
		WHERE token_hash = ? FOR UPDATE`, hash[:]).Scan(&expiresAt, &consumedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrBootstrapInvalid
		}
		return err
	}
	if consumedAt.Valid || !now.UTC().Before(expiresAt.UTC()) {
		return ErrBootstrapInvalid
	}
	if _, err := transaction.ExecContext(ctx, `
        INSERT INTO admin_users (id, webauthn_id, display_name)
        VALUES (?, ?, ?)`, user.ID, user.Handle, user.DisplayName); err != nil {
		return err
	}
	if err := insertCredential(ctx, transaction, user.ID, "主通行密钥", credential); err != nil {
		return err
	}
	for _, recoveryHash := range recoveryHashes {
		if _, err := transaction.ExecContext(ctx, `
            INSERT INTO admin_recovery_codes (admin_user_id, code_hash)
            VALUES (?, ?)`, user.ID, recoveryHash); err != nil {
			return err
		}
	}
	if _, err := transaction.ExecContext(ctx, `
        UPDATE admin_bootstrap_tokens SET consumed_at = ? WHERE token_hash = ?`, now.UTC(), hash[:]); err != nil {
		return err
	}
	return transaction.Commit()
}

func (repository *MySQLRepository) AddCredential(
	ctx context.Context,
	userID string,
	displayName string,
	credential webauthn.Credential,
) error {
	return insertCredential(ctx, repository.database, userID, displayName, credential)
}

func (repository *MySQLRepository) UpdateCredential(
	ctx context.Context,
	userID string,
	credential webauthn.Credential,
	now time.Time,
) error {
	encoded, err := json.Marshal(credential)
	if err != nil {
		return err
	}
	result, err := repository.database.ExecContext(ctx, `
        UPDATE admin_webauthn_credentials
        SET credential_json = ?, last_used_at = ?
        WHERE credential_id = ? AND admin_user_id = ?`, encoded, now.UTC(), credential.ID, userID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errors.New("admin credential not found")
	}
	return nil
}

func (repository *MySQLRepository) ConsumeRecoveryCode(
	ctx context.Context,
	code string,
	now time.Time,
) (User, bool, error) {
	transaction, err := repository.database.BeginTx(ctx, nil)
	if err != nil {
		return User{}, false, err
	}
	defer func() { _ = transaction.Rollback() }()
	var user User
	if err := transaction.QueryRowContext(ctx, `
        SELECT id, webauthn_id, display_name
        FROM admin_users
        WHERE role = 'owner' AND status = 'active'
        LIMIT 1 FOR UPDATE`).Scan(&user.ID, &user.Handle, &user.DisplayName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, false, nil
		}
		return User{}, false, err
	}
	rows, err := transaction.QueryContext(ctx, `
        SELECT id, code_hash
        FROM admin_recovery_codes
        WHERE admin_user_id = ? AND consumed_at IS NULL
        FOR UPDATE`, user.ID)
	if err != nil {
		return User{}, false, err
	}
	type candidate struct {
		id   uint64
		hash string
	}
	var candidates []candidate
	for rows.Next() {
		var value candidate
		if err := rows.Scan(&value.id, &value.hash); err != nil {
			_ = rows.Close()
			return User{}, false, err
		}
		candidates = append(candidates, value)
	}
	if err := rows.Close(); err != nil {
		return User{}, false, err
	}
	for _, candidate := range candidates {
		if !VerifyRecoveryCode(code, candidate.hash) {
			continue
		}
		if _, err := transaction.ExecContext(ctx, `
            UPDATE admin_recovery_codes SET consumed_at = ? WHERE id = ?`, now.UTC(), candidate.id); err != nil {
			return User{}, false, err
		}
		if err := transaction.Commit(); err != nil {
			return User{}, false, err
		}
		return user, true, nil
	}
	return User{}, false, nil
}

func (repository *MySQLRepository) ReplaceRecoveryCodes(
	ctx context.Context,
	userID string,
	hashes []string,
	now time.Time,
) error {
	transaction, err := repository.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	if _, err := transaction.ExecContext(ctx, `
        UPDATE admin_recovery_codes SET consumed_at = ?
        WHERE admin_user_id = ? AND consumed_at IS NULL`, now.UTC(), userID); err != nil {
		return err
	}
	for _, hash := range hashes {
		if _, err := transaction.ExecContext(ctx, `
            INSERT INTO admin_recovery_codes (admin_user_id, code_hash)
            VALUES (?, ?)`, userID, hash); err != nil {
			return err
		}
	}
	return transaction.Commit()
}

func (repository *MySQLRepository) AppendAudit(ctx context.Context, event AuditEvent) error {
	metadata, err := json.Marshal(event.Metadata)
	if err != nil {
		return err
	}
	var userID any
	if event.UserID != "" {
		userID = event.UserID
	}
	_, err = repository.database.ExecContext(ctx, `
        INSERT INTO admin_audit_events
            (admin_user_id, request_id, action, target_type, target_id, outcome, metadata_json)
        VALUES (?, ?, ?, ?, ?, ?, ?)`, userID, event.RequestID, event.Action, event.TargetType,
		event.TargetID, event.Outcome, metadata)
	return err
}

type credentialQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

type credentialExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func (repository *MySQLRepository) credentials(
	ctx context.Context,
	queryer credentialQueryer,
	userID string,
) ([]webauthn.Credential, error) {
	rows, err := queryer.QueryContext(ctx, `
        SELECT credential_json
        FROM admin_webauthn_credentials
        WHERE admin_user_id = ?
        ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var credentials []webauthn.Credential
	for rows.Next() {
		var encoded []byte
		if err := rows.Scan(&encoded); err != nil {
			return nil, err
		}
		var credential webauthn.Credential
		if err := json.Unmarshal(encoded, &credential); err != nil {
			return nil, err
		}
		credentials = append(credentials, credential)
	}
	return credentials, rows.Err()
}

func insertCredential(
	ctx context.Context,
	execer credentialExecer,
	userID string,
	displayName string,
	credential webauthn.Credential,
) error {
	encoded, err := json.Marshal(credential)
	if err != nil {
		return err
	}
	_, err = execer.ExecContext(ctx, `
        INSERT INTO admin_webauthn_credentials
            (credential_id, admin_user_id, display_name, credential_json)
        VALUES (?, ?, ?, ?)`, credential.ID, userID, displayName, encoded)
	return err
}

var _ Repository = (*MySQLRepository)(nil)
