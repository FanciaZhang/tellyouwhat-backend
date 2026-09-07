package airollout

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/aiconfig"
	"github.com/tellyouwhat/backend/internal/arkcontrol"
	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/costcontrol"
)

var (
	ErrConflict    = aiconfig.ErrConflict
	ErrUnsupported = aiconfig.ErrInvalid
)

type Input struct {
	Endpoint string                     `json:"endpoint"`
	Action   string                     `json:"action"`
	Target   arkcontrol.FoundationModel `json:"target"`
}
type Snapshot struct {
	Endpoint arkcontrol.Endpoint    `json:"endpoint"`
	Rolling  *arkcontrol.Rolling    `json:"rolling,omitempty"`
	Price    costcontrol.TokenPrice `json:"price"`
	Catalog  string                 `json:"catalog"`
}
type Command struct {
	ID        string    `json:"id"`
	Actor     string    `json:"actor"`
	Input     Input     `json:"input"`
	Before    Snapshot  `json:"before"`
	State     string    `json:"state"`
	RollingID string    `json:"rollingID,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}
type PriceState struct {
	Drift    bool                         `json:"drift"`
	Price    costcontrol.TokenPrice       `json:"price"`
	Models   []arkcontrol.FoundationModel `json:"models"`
	Blocked  bool                         `json:"blocked"`
	SyncedAt time.Time                    `json:"syncedAt"`
}
type Store struct{ DB *sql.DB }

// WithLock serializes a specific endpoint across administrator processes. A lost
// connection releases the lock; the durable dispatching state prevents replay.
func (s Store) WithLock(ctx context.Context, endpoint string, fn func() error) error {
	h := sha256.Sum256([]byte(endpoint))
	name := "health-ai:" + hex.EncodeToString(h[:20])
	conn, err := s.DB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var got sql.NullInt64
	if err = conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, 2)", name).Scan(&got); err != nil {
		return err
	}
	if !got.Valid || got.Int64 != 1 {
		return ErrConflict
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, err := conn.ExecContext(c, "SELECT RELEASE_LOCK(?)", name); err != nil {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	return fn()
}
func hash(input Input) []byte {
	raw, _ := json.Marshal(input)
	sum := sha256.Sum256(raw)
	return sum[:]
}
func (s Store) Replay(ctx context.Context, m aiconfig.Mutation, in Input) (*Command, error) {
	if !aiconfig.ValidMutation(m) {
		return nil, ErrUnsupported
	}
	var raw, oldHash []byte
	err := s.DB.QueryRowContext(ctx, "SELECT document,request_hash FROM health_ai_rollout_commands WHERE actor=? AND idempotency_key=?", m.Actor, m.Key).Scan(&raw, &oldHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(hash(in), oldHash) {
		return nil, ErrConflict
	}
	var c Command
	err = json.Unmarshal(raw, &c)
	return &c, err
}
func (s Store) List(ctx context.Context, endpoint string) ([]Command, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT document FROM health_ai_rollout_commands WHERE endpoint=? ORDER BY created_at DESC LIMIT 50", endpoint)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Command, 0)
	for rows.Next() {
		var raw []byte
		var c Command
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(raw, &c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
func (s Store) Pending(ctx context.Context, endpoint string) ([]Command, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT document FROM health_ai_rollout_commands WHERE endpoint=? AND state IN ('queued','dispatching','watching','uncertain') ORDER BY created_at", endpoint)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Command{}
	for rows.Next() {
		var raw []byte
		var c Command
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(raw, &c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
func (s Store) PriceState(ctx context.Context, endpoint string) (*PriceState, error) {
	var raw []byte
	err := s.DB.QueryRowContext(ctx, "SELECT document FROM health_ai_endpoint_prices WHERE endpoint=?", endpoint).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p PriceState
	err = json.Unmarshal(raw, &p)
	return &p, err
}
func audit(ctx context.Context, tx *sql.Tx, c Command, outcome string) error {
	raw, _ := json.Marshal(map[string]string{"state": c.State, "endpoint": c.Input.Endpoint, "action": c.Input.Action})
	_, err := tx.ExecContext(ctx, `INSERT INTO admin_audit_events(admin_user_id,app_id,request_id,action,target_type,target_id,outcome,metadata_json) VALUES(?,'health',?,'ai.rolling.command','ai_command',?,?,?)`, c.Actor, uuid.NewString(), c.ID, outcome, raw)
	return err
}
func savePrice(ctx context.Context, tx *sql.Tx, endpoint string, p PriceState) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO health_ai_endpoint_prices(endpoint,document,updated_at) VALUES(?,?,?) ON DUPLICATE KEY UPDATE document=VALUES(document),updated_at=VALUES(updated_at)`, endpoint, raw, time.Now().UTC())
	return err
}
func (s Store) SavePrice(ctx context.Context, endpoint string, p PriceState) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = savePrice(ctx, tx, endpoint, p); err != nil {
		return err
	}
	return tx.Commit()
}

// Enqueue commits the command, price guard and audit together before cloud writes.
// Caller must hold the endpoint lock.
func (s Store) Enqueue(ctx context.Context, m aiconfig.Mutation, in Input, before Snapshot) (Command, error) {
	if !aiconfig.ValidMutation(m) {
		return Command{}, ErrUnsupported
	}
	if old, err := s.Replay(ctx, m, in); err != nil {
		return Command{}, err
	} else if old != nil {
		return *old, nil
	}
	pending, err := s.Pending(ctx, in.Endpoint)
	if err != nil {
		return Command{}, err
	}
	for _, c := range pending {
		if in.Action != "reconcile" && (c.State != "watching" || in.Action == "start") {
			return Command{}, ErrConflict
		}
	}
	now := time.Now().UTC()
	c := Command{ID: uuid.NewString(), Actor: m.Actor, Input: in, Before: before, State: "queued", CreatedAt: now, UpdatedAt: now}
	if before.Rolling != nil {
		c.RollingID = before.Rolling.ID
	}
	p, err := s.PriceState(ctx, in.Endpoint)
	if err != nil {
		return c, err
	}
	if p == nil {
		p = &PriceState{}
	}
	p.Price = ceiling(p.Price, before.Price)
	p.Blocked = false
	p.Drift = false
	p.SyncedAt = now
	models := []arkcontrol.FoundationModel{endpointModel(before.Endpoint), in.Target}
	if before.Rolling != nil {
		models = append(models, before.Rolling.In, before.Rolling.Out)
	}
	for _, model := range models {
		if Known(model) {
			p.Models = addModel(p.Models, model)
		}
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return c, err
	}
	defer tx.Rollback()
	raw, _ := json.Marshal(c)
	if _, err = tx.ExecContext(ctx, `INSERT INTO health_ai_rollout_commands(id,actor,idempotency_key,request_hash,endpoint,state,document,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`, c.ID, m.Actor, m.Key, hash(in), in.Endpoint, c.State, raw, now, now); err != nil {
		return c, err
	}
	if err = savePrice(ctx, tx, in.Endpoint, *p); err != nil {
		return c, err
	}
	if err = audit(ctx, tx, c, "succeeded"); err != nil {
		return c, err
	}
	return c, tx.Commit()
}
func (s Store) Save(ctx context.Context, c Command) error {
	c.UpdatedAt = time.Now().UTC()
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "UPDATE health_ai_rollout_commands SET state=?,document=?,updated_at=? WHERE id=?", c.State, raw, c.UpdatedAt, c.ID); err != nil {
		return err
	}
	outcome := "succeeded"
	if c.State == "failed" || c.State == "uncertain" || c.State == "conflict" {
		outcome = "failed"
	}
	if err = audit(ctx, tx, c, outcome); err != nil {
		return err
	}
	return tx.Commit()
}
func addModel(models []arkcontrol.FoundationModel, m arkcontrol.FoundationModel) []arkcontrol.FoundationModel {
	for _, old := range models {
		if old == m {
			return models
		}
	}
	return append(models, m)
}

// AttemptPrice reads the current endpoint envelope, including for old queued
// request snapshots. Missing rows preserve deployment compatibility; once an
// endpoint is managed, stale or blocked inventory fails closed.
func (s Store) AttemptPrice(endpoints map[contracts.Operation]string) func(context.Context, contracts.Request) (*costcontrol.TokenPrice, error) {
	return func(ctx context.Context, r contracts.Request) (*costcontrol.TokenPrice, error) {
		id := endpoints[r.Operation]
		if r.ExecutionPolicy != nil {
			id = r.ExecutionPolicy.Endpoint
		}
		p, err := s.PriceState(ctx, id)
		if err != nil {
			return nil, err
		}
		if p == nil {
			return nil, nil
		}
		if p.Blocked || !p.Price.Valid() || time.Since(p.SyncedAt) > 5*time.Minute {
			return nil, costcontrol.ErrInvalidAttempt
		}
		return &p.Price, nil
	}
}

// Reservation and command submission share the endpoint lock. Before a native
// mutation, earlier reservations drain while new attempts use the new envelope.
func (s Store) Reservation(endpoints map[contracts.Operation]string) func(context.Context, contracts.Request, *costcontrol.Controller, string, costcontrol.TokenPrice) (*costcontrol.Lease, costcontrol.TokenPrice, error) {
	return func(ctx context.Context, r contracts.Request, controller *costcontrol.Controller, appID string, fallback costcontrol.TokenPrice) (*costcontrol.Lease, costcontrol.TokenPrice, error) {
		id := endpoints[r.Operation]
		if r.ExecutionPolicy != nil {
			id = r.ExecutionPolicy.Endpoint
		}
		var lease *costcontrol.Lease
		price := fallback
		err := s.WithLock(ctx, id, func() error {
			p, err := s.AttemptPrice(endpoints)(ctx, r)
			if err != nil {
				return err
			}
			if p != nil {
				price = *p
			}
			input := contracts.ReservationTokens(r) - r.OutputTokenReservation()
			reserved, err := price.Cost(input, r.OutputTokenReservation())
			if err != nil {
				return err
			}
			lease, err = controller.Reserve(ctx, appID, string(r.Operation), "ark", reserved)
			return err
		})
		return lease, price, err
	}
}
func (s Store) EarlierAttempts(ctx context.Context, before time.Time) (bool, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM ai_cost_attempts WHERE app_id='health' AND status='pending' AND created_at < ? AND lease_expires_at > ?`, before, time.Now().UTC()).Scan(&n)
	return n > 0, err
}
