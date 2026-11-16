package albums

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"math"
	"slices"
	"strings"
	"time"
)

// Explicit user choices only. This record is neither recognition nor media evidence.
type ContentCategories struct {
	Included []string `json:"included"`
	Excluded []string `json:"excluded"`
}
type ContentCorrectionInput struct {
	RequestID        string `json:"requestID"`
	ExpectedRevision int64  `json:"expectedRevision"`
	ManifestDigest   string `json:"manifestDigest"`
	ContentCategories
}
type ContentCorrection struct {
	VersionID      string     `json:"versionID"`
	ManifestDigest string     `json:"manifestDigest"`
	Revision       int64      `json:"revision"`
	Sequence       int64      `json:"sequence"`
	UpdatedAt      *time.Time `json:"updatedAt,omitempty"`
	ContentCategories
}
type ContentCorrectionPage struct {
	Corrections  []ContentCorrection `json:"corrections"`
	NextSequence int64               `json:"nextSequence"`
	HasMore      bool                `json:"hasMore"`
}
type contentCorrectionRepository interface {
	ContentCorrection(context.Context, string, string, time.Time) (ContentCorrection, error)
	SaveContentCorrection(context.Context, string, string, ContentCorrectionInput, time.Time) (ContentCorrection, error)
	ContentCorrections(context.Context, string, int64, int, time.Time) (ContentCorrectionPage, error)
}

func canonicalContentCategories(input ContentCategories) (ContentCategories, error) {
	if input.Included == nil || input.Excluded == nil || len(input.Included)+len(input.Excluded) > 9 {
		return ContentCategories{}, ErrInvalidManifest
	}
	seen := map[string]bool{}
	for _, category := range append(slices.Clone(input.Included), input.Excluded...) {
		switch category {
		case "people", "cats", "dogs", "food", "plants", "scenery", "buildings", "documents", "screenshots":
		default:
			return ContentCategories{}, ErrInvalidManifest
		}
		if seen[category] {
			return ContentCategories{}, ErrInvalidManifest
		}
		seen[category] = true
	}
	result := ContentCategories{slices.Clone(input.Included), slices.Clone(input.Excluded)}
	slices.Sort(result.Included)
	slices.Sort(result.Excluded)
	return result, nil
}

const contentCorrectionColumns = `c.version_id,v.manifest_digest,c.revision,c.sequence_id,c.updated_at,c.correction_json,c.request_id`

func scanContentCorrection(row rowScanner) (ContentCorrection, string, error) {
	var value ContentCorrection
	var request string
	var raw []byte
	var at time.Time
	if err := row.Scan(&value.VersionID, &value.ManifestDigest, &value.Revision, &value.Sequence, &at, &raw, &request); err != nil {
		return value, "", err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value.ContentCategories); err != nil || decoder.Decode(new(any)) != io.EOF {
		return value, "", ErrConflict
	}
	categories, err := canonicalContentCategories(value.ContentCategories)
	if err != nil || !validOwner(value.VersionID) || !validOwner(request) || !validSHA256(value.ManifestDigest) || value.ManifestDigest != strings.ToLower(value.ManifestDigest) || value.Revision < 1 || value.Sequence < 1 || at.IsZero() {
		return value, "", ErrConflict
	}
	value.ContentCategories = categories
	value.UpdatedAt = &at
	return value, request, nil
}

func contentCorrectionRow(ctx context.Context, tx *sql.Tx, owner, version string) (ContentCorrection, string, error) {
	return scanContentCorrection(tx.QueryRowContext(ctx, `SELECT `+contentCorrectionColumns+` FROM album_content_corrections c JOIN album_asset_versions v ON v.owner_id=c.owner_id AND v.version_id=c.version_id WHERE c.owner_id=? AND c.version_id=?`, owner, version))
}

func (r *MySQLUploads) ContentCorrection(ctx context.Context, owner, version string, now time.Time) (ContentCorrection, error) {
	var result ContentCorrection
	err := r.readableVersionTransaction(ctx, owner, version, now, func(tx *sql.Tx, v AssetVersion) error {
		value, _, err := contentCorrectionRow(ctx, tx, owner, version)
		if errors.Is(err, sql.ErrNoRows) {
			result = ContentCorrection{VersionID: version, ManifestDigest: v.ManifestDigest, ContentCategories: ContentCategories{[]string{}, []string{}}}
			return nil
		}
		result = value
		return err
	})
	return result, err
}

func (r *MySQLUploads) SaveContentCorrection(ctx context.Context, owner, version string, input ContentCorrectionInput, now time.Time) (ContentCorrection, error) {
	categories, err := canonicalContentCategories(input.ContentCategories)
	if err != nil || !validOwner(input.RequestID) || input.ExpectedRevision < 0 || input.ExpectedRevision == math.MaxInt64 || !validSHA256(input.ManifestDigest) || input.ManifestDigest != strings.ToLower(input.ManifestDigest) {
		return ContentCorrection{}, ErrInvalidManifest
	}
	var result ContentCorrection
	err = r.readableVersionTransaction(ctx, owner, version, now, func(tx *sql.Tx, v AssetVersion) error {
		if input.ManifestDigest != v.ManifestDigest {
			return ErrConflict
		}
		old, request, err := contentCorrectionRow(ctx, tx, owner, version)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if request == input.RequestID {
			if old.Revision != input.ExpectedRevision+1 || !slices.Equal(old.Included, categories.Included) || !slices.Equal(old.Excluded, categories.Excluded) {
				return ErrConflict
			}
			result = old
			return nil
		}
		if old.Revision != input.ExpectedRevision {
			return ErrConflict
		}
		raw, err := json.Marshal(categories)
		if err != nil {
			return err
		}
		// The account lock orders sequence allocation and commit for this owner's cursor.
		if _, err = tx.ExecContext(ctx, `DELETE FROM album_content_corrections WHERE owner_id=? AND version_id=?`, owner, version); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO album_content_corrections(owner_id,version_id,revision,request_id,correction_json,updated_at) VALUES(?,?,?,?,?,?)`, owner, version, old.Revision+1, input.RequestID, raw, now.UTC().Truncate(time.Microsecond)); err != nil {
			return err
		}
		result, _, err = contentCorrectionRow(ctx, tx, owner, version)
		return err
	})
	return result, err
}

func (r *MySQLUploads) ContentCorrections(ctx context.Context, owner string, after int64, limit int, now time.Time) (ContentCorrectionPage, error) {
	result := ContentCorrectionPage{Corrections: []ContentCorrection{}, NextSequence: after}
	if after < 0 || limit < 1 || limit > 100 {
		return result, ErrInvalidManifest
	}
	err := r.readableAccountTransaction(ctx, owner, now, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT `+contentCorrectionColumns+` FROM album_content_corrections c JOIN album_asset_versions v ON v.owner_id=c.owner_id AND v.version_id=c.version_id WHERE c.owner_id=? AND c.sequence_id>? ORDER BY c.sequence_id LIMIT ?`, owner, after, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			value, _, err := scanContentCorrection(rows)
			if err != nil {
				return err
			}
			if len(result.Corrections) == limit {
				result.HasMore = true
				break
			}
			result.Corrections = append(result.Corrections, value)
			result.NextSequence = value.Sequence
		}
		return rows.Err()
	})
	return result, err
}
