package albums

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

type MultipartCompletion struct {
	UploadID, ResourceID, OperationID, State, ObjectVersion string
}

func scanMultipartCompletion(row rowScanner) (MultipartCompletion, error) {
	var value MultipartCompletion
	var version sql.NullString
	err := row.Scan(&value.UploadID, &value.ResourceID, &value.OperationID, &value.State, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return value, ErrNotFound
	}
	value.ObjectVersion = version.String
	return value, err
}

func validMultipartVersion(version string) bool {
	return concreteCOSVersion(version) && len(version) <= 1024 && !strings.ContainsRune(version, 0)
}

// ClaimMultipartCompletion only permits one external attempt. A retry requires
// an explicit retryable result proving no external completion was attempted.
func (r *MySQLUploads) ClaimMultipartCompletion(ctx context.Context, owner, uploadID, resourceID string, now time.Time) (MultipartCompletion, bool, error) {
	if !validOwner(owner) {
		return MultipartCompletion{}, false, ErrOwner
	}
	if !validOwner(uploadID) || !resourceIDPattern.MatchString(resourceID) {
		return MultipartCompletion{}, false, ErrNotFound
	}
	var result MultipartCompletion
	claimed := false
	_, err := r.mutate(ctx, owner, uploadID, func(tx *sql.Tx, u Upload) (Upload, error) {
		if err := requireActiveUpload(ctx, tx, u); err != nil {
			return u, err
		}
		if err := checkMultipartParent(u, resourceID, now); err != nil {
			return u, err
		}
		intent, err := lockMultipart(ctx, tx, u.ID, resourceID)
		if err != nil {
			return u, err
		}
		if err := readyMultipart(intent, u.ID, resourceID); err != nil {
			return u, err
		}
		old, err := scanMultipartCompletion(tx.QueryRowContext(ctx, `SELECT upload_id,resource_id,operation_id,state,object_version FROM album_multipart_completions WHERE upload_id=? AND resource_id=? FOR UPDATE`, u.ID, resourceID))
		if err == nil && old.State != "retryable" {
			result = old
			return u, nil
		}
		if err != nil && !errors.Is(err, ErrNotFound) {
			return u, err
		}
		result = MultipartCompletion{UploadID: u.ID, ResourceID: resourceID, OperationID: uuid.NewString(), State: "completing"}
		if errors.Is(err, ErrNotFound) {
			_, err = tx.ExecContext(ctx, `INSERT INTO album_multipart_completions(upload_id,resource_id,operation_id,state,updated_at) VALUES (?,?,?,'completing',?)`, u.ID, resourceID, result.OperationID, now)
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE album_multipart_completions SET operation_id=?,state='completing',object_version=NULL,updated_at=? WHERE upload_id=? AND resource_id=?`, result.OperationID, now, u.ID, resourceID)
		}
		claimed = err == nil
		return u, err
	})
	if err != nil {
		return MultipartCompletion{}, false, err
	}
	return result, claimed, nil
}

// ResolveMultipartCompletion accepts trusted worker observations, never client
// receipts. A stale operation cannot overwrite a retry's newer observation.
func (r *MySQLUploads) ResolveMultipartCompletion(ctx context.Context, owner, uploadID, resourceID, operation, state, version string, now time.Time) (MultipartCompletion, error) {
	if !validOwner(owner) {
		return MultipartCompletion{}, ErrOwner
	}
	if !validOwner(uploadID) || !resourceIDPattern.MatchString(resourceID) || !validOwner(operation) {
		return MultipartCompletion{}, ErrInvalidManifest
	}
	if (state != "assembled" && state != "unknown" && state != "retryable") || (state == "assembled" && !validMultipartVersion(version)) || (state != "assembled" && version != "") {
		return MultipartCompletion{}, ErrInvalidManifest
	}
	var result MultipartCompletion
	_, err := r.mutate(ctx, owner, uploadID, func(tx *sql.Tx, u Upload) (Upload, error) {
		old, err := scanMultipartCompletion(tx.QueryRowContext(ctx, `SELECT upload_id,resource_id,operation_id,state,object_version FROM album_multipart_completions WHERE upload_id=? AND resource_id=? FOR UPDATE`, u.ID, resourceID))
		if err != nil {
			return u, err
		}
		if old.OperationID != operation {
			return u, ErrConflict
		}
		if old.State == state && old.ObjectVersion == version {
			result = old
			return u, nil
		}
		// Unknown can only advance when an actual version has been reconciled.
		if old.State != "completing" && !(old.State == "unknown" && state == "assembled") {
			return u, ErrConflict
		}
		var stored any
		if version != "" {
			stored = version
		}
		_, err = tx.ExecContext(ctx, `UPDATE album_multipart_completions SET state=?,object_version=?,updated_at=? WHERE upload_id=? AND resource_id=? AND operation_id=?`, state, stored, now, u.ID, resourceID, operation)
		old.State, old.ObjectVersion = state, version
		result = old
		return u, err
	})
	if err != nil {
		return MultipartCompletion{}, err
	}
	return result, nil
}

func (r *MySQLUploads) GetMultipartCompletion(ctx context.Context, owner, uploadID, resourceID string) (MultipartCompletion, error) {
	if !validOwner(owner) {
		return MultipartCompletion{}, ErrOwner
	}
	if !validOwner(uploadID) || !resourceIDPattern.MatchString(resourceID) {
		return MultipartCompletion{}, ErrNotFound
	}
	return scanMultipartCompletion(r.db.QueryRowContext(ctx, `SELECT c.upload_id,c.resource_id,c.operation_id,c.state,c.object_version
 FROM album_multipart_completions c JOIN album_uploads u ON u.upload_id=c.upload_id
 WHERE u.owner_id=? AND c.upload_id=? AND c.resource_id=?`, owner, uploadID, resourceID))
}
