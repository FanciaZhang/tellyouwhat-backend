package albums

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type uploadGrantAuthorizer interface {
	WithUploadGrant(context.Context, Upload, string, string, time.Time, time.Time, func(time.Time) error) error
}

func authorizeUploadGrant(ctx context.Context, repository any, u Upload, resource, provider string, now func() time.Time, expiry time.Time, sign func(time.Time) error) error {
	authorize := func(deadline time.Time) error {
		if deadline.After(expiry) || !now().Before(deadline) {
			return ErrExpired
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := sign(deadline); err != nil {
			return err
		}
		if !now().Before(deadline) {
			return ErrExpired
		}
		return ctx.Err()
	}
	if r, ok := repository.(uploadGrantAuthorizer); ok {
		return r.WithUploadGrant(ctx, u, resource, provider, now().UTC(), expiry, authorize)
	}
	// In-memory protocol fixtures retain their existing subscription adapter.
	deadline, err := cloudMediaGrantExpiry(ctx, repository, u.OwnerID, now().UTC(), expiry)
	if err != nil {
		return err
	}
	return authorize(deadline)
}

// The callback only signs locally. Holding the storage lock makes grant issuance
// precede deletion acceptance or fail after it; no provider I/O belongs here.
func (r *MySQLUploads) WithUploadGrant(ctx context.Context, expected Upload, resource, provider string, now, expiry time.Time, authorize func(time.Time) error) error {
	if !validOwner(expected.OwnerID) || !validOwner(expected.ID) || now.IsZero() || !expiry.After(now) || expiry.After(now.Add(10*time.Minute)) || authorize == nil ||
		(resource == "") != (provider == "") || resource != "" && (!resourceIDPattern.MatchString(resource) || !validCOSMultipartID(provider)) {
		return ErrInvalidManifest
	}
	currentTime := func() time.Time {
		if r.billingNow != nil {
			return maxTime(now, r.billingNow().UTC())
		}
		return now
	}
	_, err := r.mutate(ctx, expected.OwnerID, expected.ID, func(tx *sql.Tx, u Upload) (Upload, error) {
		if u.State != Uploading || u.Digest != expected.Digest || u.LifecycleEpoch != expected.LifecycleEpoch {
			return u, ErrConflict
		}
		if err := requireActiveUpload(ctx, tx, u); err != nil {
			return u, err
		}
		if resource != "" {
			if err := checkMultipartParent(u, resource, currentTime()); err != nil {
				return u, err
			}
			intent, err := lockMultipart(ctx, tx, u.ID, resource)
			if err != nil {
				return u, err
			}
			if readyMultipart(intent, u.ID, resource) != nil || intent.ProviderUploadID != provider {
				return u, ErrConflict
			}
			completion, err := scanMultipartCompletion(tx.QueryRowContext(ctx, `SELECT upload_id,resource_id,operation_id,state,object_version FROM album_multipart_completions WHERE upload_id=? AND resource_id=? FOR UPDATE`, u.ID, resource))
			if err != nil && !errors.Is(err, ErrNotFound) {
				return u, err
			}
			if err == nil && completion.State != "retryable" {
				return u, ErrConflict
			}
		}
		expiry = minTime(expiry, u.ExpiresAt)
		if r.billingEnvironment != "" {
			account, err := r.cloudAccountInTransaction(ctx, tx, u.OwnerID, currentTime())
			if err != nil {
				return u, err
			}
			deadline, err := cloudAccountMediaDeadline(account)
			if err != nil {
				return u, err
			}
			expiry = minTime(expiry, deadline)
		}
		if !currentTime().Before(expiry) {
			return u, ErrExpired
		}
		if err := authorize(expiry); err != nil {
			return u, err
		}
		if !currentTime().Before(expiry) {
			return u, ErrExpired
		}
		return u, nil
	})
	return err
}
