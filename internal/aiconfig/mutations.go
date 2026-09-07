package aiconfig

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/contracts"
)

const DraftAction = "ai.draft.create"
const PublishAction = "ai.config.publish"

var validKey = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,128}$`)

func ValidMutation(m Mutation) bool {
	_, actorErr := uuid.Parse(m.Actor)
	_, requestErr := uuid.Parse(m.RequestID)
	return actorErr == nil && requestErr == nil && validKey.MatchString(m.Key)
}

// DraftInput excludes server-generated identity and timestamps from idempotency.
func DraftInput(r Revision) any {
	p := r.Policy
	p.Version = ""
	return struct {
		Operation   contracts.Operation
		BaseVersion string
		Policy      contracts.ExecutionPolicy
	}{r.Operation, r.BaseVersion, p}
}
func commandHash(body any) ([32]byte, error) {
	b, err := json.Marshal(body)
	return sha256.Sum256(b), err
}

func (s MySQLStore) Replay(ctx context.Context, m Mutation, action string, body any) (*Revision, error) {
	if !ValidMutation(m) {
		return nil, ErrInvalid
	}
	hash, err := commandHash(body)
	if err != nil {
		return nil, ErrInvalid
	}
	var storedAction, state string
	var storedHash, raw []byte
	err = s.DB.QueryRowContext(ctx, `SELECT action,request_hash,state,response_json FROM admin_operations WHERE admin_user_id=? AND app_id='health' AND idempotency_key=?`, m.Actor, m.Key).Scan(&storedAction, &storedHash, &state, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if storedAction != action || !bytes.Equal(storedHash, hash[:]) || state != "completed" {
		return nil, ErrConflict
	}
	var r Revision
	if err = json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (s MySQLStore) mutate(ctx context.Context, m Mutation, action string, body any, now time.Time, apply func(*sql.Tx) (Revision, error)) (Revision, error) {
	if !ValidMutation(m) {
		return Revision{}, ErrInvalid
	}
	hash, err := commandHash(body)
	if err != nil {
		return Revision{}, ErrInvalid
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Revision{}, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO admin_operations(admin_user_id,app_id,idempotency_key,action,request_hash) VALUES(?,'health',?,?,?) ON DUPLICATE KEY UPDATE id=id`, m.Actor, m.Key, action, hash[:])
	if err != nil {
		return Revision{}, err
	}
	var oldAction, state string
	var oldHash, raw []byte
	err = tx.QueryRowContext(ctx, `SELECT action,request_hash,state,response_json FROM admin_operations WHERE admin_user_id=? AND app_id='health' AND idempotency_key=? FOR UPDATE`, m.Actor, m.Key).Scan(&oldAction, &oldHash, &state, &raw)
	if err != nil {
		return Revision{}, err
	}
	if oldAction != action || !bytes.Equal(hash[:], oldHash) {
		return Revision{}, ErrConflict
	}
	if state == "completed" {
		var r Revision
		if err = json.Unmarshal(raw, &r); err != nil {
			return r, err
		}
		return r, tx.Commit()
	}
	result, err := apply(tx)
	if err != nil {
		return Revision{}, err
	}
	raw, err = json.Marshal(result)
	if err != nil {
		return Revision{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO admin_audit_events(admin_user_id,app_id,request_id,action,target_type,target_id,outcome,metadata_json,created_at) VALUES(?,'health',?,?,'ai_revision',?,'succeeded',?,?)`, m.Actor, m.RequestID, action, result.ID, `{}`, now.UTC())
	if err != nil {
		return Revision{}, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE admin_operations SET state='completed',response_status=200,response_json=?,completed_at=? WHERE admin_user_id=? AND app_id='health' AND idempotency_key=?`, raw, now.UTC(), m.Actor, m.Key)
	if err != nil {
		return Revision{}, err
	}
	return result, tx.Commit()
}

func (s MySQLStore) Draft(ctx context.Context, r Revision, m Mutation) (Revision, error) {
	r.CreatedAt = r.CreatedAt.UTC().Truncate(time.Microsecond)
	if r.ID == "" || r.Policy.Version != r.ID || r.CreatedBy != m.Actor || r.Policy.Validate(r.Operation) != nil {
		return Revision{}, ErrInvalid
	}
	return s.mutate(ctx, m, DraftAction, DraftInput(r), r.CreatedAt, func(tx *sql.Tx) (Revision, error) {
		raw, err := json.Marshal(r)
		if err != nil {
			return Revision{}, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO health_ai_config_revisions(id,operation,document,created_at) VALUES(?,?,?,?)`, r.ID, r.Operation, raw, r.CreatedAt.UTC())
		return r, err
	})
}
func (s MySQLStore) Publish(ctx context.Context, p Publication, m Mutation, now time.Time) (Revision, error) {
	return s.mutate(ctx, m, PublishAction, p, now, func(tx *sql.Tx) (Revision, error) {
		if _, err := tx.ExecContext(ctx, `INSERT IGNORE INTO health_ai_config_current(operation,revision_id) VALUES(?,'')`, p.Operation); err != nil {
			return Revision{}, err
		}
		var current string
		if err := tx.QueryRowContext(ctx, `SELECT revision_id FROM health_ai_config_current WHERE operation=? FOR UPDATE`, p.Operation).Scan(&current); err != nil {
			return Revision{}, err
		}
		if current != p.BaseVersion {
			return Revision{}, ErrConflict
		}
		r, err := scanRevision(tx.QueryRowContext(ctx, `SELECT document,published_at FROM health_ai_config_revisions WHERE id=? AND operation=?`, p.Revision, p.Operation))
		if err != nil {
			return Revision{}, err
		}
		if r.ID != p.Revision || r.Operation != p.Operation || r.Policy.Version != r.ID || r.BaseVersion != p.BaseVersion || r.PublishedAt != nil || r.Policy.Validate(p.Operation) != nil {
			return Revision{}, ErrConflict
		}
		if _, err = tx.ExecContext(ctx, `UPDATE health_ai_config_revisions SET published_at=? WHERE id=?`, now.UTC(), p.Revision); err != nil {
			return Revision{}, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE health_ai_config_current SET revision_id=? WHERE operation=?`, p.Revision, p.Operation); err != nil {
			return Revision{}, err
		}
		r.PublishedAt = &now
		return *r, nil
	})
}

func scanRevision(row *sql.Row) (*Revision, error) {
	var raw []byte
	var published sql.NullTime
	if err := row.Scan(&raw, &published); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var r Revision
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	if published.Valid {
		r.PublishedAt = &published.Time
	}
	return &r, nil
}
func (s MySQLStore) Get(ctx context.Context, op contracts.Operation, id string) (*Revision, error) {
	return scanRevision(s.DB.QueryRowContext(ctx, `SELECT document,published_at FROM health_ai_config_revisions WHERE id=? AND operation=?`, id, op))
}
func (s MySQLStore) HistoryPage(ctx context.Context, op contracts.Operation, cursor string) (HistoryPage, error) {
	args := []any{op}
	query := `SELECT document,published_at FROM health_ai_config_revisions WHERE operation=?`
	if cursor != "" {
		var at time.Time
		err := s.DB.QueryRowContext(ctx, `SELECT created_at FROM health_ai_config_revisions WHERE operation=? AND id=?`, op, cursor).Scan(&at)
		if errors.Is(err, sql.ErrNoRows) {
			return HistoryPage{}, ErrNotFound
		}
		if err != nil {
			return HistoryPage{}, err
		}
		query += ` AND (created_at < ? OR (created_at = ? AND id < ?))`
		args = append(args, at, at, cursor)
	}
	rows, err := s.DB.QueryContext(ctx, query+` ORDER BY created_at DESC,id DESC LIMIT 51`, args...)
	if err != nil {
		return HistoryPage{}, err
	}
	defer rows.Close()
	out := HistoryPage{Revisions: make([]Revision, 0)}
	for rows.Next() {
		var raw []byte
		var published sql.NullTime
		if err := rows.Scan(&raw, &published); err != nil {
			return HistoryPage{}, err
		}
		var r Revision
		if err := json.Unmarshal(raw, &r); err != nil {
			return HistoryPage{}, err
		}
		if published.Valid {
			r.PublishedAt = &published.Time
		}
		out.Revisions = append(out.Revisions, r)
	}
	if err := rows.Err(); err != nil {
		return HistoryPage{}, err
	}
	if len(out.Revisions) > 50 {
		out.Revisions = out.Revisions[:50]
		out.NextCursor = out.Revisions[49].ID
	}
	return out, nil
}
