package aiconfig

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/tellyouwhat/backend/internal/contracts"
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
