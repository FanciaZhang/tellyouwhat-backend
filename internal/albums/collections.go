package albums

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type CloudCollection struct {
	CollectionID string    `json:"collectionID"`
	Revision     int64     `json:"revision"`
	Sequence     int64     `json:"sequence"`
	Name         string    `json:"name"`
	Deleted      bool      `json:"deleted"`
	UpdatedAt    time.Time `json:"updatedAt"`
}
type CloudCollectionInput struct {
	RequestID        string `json:"requestID"`
	ExpectedRevision int64  `json:"expectedRevision"`
	Name             string `json:"name"`
	Deleted          bool   `json:"deleted"`
}
type CloudCollectionPage struct {
	Collections  []CloudCollection `json:"collections"`
	NextSequence int64             `json:"nextSequence"`
	HasMore      bool              `json:"hasMore"`
}
type CloudCollectionMember struct {
	CollectionID string     `json:"collectionID"`
	AssetID      string     `json:"assetID"`
	Revision     int64      `json:"revision"`
	Sequence     int64      `json:"sequence"`
	Present      bool       `json:"present"`
	UpdatedAt    *time.Time `json:"updatedAt,omitempty"`
}
type CloudCollectionMemberInput struct {
	RequestID        string `json:"requestID"`
	ExpectedRevision int64  `json:"expectedRevision"`
	Present          bool   `json:"present"`
}
type CloudCollectionMemberPage struct {
	Members      []CloudCollectionMember `json:"members"`
	NextSequence int64                   `json:"nextSequence"`
	HasMore      bool                    `json:"hasMore"`
}
type cloudCollectionRepository interface {
	Collection(context.Context, string, string, time.Time) (CloudCollection, error)
	SaveCollection(context.Context, string, string, CloudCollectionInput, time.Time) (CloudCollection, error)
	Collections(context.Context, string, int64, int, time.Time) (CloudCollectionPage, error)
	CollectionMember(context.Context, string, string, string, time.Time) (CloudCollectionMember, error)
	SaveCollectionMember(context.Context, string, string, string, CloudCollectionMemberInput, time.Time) (CloudCollectionMember, error)
	CollectionMembers(context.Context, string, int64, int, time.Time) (CloudCollectionMemberPage, error)
}

const collectionColumns = `collection_id,revision,sequence_id,name,deleted,updated_at,request_id`
const collectionMemberColumns = `collection_id,asset_id,revision,sequence_id,present,updated_at,request_id`

func validCollectionName(name string, deleted bool) bool {
	if deleted {
		return name == ""
	}
	return utf8.ValidString(name) && utf8.RuneCountInString(name) >= 1 && utf8.RuneCountInString(name) <= 80 && strings.TrimSpace(name) == name && !strings.ContainsFunc(name, unicode.IsControl)
}
func validCollectionWrite(request string, revision int64) bool {
	return validOwner(request) && revision >= 0 && revision < math.MaxInt64
}
func scanCollection(row rowScanner) (CloudCollection, string, error) {
	var v CloudCollection
	var request string
	err := row.Scan(&v.CollectionID, &v.Revision, &v.Sequence, &v.Name, &v.Deleted, &v.UpdatedAt, &request)
	if errors.Is(err, sql.ErrNoRows) {
		return v, "", ErrNotFound
	}
	if err == nil && (!validOwner(v.CollectionID) || v.Revision < 1 || v.Sequence < 1 || !validCollectionName(v.Name, v.Deleted) || v.UpdatedAt.IsZero() || !validOwner(request)) {
		err = ErrConflict
	}
	return v, request, err
}
func collectionRow(ctx context.Context, tx *sql.Tx, owner, id string) (CloudCollection, string, error) {
	return scanCollection(tx.QueryRowContext(ctx, `SELECT `+collectionColumns+` FROM album_collections WHERE owner_id=? AND collection_id=?`, owner, id))
}
func activeCollection(ctx context.Context, tx *sql.Tx, owner, id string) error {
	if !validOwner(id) {
		return ErrInvalidManifest
	}
	v, _, err := collectionRow(ctx, tx, owner, id)
	if err == nil && v.Deleted {
		return ErrNotFound
	}
	return err
}
func (r *MySQLUploads) Collection(ctx context.Context, owner, id string, now time.Time) (CloudCollection, error) {
	var v CloudCollection
	if !validOwner(id) {
		return v, ErrInvalidManifest
	}
	err := r.readableAccountTransaction(ctx, owner, now, func(tx *sql.Tx) error { var err error; v, _, err = collectionRow(ctx, tx, owner, id); return err })
	return v, err
}
func (r *MySQLUploads) SaveCollection(ctx context.Context, owner, id string, input CloudCollectionInput, now time.Time) (CloudCollection, error) {
	var v CloudCollection
	if !validOwner(id) || !validCollectionWrite(input.RequestID, input.ExpectedRevision) || !validCollectionName(input.Name, input.Deleted) {
		return v, ErrInvalidManifest
	}
	err := r.readableAccountTransaction(ctx, owner, now, func(tx *sql.Tx) error {
		old, request, err := collectionRow(ctx, tx, owner, id)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if request == input.RequestID {
			if old.Revision != input.ExpectedRevision+1 || old.Name != input.Name || old.Deleted != input.Deleted {
				return ErrConflict
			}
			v = old
			return nil
		}
		if old.Revision != input.ExpectedRevision || old.Deleted {
			return ErrConflict
		}
		if old.Revision == 0 && input.Deleted {
			return ErrNotFound
		}
		// Account locking orders allocation/commit. No parent FK: replacing this feed row must retain members.
		if _, err = tx.ExecContext(ctx, `DELETE FROM album_collections WHERE owner_id=? AND collection_id=?`, owner, id); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO album_collections(owner_id,collection_id,revision,request_id,name,deleted,updated_at) VALUES(?,?,?,?,?,?,?)`, owner, id, old.Revision+1, input.RequestID, input.Name, input.Deleted, now.UTC().Truncate(time.Microsecond)); err != nil {
			return err
		}
		v, _, err = collectionRow(ctx, tx, owner, id)
		return err
	})
	return v, err
}
func (r *MySQLUploads) Collections(ctx context.Context, owner string, after int64, limit int, now time.Time) (CloudCollectionPage, error) {
	p := CloudCollectionPage{Collections: []CloudCollection{}, NextSequence: after}
	if after < 0 || limit < 1 || limit > 100 {
		return p, ErrInvalidManifest
	}
	err := r.readableAccountTransaction(ctx, owner, now, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT `+collectionColumns+` FROM album_collections WHERE owner_id=? AND sequence_id>? ORDER BY sequence_id LIMIT ?`, owner, after, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			v, _, err := scanCollection(rows)
			if err != nil {
				return err
			}
			if len(p.Collections) == limit {
				p.HasMore = true
				break
			}
			p.Collections = append(p.Collections, v)
			p.NextSequence = v.Sequence
		}
		return rows.Err()
	})
	return p, err
}
func scanCollectionMember(row rowScanner) (CloudCollectionMember, string, error) {
	var v CloudCollectionMember
	var at time.Time
	var request string
	err := row.Scan(&v.CollectionID, &v.AssetID, &v.Revision, &v.Sequence, &v.Present, &at, &request)
	if err == nil {
		if !validOwner(v.CollectionID) || !resourceIDPattern.MatchString(v.AssetID) || v.Revision < 1 || v.Sequence < 1 || at.IsZero() || !validOwner(request) {
			err = ErrConflict
		} else {
			v.UpdatedAt = &at
		}
	}
	return v, request, err
}
func memberRow(ctx context.Context, tx *sql.Tx, owner, id, asset string) (CloudCollectionMember, string, error) {
	return scanCollectionMember(tx.QueryRowContext(ctx, `SELECT `+collectionMemberColumns+` FROM album_collection_members WHERE owner_id=? AND collection_id=? AND asset_id=?`, owner, id, asset))
}
func requireCollectionMemberAsset(ctx context.Context, tx *sql.Tx, owner, id, asset string) error {
	if err := activeCollection(ctx, tx, owner, id); err != nil {
		return err
	}
	_, err := latestActiveAssetVersion(ctx, tx, owner, asset)
	return err
}
func (r *MySQLUploads) CollectionMember(ctx context.Context, owner, id, asset string, now time.Time) (CloudCollectionMember, error) {
	var v CloudCollectionMember
	err := r.readableAccountTransaction(ctx, owner, now, func(tx *sql.Tx) error {
		if err := requireCollectionMemberAsset(ctx, tx, owner, id, asset); err != nil {
			return err
		}
		var err error
		v, _, err = memberRow(ctx, tx, owner, id, asset)
		if errors.Is(err, sql.ErrNoRows) {
			v = CloudCollectionMember{CollectionID: id, AssetID: asset}
			return nil
		}
		return err
	})
	return v, err
}
func (r *MySQLUploads) SaveCollectionMember(ctx context.Context, owner, id, asset string, input CloudCollectionMemberInput, now time.Time) (CloudCollectionMember, error) {
	var v CloudCollectionMember
	if !validCollectionWrite(input.RequestID, input.ExpectedRevision) {
		return v, ErrInvalidManifest
	}
	err := r.readableAccountTransaction(ctx, owner, now, func(tx *sql.Tx) error {
		if err := requireCollectionMemberAsset(ctx, tx, owner, id, asset); err != nil {
			return err
		}
		old, request, err := memberRow(ctx, tx, owner, id, asset)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if request == input.RequestID {
			if old.Revision != input.ExpectedRevision+1 || old.Present != input.Present {
				return ErrConflict
			}
			v = old
			return nil
		}
		if old.Revision != input.ExpectedRevision {
			return ErrConflict
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM album_collection_members WHERE owner_id=? AND collection_id=? AND asset_id=?`, owner, id, asset); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO album_collection_members(owner_id,collection_id,asset_id,revision,request_id,present,updated_at) VALUES(?,?,?,?,?,?,?)`, owner, id, asset, old.Revision+1, input.RequestID, input.Present, now.UTC().Truncate(time.Microsecond)); err != nil {
			return err
		}
		v, _, err = memberRow(ctx, tx, owner, id, asset)
		return err
	})
	return v, err
}
func (r *MySQLUploads) CollectionMembers(ctx context.Context, owner string, after int64, limit int, now time.Time) (CloudCollectionMemberPage, error) {
	p := CloudCollectionMemberPage{Members: []CloudCollectionMember{}, NextSequence: after}
	if after < 0 || limit < 1 || limit > 100 {
		return p, ErrInvalidManifest
	}
	err := r.readableAccountTransaction(ctx, owner, now, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT `+collectionMemberColumns+` FROM album_collection_members WHERE owner_id=? AND sequence_id>? ORDER BY sequence_id LIMIT ?`, owner, after, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		// Retain removals and trash/deleted-collection membership; clients join active catalogue/collection state.
		for rows.Next() {
			v, _, err := scanCollectionMember(rows)
			if err != nil {
				return err
			}
			if len(p.Members) == limit {
				p.HasMore = true
				break
			}
			p.Members = append(p.Members, v)
			p.NextSequence = v.Sequence
		}
		return rows.Err()
	})
	return p, err
}
