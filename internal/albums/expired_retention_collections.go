package albums

import (
	"context"
	"math"
	"time"

	"github.com/google/uuid"
)

// The opt-in retention worker consumes a fresh proof; activation also requires
// cloud-write closure and backup erasure verification.
func (r *MySQLUploads) expireRetentionCollections(ctx context.Context, proof expiredRetentionProof, limit int, now func() time.Time) (int, error) {
	if limit < 1 || limit > 100 || now == nil {
		return 0, ErrInvalidManifest
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := lockAccount(ctx, tx, proof.owner); err != nil {
		return 0, err
	}
	clock := now().UTC()
	if r.billingNow != nil {
		clock = maxTime(clock, r.billingNow().UTC())
	}
	if err := r.checkExpiredRetention(ctx, tx, proof, clock); err != nil {
		return 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+collectionColumns+` FROM album_collections c WHERE owner_id=?
 AND (deleted=FALSE OR EXISTS(SELECT 1 FROM album_collection_members m WHERE m.owner_id=c.owner_id AND m.collection_id=c.collection_id))
 ORDER BY sequence_id LIMIT ?`, proof.owner, limit)
	if err != nil {
		return 0, err
	}
	var collections []CloudCollection
	for rows.Next() {
		v, _, err := scanCollection(rows)
		if err != nil {
			rows.Close()
			return 0, err
		}
		if v.UpdatedAt.After(clock) || !v.Deleted && v.Revision == math.MaxInt64 {
			rows.Close()
			return 0, ErrConflict
		}
		collections = append(collections, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	for _, v := range collections {
		if _, err = tx.ExecContext(ctx, `DELETE FROM album_collection_members WHERE owner_id=? AND collection_id=?`, proof.owner, v.CollectionID); err != nil {
			return 0, err
		}
		if v.Deleted {
			continue
		}
		// A new feed sequence lets returning devices discard the retired group.
		if _, err = tx.ExecContext(ctx, `DELETE FROM album_collections WHERE owner_id=? AND collection_id=?`, proof.owner, v.CollectionID); err != nil {
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO album_collections(owner_id,collection_id,revision,request_id,name,deleted,updated_at) VALUES(?,?,?,?,'',TRUE,?)`, proof.owner, v.CollectionID, v.Revision+1, uuid.NewString(), clock.Truncate(time.Microsecond)); err != nil {
			return 0, err
		}
	}
	clock = now().UTC()
	if r.billingNow != nil {
		clock = maxTime(clock, r.billingNow().UTC())
	}
	if err = r.checkExpiredRetention(ctx, tx, proof, clock); err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return len(collections), nil
}
