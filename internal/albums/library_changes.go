package albums

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

type LibraryChange struct {
	Sequence       int64         `json:"sequence"`
	AssetID        string        `json:"assetID"`
	VersionID      string        `json:"versionID"`
	Revision       int64         `json:"revision"`
	State          string        `json:"state"`
	ChangedAt      time.Time     `json:"changedAt"`
	TrashedAt      *time.Time    `json:"trashedAt,omitempty"`
	TrashExpiresAt *time.Time    `json:"trashExpiresAt,omitempty"`
	Version        *AssetVersion `json:"version,omitempty"`
}
type LibraryChangePage struct {
	Changes      []LibraryChange `json:"changes"`
	NextSequence int64           `json:"nextSequence"`
	HasMore      bool            `json:"hasMore"`
}

const changeColumns = `sequence_id,asset_id,version_id,revision,state,changed_at,trashed_at,trash_expires_at`

func scanLibraryChange(row rowScanner) (LibraryChange, error) {
	var c LibraryChange
	var trashed, expires sql.NullTime
	err := row.Scan(&c.Sequence, &c.AssetID, &c.VersionID, &c.Revision, &c.State, &c.ChangedAt, &trashed, &expires)
	if err != nil {
		return c, err
	}
	if c.Sequence < 1 || c.Revision < 1 || !resourceIDPattern.MatchString(c.AssetID) || !validOwner(c.VersionID) || c.ChangedAt.IsZero() || (c.State != "active" && c.State != "trashed" && c.State != "purged") || (c.State == "trashed") != trashed.Valid || trashed.Valid != expires.Valid || expires.Valid && !trashed.Time.Before(expires.Time) {
		return c, ErrConflict
	}
	if trashed.Valid {
		c.TrashedAt = &trashed.Time
		c.TrashExpiresAt = &expires.Time
	}
	return c, nil
}
func (r *MySQLUploads) LibraryChanges(ctx context.Context, owner string, after int64, limit int) (LibraryChangePage, error) {
	page := LibraryChangePage{Changes: []LibraryChange{}, NextSequence: after}
	if !validOwner(owner) || after < 0 || limit < 1 || limit > 100 {
		return page, ErrInvalidManifest
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT `+changeColumns+` FROM album_library_changes WHERE owner_id=? AND sequence_id>? ORDER BY sequence_id LIMIT ?`, owner, after, limit+1)
	if err != nil {
		return page, err
	}
	var events []LibraryChange
	for rows.Next() {
		c, err := scanLibraryChange(rows)
		if err != nil {
			rows.Close()
			return page, err
		}
		events = append(events, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return page, err
	}
	size := 0
	for _, c := range events {
		if len(page.Changes) == limit {
			page.HasMore = true
			break
		}
		if c.State == "active" {
			v, err := scanAssetVersion(tx.QueryRowContext(ctx, `SELECT `+versionColumns+` FROM album_asset_versions WHERE owner_id=? AND version_id=?`, owner, c.VersionID))
			if err != nil {
				return page, err
			}
			if v.AssetID != c.AssetID || validateVersionOwner(v, owner) != nil {
				return page, ErrIncompleteBackup
			}
			c.Version = &v
		}
		encoded, err := json.Marshal(c)
		if err != nil {
			return page, err
		}
		if len(encoded) > (1<<20)-4096 {
			return page, ErrInvalidManifest
		}
		if size+len(encoded) > (1<<20)-4096 {
			page.HasMore = true
			break
		}
		size += len(encoded)
		page.Changes = append(page.Changes, c)
		page.NextSequence = c.Sequence
	}
	return page, tx.Commit()
}
