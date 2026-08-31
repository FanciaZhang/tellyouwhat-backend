package adminauth

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/go-webauthn/webauthn/webauthn"
)

var (
	ErrBootstrapInvalid     = errors.New("admin bootstrap token is invalid or expired")
	ErrAlreadyInitialized   = errors.New("admin service is already initialized")
	ErrInvitationInvalid    = errors.New("admin invitation is invalid or expired")
	ErrInvitationUsed       = errors.New("admin invitation is already used or revoked")
	ErrUserNotFound         = errors.New("admin user was not found")
	ErrLastAdmin            = errors.New("the last active admin cannot be disabled or demoted")
	ErrLastCredential       = errors.New("the last passkey cannot be removed")
	ErrCredentialExists     = errors.New("the passkey is already registered")
	ErrCredentialNotFound   = errors.New("the passkey was not found")
	ErrAuthorizationChanged = errors.New("the administrator authorization changed")
	ErrInvalidUser          = errors.New("invalid admin user configuration")
)

type MySQLRepository struct{ database *sql.DB }

func NewMySQLRepository(database *sql.DB) *MySQLRepository {
	return &MySQLRepository{database: database}
}

func (repository *MySQLRepository) Initialized(ctx context.Context) (bool, error) {
	var initialized sql.NullTime
	err := repository.database.QueryRowContext(ctx, `
		SELECT initialized_at FROM admin_control_state WHERE singleton_id = 1`).Scan(&initialized)
	return initialized.Valid, err
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
		SELECT COUNT(*) FROM admin_bootstrap_tokens
		WHERE token_hash = ? AND consumed_at IS NULL AND expires_at > ?`, hash[:], now.UTC()).Scan(&count)
	return count == 1, err
}

func (repository *MySQLRepository) CompleteBootstrap(
	ctx context.Context,
	hash [32]byte,
	user User,
	credential webauthn.Credential,
	now time.Time,
) error {
	transaction, err := repository.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	var initialized sql.NullTime
	if err := transaction.QueryRowContext(ctx, `
		SELECT initialized_at FROM admin_control_state WHERE singleton_id = 1 FOR UPDATE`).Scan(&initialized); err != nil {
		return err
	}
	if initialized.Valid {
		return ErrAlreadyInitialized
	}
	if err := lockValidBootstrap(ctx, transaction, hash, now); err != nil {
		return err
	}
	if _, err := transaction.ExecContext(ctx, `
		INSERT INTO admin_users
			(id, webauthn_id, display_name, role, status, session_version, created_at, updated_at)
		VALUES (?, ?, ?, 'admin', 'active', 1, ?, ?)`,
		user.ID, user.Handle, user.DisplayName, now.UTC(), now.UTC()); err != nil {
		return err
	}
	if err := insertCredential(ctx, transaction, user.ID, "主通行密钥", credential); err != nil {
		return credentialInsertError(err)
	}
	if _, err := transaction.ExecContext(ctx, `
		UPDATE admin_bootstrap_tokens SET consumed_at = ? WHERE token_hash = ?`, now.UTC(), hash[:]); err != nil {
		return err
	}
	if _, err := transaction.ExecContext(ctx, `
		UPDATE admin_control_state SET initialized_at = ? WHERE singleton_id = 1`, now.UTC()); err != nil {
		return err
	}
	return transaction.Commit()
}

func lockValidBootstrap(ctx context.Context, transaction *sql.Tx, hash [32]byte, now time.Time) error {
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
	return nil
}

func (repository *MySQLRepository) UserByID(ctx context.Context, id string) (User, bool, error) {
	return repository.user(ctx, repository.database, "id = ?", id)
}

func (repository *MySQLRepository) UserByHandle(ctx context.Context, handle []byte) (User, bool, error) {
	return repository.user(ctx, repository.database, "webauthn_id = ?", handle)
}

func (repository *MySQLRepository) user(
	ctx context.Context,
	queryer adminQueryer,
	clause string,
	argument any,
) (User, bool, error) {
	var user User
	err := queryer.QueryRowContext(ctx, `
		SELECT id, webauthn_id, display_name, role, status, session_version
		FROM admin_users WHERE `+clause+` LIMIT 1`, argument).
		Scan(&user.ID, &user.Handle, &user.DisplayName, &user.Role, &user.Status, &user.SessionVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, false, nil
	}
	if err != nil {
		return User{}, false, err
	}
	user.AppIDs, err = appIDs(ctx, queryer, user.ID)
	if err != nil {
		return User{}, false, err
	}
	user.Credentials, err = credentials(ctx, queryer, user.ID)
	if err != nil {
		return User{}, false, err
	}
	return user, true, nil
}

func (repository *MySQLRepository) ListUsers(ctx context.Context) ([]UserSummary, error) {
	rows, err := repository.database.QueryContext(ctx, `
		SELECT u.id, u.display_name, u.role, u.status,
			(SELECT COUNT(*) FROM admin_webauthn_credentials c WHERE c.admin_user_id = u.id),
			u.created_at, u.updated_at
		FROM admin_users u
		ORDER BY u.created_at, u.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	users := make([]UserSummary, 0)
	for rows.Next() {
		var user UserSummary
		if err := rows.Scan(&user.ID, &user.DisplayName, &user.Role, &user.Status,
			&user.CredentialCount, &user.CreatedAt, &user.UpdatedAt); err != nil {
			return nil, err
		}
		users = append(users, user)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for index := range users {
		users[index].AppIDs, err = appIDs(ctx, repository.database, users[index].ID)
		if err != nil {
			return nil, err
		}
	}
	return users, nil
}

func (repository *MySQLRepository) UpdateUser(
	ctx context.Context,
	userID string,
	update UserUpdate,
	now time.Time,
) (User, error) {
	update.AppIDs = uniqueStrings(update.AppIDs)
	if update.Role == RoleAdmin {
		update.AppIDs = []string{}
	}
	if update.DisplayName == "" || !update.Role.Valid() || !update.Status.Valid() ||
		update.Role == RoleOperator && len(update.AppIDs) == 0 {
		return User{}, ErrInvalidUser
	}
	transaction, err := repository.database.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer func() { _ = transaction.Rollback() }()
	var controlID uint8
	if err := transaction.QueryRowContext(ctx, `
		SELECT singleton_id FROM admin_control_state WHERE singleton_id = 1 FOR UPDATE`).Scan(&controlID); err != nil {
		return User{}, err
	}
	var currentRole Role
	var currentStatus UserStatus
	var currentSessionVersion uint64
	if err := transaction.QueryRowContext(ctx, `
		SELECT role, status, session_version FROM admin_users WHERE id = ? FOR UPDATE`, userID).
		Scan(&currentRole, &currentStatus, &currentSessionVersion); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, ErrUserNotFound
		}
		return User{}, err
	}
	if currentRole == RoleAdmin && currentStatus == UserStatusActive &&
		(update.Role != RoleAdmin || update.Status != UserStatusActive) {
		var otherAdmins int
		if err := transaction.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM admin_users
			WHERE id <> ? AND role = 'admin' AND status = 'active'`, userID).Scan(&otherAdmins); err != nil {
			return User{}, err
		}
		if otherAdmins == 0 {
			return User{}, ErrLastAdmin
		}
	}
	currentAppIDs, err := appIDs(ctx, transaction, userID)
	if err != nil {
		return User{}, err
	}
	authorizationChanged := currentRole != update.Role || currentStatus != update.Status ||
		!slices.Equal(currentAppIDs, update.AppIDs)
	sessionIncrement := 0
	if authorizationChanged {
		sessionIncrement = 1
	}
	if _, err := transaction.ExecContext(ctx, `
		UPDATE admin_users
		SET display_name = ?, role = ?, status = ?, session_version = session_version + ?, updated_at = ?
		WHERE id = ?`, update.DisplayName, update.Role, update.Status, sessionIncrement, now.UTC(), userID); err != nil {
		return User{}, err
	}
	if err := replaceUserApps(ctx, transaction, userID, update.Role, update.AppIDs); err != nil {
		return User{}, err
	}
	user, found, err := repository.user(ctx, transaction, "id = ?", userID)
	if err != nil || !found {
		if !found && err == nil {
			err = ErrUserNotFound
		}
		return User{}, err
	}
	if !authorizationChanged && user.SessionVersion != currentSessionVersion {
		return User{}, errors.New("unchanged authorization unexpectedly invalidated sessions")
	}
	if err := transaction.Commit(); err != nil {
		return User{}, err
	}
	return user, nil
}

func (repository *MySQLRepository) CreateInvitation(
	ctx context.Context,
	tokenHash [32]byte,
	invitation Invitation,
) error {
	if invitation.ID == "" || invitation.DisplayName == "" || !invitation.Role.Valid() ||
		invitation.Kind != InvitationKindCreate && invitation.Kind != InvitationKindRecovery ||
		invitation.Kind == InvitationKindCreate && invitation.TargetUserID != "" ||
		invitation.Kind == InvitationKindRecovery && invitation.TargetUserID == "" ||
		invitation.Role == RoleOperator && len(invitation.AppIDs) == 0 {
		return ErrInvitationInvalid
	}
	transaction, err := repository.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	if invitation.Kind == InvitationKindRecovery {
		var status UserStatus
		if err := transaction.QueryRowContext(ctx, `
			SELECT status FROM admin_users WHERE id = ? FOR UPDATE`, invitation.TargetUserID).Scan(&status); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrUserNotFound
			}
			return err
		}
		if status != UserStatusActive {
			return ErrInvitationInvalid
		}
		if _, err := transaction.ExecContext(ctx, `
			DELETE FROM admin_webauthn_credentials WHERE admin_user_id = ?`, invitation.TargetUserID); err != nil {
			return err
		}
		if _, err := transaction.ExecContext(ctx, `
			UPDATE admin_users SET session_version = session_version + 1, updated_at = ? WHERE id = ?`,
			invitation.CreatedAt.UTC(), invitation.TargetUserID); err != nil {
			return err
		}
		if _, err := transaction.ExecContext(ctx, `
			UPDATE admin_invitations SET revoked_at = ?
			WHERE kind = 'recovery' AND target_admin_user_id = ?
				AND consumed_at IS NULL AND revoked_at IS NULL`, invitation.CreatedAt.UTC(), invitation.TargetUserID); err != nil {
			return err
		}
	}
	var targetID, actorID any
	if invitation.TargetUserID != "" {
		targetID = invitation.TargetUserID
	}
	if invitation.InvitedByID != "" {
		actorID = invitation.InvitedByID
	}
	if _, err := transaction.ExecContext(ctx, `
		INSERT INTO admin_invitations
			(id, token_hash, kind, target_admin_user_id, invited_by_admin_user_id,
			 display_name, role, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, invitation.ID, tokenHash[:], invitation.Kind,
		targetID, actorID, invitation.DisplayName, invitation.Role, invitation.ExpiresAt.UTC(), invitation.CreatedAt.UTC()); err != nil {
		return err
	}
	if err := insertInvitationApps(ctx, transaction, invitation.ID, invitation.Role, invitation.AppIDs); err != nil {
		return err
	}
	return transaction.Commit()
}

func (repository *MySQLRepository) InvitationByToken(
	ctx context.Context,
	hash [32]byte,
	now time.Time,
) (Invitation, bool, error) {
	var invitation Invitation
	var targetID, actorID, actorName sql.NullString
	err := repository.database.QueryRowContext(ctx, `
		SELECT i.id, i.kind, i.target_admin_user_id, i.invited_by_admin_user_id,
			a.display_name, i.display_name, i.role, i.expires_at, i.created_at
		FROM admin_invitations i
		LEFT JOIN admin_users a ON a.id = i.invited_by_admin_user_id
		WHERE i.token_hash = ? AND i.consumed_at IS NULL AND i.revoked_at IS NULL AND i.expires_at > ?`,
		hash[:], now.UTC()).Scan(&invitation.ID, &invitation.Kind, &targetID, &actorID,
		&actorName, &invitation.DisplayName, &invitation.Role, &invitation.ExpiresAt, &invitation.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Invitation{}, false, nil
	}
	if err != nil {
		return Invitation{}, false, err
	}
	invitation.TargetUserID = targetID.String
	invitation.InvitedByID = actorID.String
	invitation.InvitedByName = actorName.String
	invitation.AppIDs, err = invitationAppIDs(ctx, repository.database, invitation.ID)
	return invitation, err == nil, err
}

func (repository *MySQLRepository) ListInvitations(ctx context.Context, now time.Time) ([]Invitation, error) {
	rows, err := repository.database.QueryContext(ctx, `
		SELECT i.id, i.kind, i.target_admin_user_id, i.invited_by_admin_user_id,
			a.display_name, i.display_name, i.role, i.expires_at, i.created_at
		FROM admin_invitations i
		LEFT JOIN admin_users a ON a.id = i.invited_by_admin_user_id
		WHERE i.consumed_at IS NULL AND i.revoked_at IS NULL AND i.expires_at > ?
		ORDER BY i.created_at DESC`, now.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	invitations := make([]Invitation, 0)
	for rows.Next() {
		var invitation Invitation
		var targetID, actorID, actorName sql.NullString
		if err := rows.Scan(&invitation.ID, &invitation.Kind, &targetID, &actorID,
			&actorName, &invitation.DisplayName, &invitation.Role, &invitation.ExpiresAt, &invitation.CreatedAt); err != nil {
			return nil, err
		}
		invitation.TargetUserID = targetID.String
		invitation.InvitedByID = actorID.String
		invitation.InvitedByName = actorName.String
		invitations = append(invitations, invitation)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for index := range invitations {
		invitations[index].AppIDs, err = invitationAppIDs(ctx, repository.database, invitations[index].ID)
		if err != nil {
			return nil, err
		}
	}
	return invitations, nil
}

func (repository *MySQLRepository) RevokeInvitation(ctx context.Context, invitationID string, now time.Time) error {
	result, err := repository.database.ExecContext(ctx, `
		UPDATE admin_invitations SET revoked_at = ?
		WHERE id = ? AND consumed_at IS NULL AND revoked_at IS NULL AND expires_at > ?`,
		now.UTC(), invitationID, now.UTC())
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrInvitationUsed
	}
	return nil
}

func (repository *MySQLRepository) CompleteInvitation(
	ctx context.Context,
	hash [32]byte,
	expected Invitation,
	candidate User,
	credential webauthn.Credential,
	now time.Time,
) (User, error) {
	transaction, err := repository.database.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer func() { _ = transaction.Rollback() }()
	kind, targetUserID, err := invitationIdentity(ctx, transaction, hash)
	if err != nil {
		return User{}, err
	}
	if kind == InvitationKindRecovery {
		var lockedUserID string
		var lockedHandle []byte
		var lockedStatus UserStatus
		if err := transaction.QueryRowContext(ctx, `
			SELECT id, webauthn_id, status FROM admin_users WHERE id = ? FOR UPDATE`, targetUserID).
			Scan(&lockedUserID, &lockedHandle, &lockedStatus); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return User{}, ErrInvitationInvalid
			}
			return User{}, err
		}
		if lockedStatus != UserStatusActive || candidate.ID != lockedUserID ||
			!slices.Equal(candidate.Handle, lockedHandle) {
			return User{}, ErrInvitationInvalid
		}
	}
	stored, err := lockedInvitation(ctx, transaction, hash, now)
	if err != nil {
		return User{}, err
	}
	if stored.ID != expected.ID || stored.Kind != expected.Kind || stored.TargetUserID != expected.TargetUserID {
		return User{}, ErrInvitationInvalid
	}
	stored.AppIDs, err = invitationAppIDs(ctx, transaction, stored.ID)
	if err != nil {
		return User{}, err
	}
	if stored.DisplayName == "" || !stored.Role.Valid() ||
		stored.Role == RoleOperator && len(stored.AppIDs) == 0 {
		return User{}, ErrInvitationInvalid
	}
	var user User
	switch stored.Kind {
	case InvitationKindCreate:
		if candidate.ID == "" || len(candidate.Handle) == 0 {
			return User{}, ErrInvitationInvalid
		}
		if _, err := transaction.ExecContext(ctx, `
			INSERT INTO admin_users
				(id, webauthn_id, display_name, role, status, session_version, created_at, updated_at)
			VALUES (?, ?, ?, ?, 'active', 1, ?, ?)`, candidate.ID, candidate.Handle,
			stored.DisplayName, stored.Role, now.UTC(), now.UTC()); err != nil {
			return User{}, err
		}
		if err := replaceUserApps(ctx, transaction, candidate.ID, stored.Role, stored.AppIDs); err != nil {
			return User{}, err
		}
		if err := insertCredential(ctx, transaction, candidate.ID, "主通行密钥", credential); err != nil {
			return User{}, credentialInsertError(err)
		}
		user, _, err = repository.user(ctx, transaction, "id = ?", candidate.ID)
	case InvitationKindRecovery:
		user.ID = stored.TargetUserID
		_, err = transaction.ExecContext(ctx, `DELETE FROM admin_webauthn_credentials WHERE admin_user_id = ?`, user.ID)
		if err == nil {
			err = credentialInsertError(insertCredential(ctx, transaction, user.ID, "恢复通行密钥", credential))
		}
		if err == nil {
			_, err = transaction.ExecContext(ctx, `
				UPDATE admin_users SET session_version = session_version + 1, updated_at = ? WHERE id = ?`, now.UTC(), user.ID)
		}
		if err == nil {
			user, _, err = repository.user(ctx, transaction, "id = ?", user.ID)
		}
	default:
		err = ErrInvitationInvalid
	}
	if err != nil {
		return User{}, err
	}
	if _, err := transaction.ExecContext(ctx, `
		UPDATE admin_invitations SET consumed_at = ? WHERE id = ?`, now.UTC(), stored.ID); err != nil {
		return User{}, err
	}
	if stored.Kind == InvitationKindRecovery {
		if _, err := transaction.ExecContext(ctx, `
			UPDATE admin_invitations SET revoked_at = ?
			WHERE kind = 'recovery' AND target_admin_user_id = ? AND id <> ?
				AND consumed_at IS NULL AND revoked_at IS NULL`, now.UTC(), user.ID, stored.ID); err != nil {
			return User{}, err
		}
	}
	if err := transaction.Commit(); err != nil {
		return User{}, err
	}
	return user, nil
}

func invitationIdentity(ctx context.Context, transaction *sql.Tx, hash [32]byte) (InvitationKind, string, error) {
	var kind InvitationKind
	var targetID sql.NullString
	err := transaction.QueryRowContext(ctx, `
		SELECT kind, target_admin_user_id FROM admin_invitations WHERE token_hash = ?`, hash[:]).
		Scan(&kind, &targetID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrInvitationInvalid
	}
	if err != nil {
		return "", "", err
	}
	return kind, targetID.String, nil
}

func lockedInvitation(ctx context.Context, transaction *sql.Tx, hash [32]byte, now time.Time) (Invitation, error) {
	var invitation Invitation
	var targetID, actorID sql.NullString
	var consumedAt, revokedAt sql.NullTime
	err := transaction.QueryRowContext(ctx, `
		SELECT id, kind, target_admin_user_id, invited_by_admin_user_id,
			display_name, role, expires_at, created_at, consumed_at, revoked_at
		FROM admin_invitations WHERE token_hash = ? FOR UPDATE`, hash[:]).
		Scan(&invitation.ID, &invitation.Kind, &targetID, &actorID, &invitation.DisplayName,
			&invitation.Role, &invitation.ExpiresAt, &invitation.CreatedAt, &consumedAt, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Invitation{}, ErrInvitationInvalid
	}
	if err != nil {
		return Invitation{}, err
	}
	if consumedAt.Valid || revokedAt.Valid || !now.UTC().Before(invitation.ExpiresAt.UTC()) {
		return Invitation{}, ErrInvitationInvalid
	}
	invitation.TargetUserID = targetID.String
	invitation.InvitedByID = actorID.String
	return invitation, nil
}

func (repository *MySQLRepository) AddCredential(
	ctx context.Context,
	userID string,
	displayName string,
	credential webauthn.Credential,
	expectedSessionVersion uint64,
) error {
	transaction, err := repository.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	var status UserStatus
	var sessionVersion uint64
	if err := transaction.QueryRowContext(ctx, `
		SELECT status, session_version FROM admin_users WHERE id = ? FOR UPDATE`, userID).
		Scan(&status, &sessionVersion); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUserNotFound
		}
		return err
	}
	if status != UserStatusActive || sessionVersion != expectedSessionVersion {
		return ErrAuthorizationChanged
	}
	if err := credentialInsertError(insertCredential(ctx, transaction, userID, displayName, credential)); err != nil {
		return err
	}
	return transaction.Commit()
}

func credentialInsertError(err error) error {
	var mysqlError *mysqldriver.MySQLError
	if errors.As(err, &mysqlError) && mysqlError.Number == 1062 {
		return ErrCredentialExists
	}
	return err
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

func (repository *MySQLRepository) ListCredentials(ctx context.Context, userID string) ([]CredentialSummary, error) {
	rows, err := repository.database.QueryContext(ctx, `
		SELECT credential_id, display_name, created_at, last_used_at
		FROM admin_webauthn_credentials WHERE admin_user_id = ? ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	summaries := make([]CredentialSummary, 0)
	for rows.Next() {
		var rawID []byte
		var summary CredentialSummary
		var lastUsed sql.NullTime
		if err := rows.Scan(&rawID, &summary.DisplayName, &summary.CreatedAt, &lastUsed); err != nil {
			return nil, err
		}
		summary.ID = base64.RawURLEncoding.EncodeToString(rawID)
		if lastUsed.Valid {
			value := lastUsed.Time
			summary.LastUsedAt = &value
		}
		summaries = append(summaries, summary)
	}
	return summaries, rows.Err()
}

func (repository *MySQLRepository) DeleteCredential(ctx context.Context, userID string, credentialID []byte) error {
	transaction, err := repository.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	var lockedUserID string
	if err := transaction.QueryRowContext(ctx, `
		SELECT id FROM admin_users WHERE id = ? FOR UPDATE`, userID).Scan(&lockedUserID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUserNotFound
		}
		return err
	}
	rows, err := transaction.QueryContext(ctx, `
		SELECT credential_id FROM admin_webauthn_credentials
		WHERE admin_user_id = ? FOR UPDATE`, userID)
	if err != nil {
		return err
	}
	credentialIDs := make([][]byte, 0, 2)
	for rows.Next() {
		var value []byte
		if err := rows.Scan(&value); err != nil {
			_ = rows.Close()
			return err
		}
		credentialIDs = append(credentialIDs, value)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	credentialFound := slices.ContainsFunc(credentialIDs, func(value []byte) bool {
		return slices.Equal(value, credentialID)
	})
	if !credentialFound {
		return ErrCredentialNotFound
	}
	if len(credentialIDs) == 1 {
		return ErrLastCredential
	}
	result, err := transaction.ExecContext(ctx, `
		DELETE FROM admin_webauthn_credentials WHERE admin_user_id = ? AND credential_id = ?`, userID, credentialID)
	if err != nil {
		return err
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if deleted != 1 {
		return ErrCredentialNotFound
	}
	if _, err := transaction.ExecContext(ctx, `
		UPDATE admin_users SET session_version = session_version + 1, updated_at = CURRENT_TIMESTAMP(6)
		WHERE id = ?`, userID); err != nil {
		return err
	}
	return transaction.Commit()
}

func (repository *MySQLRepository) AppendAudit(ctx context.Context, event AuditEvent) error {
	metadata, err := json.Marshal(event.Metadata)
	if err != nil {
		return err
	}
	var userID, appID any
	if event.UserID != "" {
		userID = event.UserID
	}
	if event.AppID != "" {
		appID = event.AppID
	}
	_, err = repository.database.ExecContext(ctx, `
		INSERT INTO admin_audit_events
			(admin_user_id, app_id, request_id, action, target_type, target_id, outcome, metadata_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, userID, appID, event.RequestID, event.Action,
		event.TargetType, event.TargetID, event.Outcome, metadata)
	return err
}

func (repository *MySQLRepository) ListAudit(
	ctx context.Context,
	userID string,
	all bool,
	limit int,
) ([]AuditRecord, error) {
	if limit < 1 || limit > 200 {
		limit = 100
	}
	query := `
		SELECT e.id, e.admin_user_id, u.display_name, e.app_id, e.action,
			e.target_type, e.target_id, e.outcome, e.metadata_json, e.created_at
		FROM admin_audit_events e
		LEFT JOIN admin_users u ON u.id = e.admin_user_id`
	arguments := []any{}
	if !all {
		query += ` WHERE e.admin_user_id = ?`
		arguments = append(arguments, userID)
	}
	query += ` ORDER BY e.id DESC LIMIT ?`
	arguments = append(arguments, limit)
	rows, err := repository.database.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := make([]AuditRecord, 0)
	for rows.Next() {
		var record AuditRecord
		var storedUserID, displayName, appID sql.NullString
		var metadata []byte
		if err := rows.Scan(&record.ID, &storedUserID, &displayName, &appID, &record.Action,
			&record.TargetType, &record.TargetID, &record.Outcome, &metadata, &record.CreatedAt); err != nil {
			return nil, err
		}
		record.UserID, record.DisplayName, record.AppID = storedUserID.String, displayName.String, appID.String
		if err := json.Unmarshal(metadata, &record.Metadata); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

type adminQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type adminExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

type adminQueryExecer interface {
	adminQueryer
	adminExecer
}

func appIDs(ctx context.Context, queryer adminQueryer, userID string) ([]string, error) {
	rows, err := queryer.QueryContext(ctx, `
		SELECT app_id FROM admin_user_apps WHERE admin_user_id = ? ORDER BY app_id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]string, 0)
	for rows.Next() {
		var appID string
		if err := rows.Scan(&appID); err != nil {
			return nil, err
		}
		result = append(result, appID)
	}
	return result, rows.Err()
}

func invitationAppIDs(ctx context.Context, queryer adminQueryer, invitationID string) ([]string, error) {
	rows, err := queryer.QueryContext(ctx, `
		SELECT app_id FROM admin_invitation_apps WHERE invitation_id = ? ORDER BY app_id`, invitationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]string, 0)
	for rows.Next() {
		var appID string
		if err := rows.Scan(&appID); err != nil {
			return nil, err
		}
		result = append(result, appID)
	}
	return result, rows.Err()
}

func credentials(ctx context.Context, queryer adminQueryer, userID string) ([]webauthn.Credential, error) {
	rows, err := queryer.QueryContext(ctx, `
		SELECT credential_json FROM admin_webauthn_credentials
		WHERE admin_user_id = ? ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]webauthn.Credential, 0)
	for rows.Next() {
		var encoded []byte
		if err := rows.Scan(&encoded); err != nil {
			return nil, err
		}
		var credential webauthn.Credential
		if err := json.Unmarshal(encoded, &credential); err != nil {
			return nil, err
		}
		result = append(result, credential)
	}
	return result, rows.Err()
}

func replaceUserApps(ctx context.Context, database adminQueryExecer, userID string, role Role, appIDs []string) error {
	if _, err := database.ExecContext(ctx, `DELETE FROM admin_user_apps WHERE admin_user_id = ?`, userID); err != nil {
		return err
	}
	if role == RoleAdmin {
		return nil
	}
	for _, appID := range appIDs {
		if _, err := database.ExecContext(ctx, `
			INSERT INTO admin_user_apps (admin_user_id, app_id) VALUES (?, ?)`, userID, appID); err != nil {
			return err
		}
	}
	return nil
}

func insertInvitationApps(ctx context.Context, execer adminExecer, invitationID string, role Role, appIDs []string) error {
	if role == RoleAdmin {
		return nil
	}
	for _, appID := range appIDs {
		if _, err := execer.ExecContext(ctx, `
			INSERT INTO admin_invitation_apps (invitation_id, app_id) VALUES (?, ?)`, invitationID, appID); err != nil {
			return err
		}
	}
	return nil
}

func insertCredential(
	ctx context.Context,
	execer adminExecer,
	userID string,
	displayName string,
	credential webauthn.Credential,
) error {
	encoded, err := json.Marshal(credential)
	if err != nil {
		return err
	}
	if len(credential.ID) == 0 {
		return fmt.Errorf("credential id is empty")
	}
	_, err = execer.ExecContext(ctx, `
		INSERT INTO admin_webauthn_credentials
			(credential_id, admin_user_id, display_name, credential_json)
		VALUES (?, ?, ?, ?)`, credential.ID, userID, displayName, encoded)
	return err
}

var _ Repository = (*MySQLRepository)(nil)
