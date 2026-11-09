package albums

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"time"
)

type AssetFavorite struct {
	AssetID   string     `json:"assetID"`
	Revision  int64      `json:"revision"`
	Sequence  int64      `json:"sequence"`
	Favorite  bool       `json:"favorite"`
	UpdatedAt *time.Time `json:"updatedAt,omitempty"`
}
type AssetFavoriteInput struct {
	RequestID        string `json:"requestID"`
	ExpectedRevision int64  `json:"expectedRevision"`
	Favorite         bool   `json:"favorite"`
}
type AssetFavoritePage struct {
	Favorites    []AssetFavorite `json:"favorites"`
	NextSequence int64           `json:"nextSequence"`
	HasMore      bool            `json:"hasMore"`
}
type assetFavoriteRepository interface {
	AssetFavorite(context.Context, string, string, time.Time) (AssetFavorite, error)
	SaveAssetFavorite(context.Context, string, string, AssetFavoriteInput, time.Time) (AssetFavorite, error)
	AssetFavorites(context.Context, string, int64, int, time.Time) (AssetFavoritePage, error)
}

const favoriteColumns = `asset_id,revision,sequence_id,favorite,updated_at,request_id`

func scanAssetFavorite(row rowScanner) (AssetFavorite, string, error) {
	var value AssetFavorite
	var at time.Time
	var request string
	if err := row.Scan(&value.AssetID, &value.Revision, &value.Sequence, &value.Favorite, &at, &request); err != nil {
		return value, "", err
	}
	if !resourceIDPattern.MatchString(value.AssetID) || value.Revision < 1 || value.Sequence < 1 || at.IsZero() || !validOwner(request) {
		return value, "", ErrConflict
	}
	value.UpdatedAt = &at
	return value, request, nil
}
func favoriteRow(ctx context.Context, tx *sql.Tx, owner, asset string) (AssetFavorite, string, error) {
	return scanAssetFavorite(tx.QueryRowContext(ctx, `SELECT `+favoriteColumns+` FROM album_asset_favorites WHERE owner_id=? AND asset_id=?`, owner, asset))
}
func latestActiveAssetVersion(ctx context.Context, tx *sql.Tx, owner, asset string) (AssetVersion, error) {
	if !resourceIDPattern.MatchString(asset) {
		return AssetVersion{}, ErrInvalidManifest
	}
	v, err := scanAssetVersion(tx.QueryRowContext(ctx, `SELECT `+versionColumns+` FROM album_asset_versions v WHERE owner_id=? AND asset_id=? AND EXISTS(SELECT 1 FROM album_asset_lifecycle a WHERE a.owner_id=v.owner_id AND a.asset_id=v.asset_id AND a.state='active') ORDER BY sequence_id DESC LIMIT 1`, owner, asset))
	if err == nil {
		err = validateReadableVersion(v, owner, v.VersionID)
	}
	return v, err
}
func (r *MySQLUploads) AssetFavorite(ctx context.Context, owner, asset string, now time.Time) (AssetFavorite, error) {
	var value AssetFavorite
	err := r.readableAccountTransaction(ctx, owner, now, func(tx *sql.Tx) error {
		v, err := latestActiveAssetVersion(ctx, tx, owner, asset)
		if err != nil {
			return err
		}
		value, _, err = favoriteRow(ctx, tx, owner, asset)
		if errors.Is(err, sql.ErrNoRows) {
			value = AssetFavorite{AssetID: asset}
			if v.Manifest.Metadata != nil {
				value.Favorite = v.Manifest.Metadata.Favorite
			}
			return nil
		}
		return err
	})
	return value, err
}
func (r *MySQLUploads) SaveAssetFavorite(ctx context.Context, owner, asset string, input AssetFavoriteInput, now time.Time) (AssetFavorite, error) {
	if !validOwner(input.RequestID) || input.ExpectedRevision < 0 || input.ExpectedRevision == math.MaxInt64 {
		return AssetFavorite{}, ErrInvalidManifest
	}
	var value AssetFavorite
	err := r.readableAccountTransaction(ctx, owner, now, func(tx *sql.Tx) error {
		if _, err := latestActiveAssetVersion(ctx, tx, owner, asset); err != nil {
			return err
		}
		old, request, err := favoriteRow(ctx, tx, owner, asset)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if request == input.RequestID {
			if old.Revision != input.ExpectedRevision+1 || old.Favorite != input.Favorite {
				return ErrConflict
			}
			value = old
			return nil
		}
		if old.Revision != input.ExpectedRevision {
			return ErrConflict
		}
		// Serialize sequence allocation with every other write for this account.
		if _, err = tx.ExecContext(ctx, `DELETE FROM album_asset_favorites WHERE owner_id=? AND asset_id=?`, owner, asset); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO album_asset_favorites(owner_id,asset_id,revision,request_id,favorite,updated_at) VALUES(?,?,?,?,?,?)`, owner, asset, old.Revision+1, input.RequestID, input.Favorite, now.UTC().Truncate(time.Microsecond)); err != nil {
			return err
		}
		value, _, err = favoriteRow(ctx, tx, owner, asset)
		return err
	})
	return value, err
}
func (r *MySQLUploads) AssetFavorites(ctx context.Context, owner string, after int64, limit int, now time.Time) (AssetFavoritePage, error) {
	page := AssetFavoritePage{Favorites: []AssetFavorite{}, NextSequence: after}
	if after < 0 || limit < 1 || limit > 100 {
		return page, ErrInvalidManifest
	}
	err := r.readableAccountTransaction(ctx, owner, now, func(tx *sql.Tx) error {
		// Keep choices for trashed assets so restoring them cannot miss an older cursor entry.
		rows, err := tx.QueryContext(ctx, `SELECT `+favoriteColumns+` FROM album_asset_favorites WHERE owner_id=? AND sequence_id>? ORDER BY sequence_id LIMIT ?`, owner, after, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			value, _, err := scanAssetFavorite(rows)
			if err != nil {
				return err
			}
			if len(page.Favorites) == limit {
				page.HasMore = true
				break
			}
			page.Favorites = append(page.Favorites, value)
			page.NextSequence = value.Sequence
		}
		return rows.Err()
	})
	return page, err
}
