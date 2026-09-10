package platformops

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"time"
)

type HostCheck struct {
	Name          string `json:"name"`
	Passed        bool   `json:"passed"`
	AgeSeconds    *int64 `json:"age_seconds,omitempty"`
	FreeMegabytes *int64 `json:"free_megabytes,omitempty"`
}
type HostHealth struct {
	CheckedAt time.Time   `json:"checkedAt"`
	Checks    []HostCheck `json:"checks"`
}

func (s Store) RecordHostHealth(ctx context.Context, source io.Reader, now time.Time) error {
	var h HostHealth
	input, err := io.ReadAll(io.LimitReader(source, 16385))
	if err != nil {
		return err
	}
	if len(input) > 16384 {
		return ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(input))
	d.DisallowUnknownFields()
	if d.Decode(&h) != nil || d.Decode(new(any)) != io.EOF || h.CheckedAt.After(now.Add(30*time.Second)) || h.CheckedAt.Before(now.Add(-2*time.Minute)) {
		return ErrInvalid
	}
	if err := validateHostChecks(h.Checks); err != nil {
		return err
	}
	h.CheckedAt = h.CheckedAt.UTC().Truncate(time.Microsecond)
	raw, err := json.Marshal(h)
	if err != nil {
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
	var previous sql.NullTime
	if err = tx.QueryRowContext(ctx, `SELECT host_checked_at FROM platform_ops_patrol WHERE singleton_id=1 FOR UPDATE`).Scan(&previous); err != nil {
		return err
	}
	if previous.Valid && !h.CheckedAt.After(previous.Time) {
		return tx.Commit()
	}
	_, err = tx.ExecContext(ctx, `UPDATE platform_ops_patrol SET host_document=?,host_checked_at=? WHERE singleton_id=1`, raw, h.CheckedAt)
	if err != nil {
		return err
	}
	// Host evidence remains durable even while the admin process is unavailable.
	for _, check := range h.Checks {
		c := condition{"", "", check.Name, true, !check.Passed, h.CheckedAt, map[string]any{"ageSeconds": check.AgeSeconds, "freeMegabytes": check.FreeMegabytes}}
		if err = observeCondition(ctx, tx, c, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Legacy reports contain eight checks; managed deployments add both lifecycle checks.
func validateHostChecks(checks []HostCheck) error {
	expected := map[string]bool{"health_gateway": true, "journal_gateway": true, "worker": true, "admin": true, "disk_space": true, "backup_freshness": true, "maintenance_freshness": true, "restore_freshness": true}
	if len(checks) == len(expected)+2 {
		expected["deployment_state"] = true
		expected["deployment_controller"] = true
	}
	if len(checks) != len(expected) {
		return ErrInvalid
	}
	for _, check := range checks {
		if !expected[check.Name] {
			return ErrInvalid
		}
		delete(expected, check.Name)
	}
	return nil
}
