package aiconfig

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/tellyouwhat/backend/internal/contracts"
	"time"
)

type MySQLStore struct{ DB *sql.DB }

func (s MySQLStore) Current(ctx context.Context, op contracts.Operation) (*Revision, error) {
	var raw []byte
	var published sql.NullTime
	err := s.DB.QueryRowContext(ctx, `SELECT r.document,r.published_at FROM health_ai_config_current c JOIN health_ai_config_revisions r ON r.id=c.revision_id WHERE c.operation=?`, op).Scan(&raw, &published)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out Revision
	err = json.Unmarshal(raw, &out)
	if published.Valid {
		out.PublishedAt = &published.Time
	}
	return &out, err
}
func (s MySQLStore) History(ctx context.Context, op contracts.Operation) ([]Revision, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT document,published_at FROM health_ai_config_revisions WHERE operation=? ORDER BY created_at DESC,id DESC LIMIT 100`, op)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Revision, 0)
	for rows.Next() {
		var raw []byte
		var published sql.NullTime
		if err := rows.Scan(&raw, &published); err != nil {
			return nil, err
		}
		var r Revision
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
		if published.Valid {
			r.PublishedAt = &published.Time
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func (s MySQLStore) Draft(ctx context.Context, r Revision) error {
	if r.ID == "" || r.Policy.Version != r.ID || r.CreatedBy == "" || r.Policy.Validate(r.Operation) != nil {
		return ErrInvalid
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx, `INSERT INTO health_ai_config_revisions(id,operation,document,created_at) VALUES(?,?,?,?)`, r.ID, r.Operation, b, r.CreatedAt.UTC())
	return err
}
func (s MySQLStore) Publish(ctx context.Context, id string, op contracts.Operation, base string, now time.Time) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT IGNORE INTO health_ai_config_current(operation,revision_id) VALUES(?,'')`, op); err != nil {
		return err
	}
	var current string
	if err = tx.QueryRowContext(ctx, `SELECT revision_id FROM health_ai_config_current WHERE operation=? FOR UPDATE`, op).Scan(&current); err != nil {
		return err
	}
	if current == id {
		return tx.Commit()
	}
	if current != base {
		return ErrConflict
	}
	var raw []byte
	var published sql.NullTime
	if err = tx.QueryRowContext(ctx, `SELECT document,published_at FROM health_ai_config_revisions WHERE id=? AND operation=?`, id, op).Scan(&raw, &published); err != nil {
		return err
	}
	var r Revision
	if json.Unmarshal(raw, &r) != nil || r.BaseVersion != base || published.Valid || r.Policy.Validate(op) != nil {
		return ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE health_ai_config_revisions SET published_at=? WHERE id=?`, now.UTC(), id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE health_ai_config_current SET revision_id=? WHERE operation=?`, id, op); err != nil {
		return err
	}
	return tx.Commit()
}
