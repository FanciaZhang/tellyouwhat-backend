package albums

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
)

// MultipartIntent is internal control state, not a client-supplied upload handle.
// Ownership, object key and byte geometry always come from the parent manifest.
type MultipartIntent struct {
	UploadID, ResourceID, IntentID string
	State                          string
	ProviderUploadID               string
}

func scanMultipart(row rowScanner) (MultipartIntent, error) {
	var result MultipartIntent
	var provider sql.NullString
	err := row.Scan(&result.UploadID, &result.ResourceID, &result.IntentID, &result.State, &provider)
	if errors.Is(err, sql.ErrNoRows) {
		return result, ErrNotFound
	}
	result.ProviderUploadID = provider.String
	return result, err
}

func lockMultipart(ctx context.Context, tx *sql.Tx, uploadID, resourceID string) (MultipartIntent, error) {
	return scanMultipart(tx.QueryRowContext(ctx, `SELECT upload_id,resource_id,intent_id,state,provider_upload_id
 FROM album_multipart_intents WHERE upload_id=? AND resource_id=? FOR UPDATE`, uploadID, resourceID))
}

func checkMultipartParent(u Upload, resourceID string, now time.Time) error {
	if !now.Before(u.ExpiresAt) {
		return ErrExpired
	}
	if u.State != Uploading {
		return ErrConflict
	}
	for _, resource := range u.Manifest.Resources {
		if resource.ID == resourceID {
			return nil
		}
	}
	return ErrNotFound
}

// BeginMultipart persists intent before contacting COS. Only created=true grants
// permission to initiate once. Replays (including after a lost commit response)
// return false; they must reconcile, never initiate another provider session.
func (r *MySQLUploads) BeginMultipart(ctx context.Context, owner, uploadID, resourceID string, now time.Time) (MultipartIntent, bool, error) {
	if !validOwner(owner) {
		return MultipartIntent{}, false, ErrOwner
	}
	if !validOwner(uploadID) || !resourceIDPattern.MatchString(resourceID) {
		return MultipartIntent{}, false, ErrNotFound
	}
	var intent MultipartIntent
	created := false
	_, err := r.mutate(ctx, owner, uploadID, func(tx *sql.Tx, u Upload) (Upload, error) {
		if err := requireActiveUpload(ctx, tx, u); err != nil {
			return u, err
		}
		if err := checkMultipartParent(u, resourceID, now); err != nil {
			return u, err
		}
		old, err := lockMultipart(ctx, tx, u.ID, resourceID)
		if err == nil {
			intent = old
			return u, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return u, err
		}
		intent = MultipartIntent{UploadID: u.ID, ResourceID: resourceID, IntentID: uuid.NewString(), State: "initiating"}
		_, err = tx.ExecContext(ctx, `INSERT INTO album_multipart_intents(upload_id,resource_id,intent_id,state,created_at,updated_at) VALUES (?,?,?,'initiating',?,?)`, u.ID, resourceID, intent.IntentID, now, now)
		created = err == nil
		return u, err
	})
	if err != nil {
		return MultipartIntent{}, false, err
	}
	return intent, created, nil
}

// ResolveMultipart records a provider response for the original durable intent.
// Empty providerID marks an unknown outcome. A late valid response may resolve
// unknown; a ready binding may never be replaced by a different provider ID.
func (r *MySQLUploads) ResolveMultipart(ctx context.Context, owner, uploadID, resourceID, intentID, providerID string, now time.Time) (MultipartIntent, error) {
	if !validOwner(owner) {
		return MultipartIntent{}, ErrOwner
	}
	if !validOwner(uploadID) || !resourceIDPattern.MatchString(resourceID) || !validOwner(intentID) || (providerID != "" && !validCOSMultipartID(providerID)) {
		return MultipartIntent{}, ErrInvalidManifest
	}
	var result MultipartIntent
	_, err := r.mutate(ctx, owner, uploadID, func(tx *sql.Tx, u Upload) (Upload, error) {
		// Persist late provider observations even after expiry/submit, so cleanup can
		// find them. Grant issuance must independently require an active parent.
		current, err := lockMultipart(ctx, tx, u.ID, resourceID)
		if err != nil {
			return u, err
		}
		if current.IntentID != intentID {
			return u, ErrConflict
		}
		if current.State == "ready" {
			if providerID != current.ProviderUploadID {
				return u, ErrConflict
			}
			result = current
			return u, nil
		}
		state := "unknown"
		var provider any
		if providerID != "" {
			state = "ready"
			provider = providerID
		}
		_, err = tx.ExecContext(ctx, `UPDATE album_multipart_intents SET state=?,provider_upload_id=?,updated_at=? WHERE upload_id=? AND resource_id=? AND intent_id=?`, state, provider, now, u.ID, resourceID, intentID)
		current.State, current.ProviderUploadID = state, providerID
		result = current
		return u, err
	})
	if err != nil {
		return MultipartIntent{}, err
	}
	return result, nil
}

// Read-only lookup cannot accidentally acquire initialization permission.
func (r *MySQLUploads) GetMultipart(ctx context.Context, owner, uploadID, resourceID string) (MultipartIntent, error) {
	if !validOwner(owner) {
		return MultipartIntent{}, ErrOwner
	}
	if !validOwner(uploadID) || !resourceIDPattern.MatchString(resourceID) {
		return MultipartIntent{}, ErrNotFound
	}
	return scanMultipart(r.db.QueryRowContext(ctx, `SELECT m.upload_id,m.resource_id,m.intent_id,m.state,m.provider_upload_id
 FROM album_multipart_intents m JOIN album_uploads u ON u.upload_id=m.upload_id
 WHERE u.owner_id=? AND m.upload_id=? AND m.resource_id=?`, owner, uploadID, resourceID))
}
