package mysqlstore

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/tellyouwhat/backend/internal/media"
)

type MediaRepository struct {
	database *sql.DB
	appID    string
}

func NewMediaRepository(database *sql.DB, appID string) *MediaRepository {
	return &MediaRepository{database: database, appID: appID}
}

func (repository *MediaRepository) Register(ctx context.Context, record media.Record) error {
	count, err := affectedRows(repository.database.ExecContext(ctx, `
        INSERT INTO media_objects
			(app_id, object_id, owner_key_id, owner_device_id, request_id, operation, media_id,
             kind, sha256, size_bytes, mime_type, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON DUPLICATE KEY UPDATE object_id = VALUES(object_id)`,
		repository.appID, record.ObjectID, record.OwnerKeyID, record.OwnerDeviceID, record.RequestID,
		record.Operation, record.MediaID, record.Kind, record.SHA256, record.SizeBytes,
		record.MIMEType, record.ExpiresAt,
	))
	if err != nil {
		return err
	}
	if count == 1 {
		return nil
	}
	existing, err := repository.Get(ctx, record.ObjectID)
	if err != nil || !sameMediaAuthorization(existing, record) || existing.DeletedAt != nil || existing.ConsumedAt != nil {
		return media.ErrAuthorizationConflict
	}
	return nil
}

func (repository *MediaRepository) Get(ctx context.Context, objectID string) (media.Record, error) {
	record, err := scanMediaRecord(repository.database.QueryRowContext(ctx, mediaSelect+` WHERE app_id = ? AND object_id = ?`, repository.appID, objectID))
	if errors.Is(err, sql.ErrNoRows) {
		return media.Record{}, media.ErrNotAuthorized
	}
	return record, err
}

func (repository *MediaRepository) CommitAttempt(ctx context.Context, expected []media.Record, attempt media.AttemptRecord, now time.Time) (media.AttemptRecord, bool, error) {
	if attempt.RequestID == "" || attempt.OwnerKeyID == "" || attempt.BodyDigest == "" {
		return media.AttemptRecord{}, false, media.ErrIdempotencyConflict
	}
	objectIDs := make([]string, 0, len(expected))
	unique := make(map[string]struct{}, len(expected))
	for _, authorization := range expected {
		if authorization.ObjectID == "" {
			return media.AttemptRecord{}, false, media.ErrNotAuthorized
		}
		objectIDs = append(objectIDs, authorization.ObjectID)
		unique[authorization.ObjectID] = struct{}{}
	}
	if len(unique) != len(expected) {
		return media.AttemptRecord{}, false, media.ErrNotAuthorized
	}
	transaction, err := repository.database.BeginTx(ctx, nil)
	if err != nil {
		return media.AttemptRecord{}, false, err
	}
	defer func() { _ = transaction.Rollback() }()
	count, err := affectedRows(transaction.ExecContext(ctx, `
		INSERT INTO idempotency_records (app_id, request_id, owner_key_id, body_digest, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
        ON DUPLICATE KEY UPDATE request_id = VALUES(request_id)`,
		repository.appID, attempt.RequestID, attempt.OwnerKeyID, attempt.BodyDigest, attempt.ExpiresAt, attempt.CreatedAt,
	))
	if err != nil {
		return media.AttemptRecord{}, false, err
	}
	if count != 1 {
		var ownerKeyID, bodyDigest string
		var expiresAt, createdAt time.Time
		err := transaction.QueryRowContext(ctx, `
            SELECT owner_key_id, body_digest, expires_at, created_at
			FROM idempotency_records WHERE app_id = ? AND request_id = ? FOR UPDATE`, repository.appID, attempt.RequestID).Scan(
			&ownerKeyID, &bodyDigest, &expiresAt, &createdAt,
		)
		if err != nil {
			return media.AttemptRecord{}, false, err
		}
		if !now.Before(expiresAt) {
			if _, err := transaction.ExecContext(ctx, `
                UPDATE idempotency_records
                SET owner_key_id = ?, body_digest = ?, expires_at = ?, created_at = ?
				WHERE app_id = ? AND request_id = ?`,
				attempt.OwnerKeyID, attempt.BodyDigest, attempt.ExpiresAt, attempt.CreatedAt, repository.appID, attempt.RequestID,
			); err != nil {
				return media.AttemptRecord{}, false, err
			}
		} else if ownerKeyID == attempt.OwnerKeyID && bodyDigest == attempt.BodyDigest {
			return media.AttemptRecord{
				RequestID: attempt.RequestID, OwnerKeyID: ownerKeyID, BodyDigest: bodyDigest,
				ExpiresAt: expiresAt, CreatedAt: createdAt,
			}, true, nil
		} else {
			return media.AttemptRecord{}, false, media.ErrIdempotencyConflict
		}
	}
	if len(expected) > 0 {
		arguments := make([]any, 0, len(objectIDs)+2)
		arguments = append(arguments, repository.appID)
		for _, objectID := range objectIDs {
			arguments = append(arguments, objectID)
		}
		arguments = append(arguments, now)
		rows, err := transaction.QueryContext(ctx, mediaSelect+`
			WHERE app_id = ? AND object_id IN (`+placeholders(len(objectIDs))+`)
              AND consumed_at IS NULL AND deleted_at IS NULL AND expires_at > ?
            FOR UPDATE`, arguments...)
		if err != nil {
			return media.AttemptRecord{}, false, err
		}
		actual := make(map[string]media.Record, len(expected))
		for rows.Next() {
			record, scanErr := scanMediaRecord(rows)
			if scanErr != nil {
				rows.Close()
				return media.AttemptRecord{}, false, scanErr
			}
			actual[record.ObjectID] = record
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return media.AttemptRecord{}, false, err
		}
		if len(actual) != len(expected) {
			return media.AttemptRecord{}, false, media.ErrNotAuthorized
		}
		for _, authorization := range expected {
			if !sameMediaAuthorization(actual[authorization.ObjectID], authorization) {
				return media.AttemptRecord{}, false, media.ErrNotAuthorized
			}
		}
	}
	if len(objectIDs) > 0 {
		arguments := make([]any, 0, len(objectIDs)+2)
		arguments = append(arguments, now)
		arguments = append(arguments, repository.appID)
		for _, objectID := range objectIDs {
			arguments = append(arguments, objectID)
		}
		count, err = affectedRows(transaction.ExecContext(ctx,
			`UPDATE media_objects SET consumed_at = ? WHERE app_id = ? AND object_id IN (`+placeholders(len(objectIDs))+`)`,
			arguments...,
		))
		if err != nil {
			return media.AttemptRecord{}, false, err
		}
		if count != int64(len(objectIDs)) {
			return media.AttemptRecord{}, false, media.ErrNotAuthorized
		}
	}
	if err := transaction.Commit(); err != nil {
		return media.AttemptRecord{}, false, err
	}
	return attempt, false, nil
}

const mediaSelect = `
    SELECT object_id, owner_key_id, owner_device_id, request_id, operation,
           media_id, kind, mime_type, sha256, size_bytes, expires_at, deleted_at, consumed_at
    FROM media_objects`

func scanMediaRecord(row rowScanner) (media.Record, error) {
	var record media.Record
	var deletedAt, consumedAt sql.NullTime
	err := row.Scan(
		&record.ObjectID, &record.OwnerKeyID, &record.OwnerDeviceID, &record.RequestID,
		&record.Operation, &record.MediaID, &record.Kind, &record.MIMEType,
		&record.SHA256, &record.SizeBytes, &record.ExpiresAt, &deletedAt, &consumedAt,
	)
	if err != nil {
		return media.Record{}, err
	}
	if deletedAt.Valid {
		record.DeletedAt = &deletedAt.Time
	}
	if consumedAt.Valid {
		record.ConsumedAt = &consumedAt.Time
	}
	return record, nil
}

func placeholders(count int) string {
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

func sameMediaAuthorization(left, right media.Record) bool {
	return left.ObjectID == right.ObjectID && left.OwnerKeyID == right.OwnerKeyID &&
		left.OwnerDeviceID == right.OwnerDeviceID && left.RequestID == right.RequestID &&
		left.Operation == right.Operation && left.MediaID == right.MediaID &&
		left.Kind == right.Kind && left.MIMEType == right.MIMEType && left.SHA256 == right.SHA256 &&
		left.SizeBytes == right.SizeBytes
}

var _ media.Registry = (*MediaRepository)(nil)
