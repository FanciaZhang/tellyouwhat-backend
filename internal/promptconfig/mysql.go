package promptconfig

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/aiconfig"
)

type Store struct{ DB *sql.DB }
type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// CurrentFrom also supports a transaction holding the cost-control lock.
func CurrentFrom(ctx context.Context, db queryer, scope string) (Revision, error) {
	return scan(db.QueryRowContext(ctx, `SELECT r.document,r.published_at FROM prompt_config_current c JOIN prompt_config_revisions r ON r.id=c.revision_id WHERE c.scope=?`, scope))
}
func (s Store) Current(ctx context.Context, scope string) (Revision, error) {
	return CurrentFrom(ctx, s.DB, scope)
}
func (s Store) Get(ctx context.Context, id string) (Revision, error) {
	return scan(s.DB.QueryRowContext(ctx, `SELECT document,published_at FROM prompt_config_revisions WHERE id=?`, id))
}
func scan(row *sql.Row) (Revision, error) {
	var raw []byte
	var published sql.NullTime
	var r Revision
	if err := row.Scan(&raw, &published); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return r, ErrNotFound
		}
		return r, err
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return r, err
	}
	if err := r.Policy.Validate(r.Scope); err != nil {
		return r, err
	}
	if published.Valid {
		r.PublishedAt = &published.Time
	}
	return r, nil
}
func lock(ctx context.Context, tx *sql.Tx) error {
	var id int
	return tx.QueryRowContext(ctx, `SELECT singleton_id FROM ai_cost_control_state WHERE singleton_id=1 FOR UPDATE`).Scan(&id)
}
func (s Store) Initialize(ctx context.Context, scope string, p Policy, now time.Time) error {
	if err := p.Validate(scope); err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = lock(ctx, tx); err != nil {
		return err
	}
	if _, err = CurrentFrom(ctx, tx, scope); err == nil {
		return tx.Commit()
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	now = now.UTC().Truncate(time.Microsecond)
	r := Revision{ID: uuid.NewString(), Scope: scope, Policy: p, CreatedBy: "deployment", CreatedAt: now, PublishedAt: &now}
	raw, _ := json.Marshal(r)
	if _, err = tx.ExecContext(ctx, `INSERT INTO prompt_config_revisions(id,document,created_at,published_at) VALUES(?,?,?,?)`, r.ID, raw, now, now); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO prompt_config_current(scope,revision_id) VALUES(?,?)`, scope, r.ID); err != nil {
		return err
	}
	return tx.Commit()
}

type Mutation = aiconfig.Mutation
type DraftInput struct {
	Scope       string `json:"scope"`
	BaseVersion string `json:"baseVersion"`
	Policy      Policy `json:"policy"`
}
type Publication struct {
	Scope       string `json:"scope"`
	Revision    string `json:"revision"`
	BaseVersion string `json:"baseVersion"`
}

func (s Store) Replay(ctx context.Context, m Mutation, action string, input any) (*Revision, error) {
	if !aiconfig.ValidMutation(m) {
		return nil, ErrInvalid
	}
	body, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(body)
	var oldAction string
	var oldHash, raw []byte
	err = s.DB.QueryRowContext(ctx, `SELECT action,request_hash,response_json FROM prompt_config_mutations WHERE actor_id=? AND idempotency_key=?`, m.Actor, m.Key).Scan(&oldAction, &oldHash, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if oldAction != action || !bytes.Equal(hash[:], oldHash) {
		return nil, ErrConflict
	}
	var r Revision
	err = json.Unmarshal(raw, &r)
	return &r, err
}

func (s Store) mutate(ctx context.Context, m Mutation, action string, input any, now time.Time, apply func(*sql.Tx) (Revision, error)) (Revision, error) {
	if !aiconfig.ValidMutation(m) {
		return Revision{}, ErrInvalid
	}
	body, err := json.Marshal(input)
	if err != nil {
		return Revision{}, err
	}
	hash := sha256.Sum256(body)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Revision{}, err
	}
	defer tx.Rollback()
	if err = lock(ctx, tx); err != nil {
		return Revision{}, err
	}
	var previousAction string
	var oldHash, raw []byte
	err = tx.QueryRowContext(ctx, `SELECT action,request_hash,response_json FROM prompt_config_mutations WHERE actor_id=? AND idempotency_key=?`, m.Actor, m.Key).Scan(&previousAction, &oldHash, &raw)
	if err == nil {
		if previousAction != action || !bytes.Equal(oldHash, hash[:]) {
			return Revision{}, ErrConflict
		}
		var r Revision
		if err = json.Unmarshal(raw, &r); err != nil {
			return r, err
		}
		return r, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Revision{}, err
	}
	r, err := apply(tx)
	if err != nil {
		return Revision{}, err
	}
	raw, err = json.Marshal(r)
	if err != nil {
		return Revision{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO admin_audit_events(admin_user_id,app_id,request_id,action,target_type,target_id,outcome,metadata_json,created_at) VALUES(?,NULL,?,?,'prompt_revision',?,'succeeded','{}',?)`, m.Actor, m.RequestID, action, r.ID, now.UTC())
	if err != nil {
		return Revision{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO prompt_config_mutations(actor_id,idempotency_key,action,request_hash,response_json,created_at) VALUES(?,?,?,?,?,?)`, m.Actor, m.Key, action, hash[:], raw, now.UTC())
	if err != nil {
		return Revision{}, err
	}
	return r, tx.Commit()
}
func (s Store) Draft(ctx context.Context, input DraftInput, m Mutation, now time.Time) (Revision, error) {
	return s.DraftResolved(ctx, input, input.Policy, m, now)
}

// DraftResolved records idempotency against the submitted input, while storing verified server metadata.
func (s Store) DraftResolved(ctx context.Context, input DraftInput, resolved Policy, m Mutation, now time.Time) (Revision, error) {
	if resolved.Validate(input.Scope) != nil || uuid.Validate(input.BaseVersion) != nil {
		return Revision{}, ErrInvalid
	}
	return s.mutate(ctx, m, "prompts.draft", input, now, func(tx *sql.Tx) (Revision, error) {
		current, err := CurrentFrom(ctx, tx, input.Scope)
		if err != nil {
			return Revision{}, err
		}
		if current.ID != input.BaseVersion {
			return Revision{}, ErrConflict
		}
		if current.Policy.Journal != nil {
			for _, style := range current.Policy.Journal.Styles {
				if _, err := resolved.Journal.Style(style.ID); err != nil {
					return Revision{}, ErrInvalid
				}
			}
		}
		r := Revision{ID: uuid.NewString(), Scope: input.Scope, BaseVersion: input.BaseVersion, Policy: resolved, CreatedBy: m.Actor, CreatedAt: now.UTC().Truncate(time.Microsecond)}
		raw, _ := json.Marshal(r)
		_, err = tx.ExecContext(ctx, `INSERT INTO prompt_config_revisions(id,document,created_at) VALUES(?,?,?)`, r.ID, raw, r.CreatedAt)
		return r, err
	})
}
func (s Store) Publish(ctx context.Context, input Publication, m Mutation, now time.Time) (Revision, error) {
	return s.mutate(ctx, m, "prompts.publish", input, now, func(tx *sql.Tx) (Revision, error) {
		current, err := CurrentFrom(ctx, tx, input.Scope)
		if err != nil {
			return Revision{}, err
		}
		if current.ID != input.BaseVersion {
			return Revision{}, ErrConflict
		}
		r, err := scan(tx.QueryRowContext(ctx, `SELECT document,published_at FROM prompt_config_revisions WHERE id=?`, input.Revision))
		if err != nil {
			return r, err
		}
		if r.Scope != input.Scope || r.PublishedAt != nil || r.BaseVersion != input.BaseVersion {
			return Revision{}, ErrConflict
		}
		_, err = tx.ExecContext(ctx, `UPDATE prompt_config_revisions SET published_at=? WHERE id=?`, now.UTC(), r.ID)
		if err != nil {
			return r, err
		}
		_, err = tx.ExecContext(ctx, `UPDATE prompt_config_current SET revision_id=? WHERE scope=?`, r.ID, input.Scope)
		if err != nil {
			return r, err
		}
		r.PublishedAt = &now
		return r, nil
	})
}

type History struct {
	Revisions  []Revision `json:"revisions"`
	NextCursor string     `json:"nextCursor,omitempty"`
}

func (s Store) History(ctx context.Context, scope, cursor string) (History, error) {
	args := []any{scope}
	condition := " WHERE JSON_UNQUOTE(JSON_EXTRACT(document, '$.scope'))=?"
	if cursor != "" {
		r, err := s.Get(ctx, cursor)
		if err != nil {
			return History{}, err
		}
		if r.Scope != scope {
			return History{}, ErrInvalid
		}
		condition += " AND (created_at < ? OR (created_at = ? AND id < ?))"
		args = append(args, r.CreatedAt, r.CreatedAt, r.ID)
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT document,published_at FROM prompt_config_revisions`+condition+` ORDER BY created_at DESC,id DESC LIMIT 51`, args...)
	if err != nil {
		return History{}, err
	}
	defer rows.Close()
	out := History{Revisions: []Revision{}}
	for rows.Next() {
		var raw []byte
		var published sql.NullTime
		var r Revision
		if err = rows.Scan(&raw, &published); err != nil {
			return out, err
		}
		if err = json.Unmarshal(raw, &r); err != nil {
			return out, err
		}
		if published.Valid {
			r.PublishedAt = &published.Time
		}
		out.Revisions = append(out.Revisions, r)
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	if len(out.Revisions) > 50 {
		out.Revisions = out.Revisions[:50]
		out.NextCursor = out.Revisions[49].ID
	}
	return out, nil
}
