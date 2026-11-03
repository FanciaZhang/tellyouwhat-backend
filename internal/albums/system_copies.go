package albums

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// These are owner-supplied discovery hints, never resource or deletion evidence.
type SystemCopy struct {
	LocatorID             string `json:"locatorID"`
	Kind                  string `json:"kind"`
	SystemCloudIdentifier string `json:"systemCloudIdentifier"`
}
type SystemCopyInput struct {
	ManifestDigest        string `json:"manifestDigest"`
	Kind                  string `json:"kind"`
	SystemCloudIdentifier string `json:"systemCloudIdentifier"`
}
type SystemCopyList struct {
	VersionID      string       `json:"versionID"`
	ManifestDigest string       `json:"manifestDigest"`
	Copies         []SystemCopy `json:"copies"`
}
type systemCopyRepository interface {
	SystemCopies(context.Context, string, string, time.Time) (SystemCopyList, error)
	RememberSystemCopy(context.Context, string, string, SystemCopyInput, time.Time) (SystemCopy, error)
	ForgetSystemCopy(context.Context, string, string, string, time.Time) error
}

func systemCopyID(kind, identifier string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + identifier))
	return hex.EncodeToString(sum[:])
}
func validSystemCopyKind(kind string, media AssetKind) bool {
	switch kind {
	case "restored_original":
		return media == Photo || media == Video || media == LivePhoto
	case string(PhotoDailyJPEG), string(PhotoDailyPNG):
		return media == Photo
	case string(VideoDaily1080):
		return media == Video
	case DailyLivePair:
		return media == LivePhoto
	}
	return false
}

// Serialize with trash, retention, quarantine and account deletion. Mapping sync
// remains available during the export period without requiring an active plan.
func (r *MySQLUploads) systemCopyTransaction(ctx context.Context, owner, version string, now time.Time,
	action func(*sql.Tx, AssetVersion) error) error {
	if !validOwner(owner) || !validOwner(version) || now.IsZero() {
		return ErrInvalidManifest
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if r.billingEnvironment != "" {
		if _, err = r.shareRetentionInTransaction(ctx, tx, owner, now); errors.Is(err, ErrShareNotAvailable) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
	} else if _, err = lockAccount(ctx, tx, owner); err != nil {
		return err
	}
	if err = requireVisibleVersion(ctx, tx, owner, version); err != nil {
		return err
	}
	v, err := scanAssetVersion(tx.QueryRowContext(ctx, `SELECT `+versionColumns+` FROM album_asset_versions WHERE owner_id=? AND version_id=?`, owner, version))
	if err != nil {
		return err
	}
	if err = validateReadableVersion(v, owner, version); err != nil {
		return err
	}
	if err = action(tx, v); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *MySQLUploads) SystemCopies(ctx context.Context, owner, version string, now time.Time) (SystemCopyList, error) {
	result := SystemCopyList{VersionID: version, Copies: []SystemCopy{}}
	err := r.systemCopyTransaction(ctx, owner, version, now, func(tx *sql.Tx, v AssetVersion) error {
		result.ManifestDigest = v.ManifestDigest
		rows, err := tx.QueryContext(ctx, `SELECT locator_id,copy_kind,cloud_identifier FROM album_system_copy_locators WHERE owner_id=? AND version_id=? ORDER BY locator_id LIMIT 33`, owner, version)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var copy SystemCopy
			if err := rows.Scan(&copy.LocatorID, &copy.Kind, &copy.SystemCloudIdentifier); err != nil {
				return err
			}
			if !validSystemCloudIdentifier(copy.SystemCloudIdentifier) || !validSystemCopyKind(copy.Kind, v.Manifest.Kind) || copy.LocatorID != systemCopyID(copy.Kind, copy.SystemCloudIdentifier) || len(result.Copies) == 32 {
				return ErrConflict
			}
			result.Copies = append(result.Copies, copy)
		}
		return rows.Err()
	})
	return result, err
}

func (r *MySQLUploads) RememberSystemCopy(ctx context.Context, owner, version string, input SystemCopyInput, now time.Time) (SystemCopy, error) {
	result := SystemCopy{LocatorID: systemCopyID(input.Kind, input.SystemCloudIdentifier), Kind: input.Kind, SystemCloudIdentifier: input.SystemCloudIdentifier}
	if !validSystemCloudIdentifier(input.SystemCloudIdentifier) || !validSHA256(input.ManifestDigest) || input.ManifestDigest != strings.ToLower(input.ManifestDigest) {
		return SystemCopy{}, ErrInvalidManifest
	}
	err := r.systemCopyTransaction(ctx, owner, version, now, func(tx *sql.Tx, v AssetVersion) error {
		if input.ManifestDigest != v.ManifestDigest {
			return ErrConflict
		}
		if !validSystemCopyKind(input.Kind, v.Manifest.Kind) {
			return ErrInvalidManifest
		}
		var existing SystemCopy
		err := tx.QueryRowContext(ctx, `SELECT locator_id,copy_kind,cloud_identifier FROM album_system_copy_locators WHERE owner_id=? AND version_id=? AND locator_id=?`, owner, version, result.LocatorID).Scan(&existing.LocatorID, &existing.Kind, &existing.SystemCloudIdentifier)
		if err == nil {
			if existing != result {
				return ErrConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var count int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM album_system_copy_locators WHERE owner_id=? AND version_id=?`, owner, version).Scan(&count); err != nil {
			return err
		}
		if count >= 32 {
			return ErrConflict
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO album_system_copy_locators(owner_id,version_id,locator_id,copy_kind,cloud_identifier) VALUES(?,?,?,?,?)`, owner, version, result.LocatorID, result.Kind, result.SystemCloudIdentifier)
		return err
	})
	return result, err
}
func (r *MySQLUploads) ForgetSystemCopy(ctx context.Context, owner, version, locator string, now time.Time) error {
	if !validSHA256(locator) || locator != strings.ToLower(locator) {
		return ErrInvalidManifest
	}
	return r.systemCopyTransaction(ctx, owner, version, now, func(tx *sql.Tx, _ AssetVersion) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM album_system_copy_locators WHERE owner_id=? AND version_id=? AND locator_id=?`, owner, version, locator)
		return err
	})
}
