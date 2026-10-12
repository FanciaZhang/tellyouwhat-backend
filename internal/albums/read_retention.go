package albums

import (
	"context"
	"errors"
	"time"
)

type cloudReadGrantAuthorizer interface {
	WithCloudReadGrant(context.Context, string, AssetVersion, time.Time, time.Time, func(time.Time) error) error
}

// All direct media reads share the retention boundary, independently of upload
// quota/freshness: an expired subscriber must still be able to export originals.
func (s *UploadService) authorizeLibraryRead(ctx context.Context, owner string, version AssetVersion, sign func(time.Time) error) (time.Time, error) {
	now := s.now().UTC()
	expiry := now.Add(5 * time.Minute)
	authorize := func(deadline time.Time) error {
		if deadline.After(expiry) || !s.now().UTC().Before(deadline) {
			return ErrNotFound
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := sign(deadline); err != nil {
			return err
		}
		if !s.now().UTC().Before(deadline) {
			return ErrNotFound
		}
		expiry = deadline
		return nil
	}
	var err error
	if repository, ok := s.repository.(cloudReadGrantAuthorizer); ok {
		err = repository.WithCloudReadGrant(ctx, owner, version, now, expiry, authorize)
	} else {
		err = authorize(expiry)
	}
	if err != nil {
		return time.Time{}, err
	}
	return expiry, nil
}

// Local URL signing runs while the storage account is locked, matching sharing
// and entitlement changes. The callback must not query the DB or contact COS.
func (r *MySQLUploads) WithCloudReadGrant(ctx context.Context, owner string, expected AssetVersion, now, expiry time.Time, authorize func(time.Time) error) error {
	if !validOwner(owner) || !validOwner(expected.VersionID) || now.IsZero() || !now.Before(expiry) || expiry.After(now.Add(5*time.Minute)) || authorize == nil {
		return ErrInvalidManifest
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if r.billingEnvironment != "" {
		deadline, err := r.shareRetentionInTransaction(ctx, tx, owner, now)
		if errors.Is(err, ErrShareNotAvailable) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		expiry = minTime(expiry, deadline)
	} else if _, err := lockAccount(ctx, tx, owner); err != nil {
		return err
	}
	current, err := scanAssetVersion(tx.QueryRowContext(ctx, `SELECT `+versionColumns+` FROM album_asset_versions WHERE owner_id=? AND version_id=?`, owner, expected.VersionID))
	if err != nil {
		return err
	}
	if current.AssetID != expected.AssetID || current.ManifestDigest != expected.ManifestDigest || validateReadableVersion(current, owner, expected.VersionID) != nil {
		return ErrIncompleteBackup
	}
	if err := authorize(expiry); err != nil {
		return err
	}
	return tx.Commit()
}
