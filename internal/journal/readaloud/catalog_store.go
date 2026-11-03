package readaloud

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

var (
	ErrCatalogConflict = errors.New("read-aloud catalog changed or mutation already claimed")
	ErrCatalogNotFound = errors.New("read-aloud catalog not found")
)

// Admin mutation input deliberately has no verification or resource override.
type VoiceConfiguration struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Description   string   `json:"description"`
	Languages     []string `json:"languages"`
	Styles        []string `json:"styles"`
	Dialects      []string `json:"dialects"`
	SupportsPitch bool     `json:"supportsPitch"`
	PreviewURL    string   `json:"previewURL"`
	BillingClass  string   `json:"billingClass"`
	Enabled       bool     `json:"enabled"`
	Rank          int      `json:"rank"`
	ProbeText     string   `json:"probeText"`
}

func (c VoiceConfiguration) Voice() Voice {
	return Voice{ID: c.ID, Name: c.Name, Description: c.Description, Languages: c.Languages, Styles: c.Styles,
		Dialects: c.Dialects, SupportsPitch: c.SupportsPitch, PreviewURL: c.PreviewURL, ResourceID: ResourceID,
		BillingClass: c.BillingClass, Enabled: c.Enabled, Rank: c.Rank, ProbeText: c.ProbeText}
}
func Configuration(v Voice) VoiceConfiguration {
	return VoiceConfiguration{v.ID, v.Name, v.Description, v.Languages, v.Styles, v.Dialects, v.SupportsPitch, v.PreviewURL, v.BillingClass, v.Enabled, v.Rank, v.ProbeText}
}

type CatalogDraft struct {
	BaseVersion    string               `json:"baseVersion"`
	DefaultVoiceID string               `json:"defaultVoiceID"`
	Voices         []VoiceConfiguration `json:"voices"`
}
type CatalogPublication struct {
	BaseVersion string `json:"baseVersion"`
	Revision    string `json:"revision"`
}
type CatalogRevision struct {
	Catalog
	BaseVersion string     `json:"baseVersion"`
	CreatedBy   string     `json:"createdBy"`
	CreatedAt   time.Time  `json:"createdAt"`
	PublishedAt *time.Time `json:"publishedAt,omitempty"`
}
type CatalogHistory struct {
	Revisions  []CatalogRevisionSummary `json:"revisions"`
	NextCursor string                   `json:"nextCursor,omitempty"`
}
type CatalogRevisionSummary struct {
	Version      string     `json:"version"`
	BaseVersion  string     `json:"baseVersion"`
	CreatedBy    string     `json:"createdBy"`
	CreatedAt    time.Time  `json:"createdAt"`
	PublishedAt  *time.Time `json:"publishedAt,omitempty"`
	VoiceCount   int        `json:"voiceCount"`
	EnabledCount int        `json:"enabledCount"`
}
type CatalogStore struct{ DB *sql.DB }

func catalogRevision(row *sql.Row) (CatalogRevision, error) {
	var result CatalogRevision
	var raw []byte
	var published sql.NullTime
	if err := row.Scan(&raw, &published); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return result, ErrCatalogNotFound
		}
		return result, err
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return result, err
	}
	if published.Valid {
		result.PublishedAt = &published.Time
	}
	return result, nil
}
func (s CatalogStore) Get(ctx context.Context, id string) (CatalogRevision, error) {
	if uuid.Validate(id) != nil {
		return CatalogRevision{}, ErrCatalog
	}
	return catalogRevision(s.DB.QueryRowContext(ctx, `SELECT document,published_at FROM journal_voice_catalog_revisions WHERE id=?`, id))
}
func (s CatalogStore) Current(ctx context.Context) (CatalogRevision, error) {
	r, err := catalogRevision(s.DB.QueryRowContext(ctx, `SELECT r.document,r.published_at FROM journal_voice_catalog_current c JOIN journal_voice_catalog_revisions r ON r.id=c.revision_id WHERE c.singleton_id=1`))
	if err == nil {
		err = r.ValidatePublication()
	}
	return r, err
}
func currentCatalogVersion(ctx context.Context, tx *sql.Tx) (string, error) {
	var id sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT revision_id FROM journal_voice_catalog_current WHERE singleton_id=1 FOR UPDATE`).Scan(&id)
	return id.String, err
}
func (s CatalogStore) History(ctx context.Context, cursor string) (CatalogHistory, error) {
	condition := ""
	args := []any{}
	if cursor != "" {
		if uuid.Validate(cursor) != nil {
			return CatalogHistory{}, ErrCatalog
		}
		var created time.Time
		err := s.DB.QueryRowContext(ctx, "SELECT created_at FROM journal_voice_catalog_revisions WHERE id=?", cursor).Scan(&created)
		if errors.Is(err, sql.ErrNoRows) {
			return CatalogHistory{}, ErrCatalogNotFound
		}
		if err != nil {
			return CatalogHistory{}, err
		}
		condition = ` WHERE created_at<? OR (created_at=? AND id<?)`
		args = append(args, created, created, cursor)
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT id,base_version,created_by,created_at,published_at,voice_count,enabled_count FROM journal_voice_catalog_revisions`+condition+` ORDER BY created_at DESC,id DESC LIMIT 51`, args...)
	if err != nil {
		return CatalogHistory{}, err
	}
	defer rows.Close()
	result := CatalogHistory{Revisions: []CatalogRevisionSummary{}}
	for rows.Next() {
		var published sql.NullTime
		var r CatalogRevisionSummary
		if err := rows.Scan(&r.Version, &r.BaseVersion, &r.CreatedBy, &r.CreatedAt, &published, &r.VoiceCount, &r.EnabledCount); err != nil {
			return result, err
		}
		if published.Valid {
			r.PublishedAt = &published.Time
		}
		result.Revisions = append(result.Revisions, r)
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	if len(result.Revisions) > 50 {
		result.Revisions = result.Revisions[:50]
		result.NextCursor = result.Revisions[49].Version
	}
	return result, nil
}

func catalogMutationHash(m aiconfig.Mutation, input any) ([32]byte, error) {
	if !aiconfig.ValidMutation(m) || m.Actor == uuid.Nil.String() || m.RequestID == uuid.Nil.String() {
		return [32]byte{}, ErrCatalog
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return [32]byte{}, ErrCatalog
	}
	return sha256.Sum256(raw), nil
}
func catalogReplay(ctx context.Context, q quotaQueryer, m aiconfig.Mutation, action string, hash [32]byte) ([]byte, bool, error) {
	var oldAction, state string
	var oldHash, raw []byte
	err := q.QueryRowContext(ctx, `SELECT action,request_hash,state,response_json FROM journal_voice_catalog_mutations WHERE actor_id=? AND idempotency_key=?`, m.Actor, m.Key).Scan(&oldAction, &oldHash, &state, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if oldAction != action || !bytes.Equal(oldHash, hash[:]) || state != "succeeded" {
		return nil, true, ErrCatalogConflict
	}
	return raw, true, nil
}
func catalogAudit(ctx context.Context, tx *sql.Tx, m aiconfig.Mutation, action, target, outcome string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO admin_audit_events(admin_user_id,app_id,request_id,action,target_type,target_id,outcome,metadata_json,created_at) VALUES(?,'journal',?,?,'voice_catalog',?,?, '{}',?)`, m.Actor, m.RequestID, action, target, outcome, now.UTC())
	return err
}
func (s CatalogStore) mutate(ctx context.Context, m aiconfig.Mutation, action string, input any, now time.Time, apply func(*sql.Tx, string) (CatalogRevision, error)) (CatalogRevision, error) {
	hash, err := catalogMutationHash(m, input)
	if err != nil {
		return CatalogRevision{}, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return CatalogRevision{}, err
	}
	defer tx.Rollback()
	current, err := currentCatalogVersion(ctx, tx)
	if err != nil {
		return CatalogRevision{}, err
	}
	raw, replay, err := catalogReplay(ctx, tx, m, action, hash)
	if err != nil {
		return CatalogRevision{}, err
	}
	if replay {
		var r CatalogRevision
		if err := json.Unmarshal(raw, &r); err != nil {
			return r, err
		}
		return r, tx.Commit()
	}
	r, err := apply(tx, current)
	if err != nil {
		return r, err
	}
	raw, err = json.Marshal(r)
	if err != nil {
		return r, err
	}
	if err := catalogAudit(ctx, tx, m, action, r.Version, "succeeded", now); err != nil {
		return r, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO journal_voice_catalog_mutations(actor_id,idempotency_key,action,request_hash,response_json,state,created_at) VALUES(?,?,?,?,?,'succeeded',?)`, m.Actor, m.Key, action, hash[:], raw, now.UTC())
	if err != nil {
		return r, err
	}
	return r, tx.Commit()
}

func (s CatalogStore) Draft(ctx context.Context, input CatalogDraft, m aiconfig.Mutation, now time.Time) (CatalogRevision, error) {
	if (input.BaseVersion != "" && uuid.Validate(input.BaseVersion) != nil) || len(input.Voices) > 5000 {
		return CatalogRevision{}, ErrCatalog
	}
	return s.mutate(ctx, m, "read_aloud.catalog.draft", input, now, func(tx *sql.Tx, current string) (CatalogRevision, error) {
		if current != input.BaseVersion {
			return CatalogRevision{}, ErrCatalogConflict
		}
		r := CatalogRevision{Catalog: Catalog{Version: uuid.NewString(), DefaultVoiceID: input.DefaultVoiceID, Voices: []Voice{}}, BaseVersion: current, CreatedBy: m.Actor, CreatedAt: now.UTC().Truncate(time.Microsecond)}
		seen := map[string]bool{}
		for _, configuration := range input.Voices {
			v := configuration.Voice()
			if v.Validate() != nil || seen[v.ID] {
				return r, ErrCatalog
			}
			seen[v.ID] = true
			var proof []byte
			err := tx.QueryRowContext(ctx, `SELECT document FROM journal_voice_verifications WHERE policy_sha256=?`, v.policyHash()).Scan(&proof)
			if err == nil {
				if json.Unmarshal(proof, &v.Verification) != nil || !v.IsVerified() {
					return r, ErrCatalog
				}
			} else if !errors.Is(err, sql.ErrNoRows) {
				return r, err
			}
			r.Voices = append(r.Voices, v)
		}
		raw, _ := json.Marshal(r)
		enabled := 0
		for _, v := range r.Voices {
			if v.Enabled {
				enabled++
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO journal_voice_catalog_revisions(id,document,base_version,created_by,voice_count,enabled_count,created_at) VALUES(?,?,?,?,?,?,?)`, r.Version, raw, r.BaseVersion, r.CreatedBy, len(r.Voices), enabled, r.CreatedAt)
		return r, err
	})
}
func (s CatalogStore) Publish(ctx context.Context, input CatalogPublication, m aiconfig.Mutation, now time.Time) (CatalogRevision, error) {
	if uuid.Validate(input.Revision) != nil {
		return CatalogRevision{}, ErrCatalog
	}
	return s.mutate(ctx, m, "read_aloud.catalog.publish", input, now, func(tx *sql.Tx, current string) (CatalogRevision, error) {
		if current != input.BaseVersion {
			return CatalogRevision{}, ErrCatalogConflict
		}
		r, err := catalogRevision(tx.QueryRowContext(ctx, `SELECT document,published_at FROM journal_voice_catalog_revisions WHERE id=?`, input.Revision))
		if err != nil {
			return r, err
		}
		if r.BaseVersion != current || r.PublishedAt != nil {
			return r, ErrCatalogConflict
		}
		if err := r.ValidatePublication(); err != nil {
			return r, err
		}
		published := now.UTC().Truncate(time.Microsecond)
		if _, err := tx.ExecContext(ctx, `UPDATE journal_voice_catalog_revisions SET published_at=? WHERE id=?`, published, r.Version); err != nil {
			return r, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE journal_voice_catalog_current SET revision_id=? WHERE singleton_id=1`, r.Version); err != nil {
			return r, err
		}
		r.PublishedAt = &published
		return r, nil
	})
}

// A paid probe is durably claimed before network I/O. Replaying an uncertain
// claim cannot purchase another synthesis; the operator must use a new key.
func (s CatalogStore) ClaimProbe(ctx context.Context, m aiconfig.Mutation, v Voice, now time.Time) (*Voice, error) {
	hash, err := catalogMutationHash(m, Configuration(v))
	if err != nil || v.Validate() != nil || v.BillingClass != "standard" {
		return nil, ErrCatalog
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := currentCatalogVersion(ctx, tx); err != nil {
		return nil, err
	}
	raw, replay, err := catalogReplay(ctx, tx, m, "read_aloud.voice.probe", hash)
	if err != nil {
		return nil, err
	}
	if replay {
		var result Voice
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, err
		}
		return &result, tx.Commit()
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO journal_voice_catalog_mutations(actor_id,idempotency_key,action,request_hash,state,created_at) VALUES(?,?, 'read_aloud.voice.probe',?,'running',?)`, m.Actor, m.Key, hash[:], now.UTC())
	if err != nil {
		return nil, err
	}
	if err := catalogAudit(ctx, tx, m, "read_aloud.voice.probe.start", v.ID, "succeeded", now); err != nil {
		return nil, err
	}
	return nil, tx.Commit()
}

func (s CatalogStore) FinishProbe(ctx context.Context, m aiconfig.Mutation, v Voice, successful bool, now time.Time) error {
	hash, err := catalogMutationHash(m, Configuration(v))
	if err != nil {
		return err
	}
	if successful && !v.IsVerified() {
		return ErrCatalog
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var action, state string
	var stored []byte
	if err := tx.QueryRowContext(ctx, `SELECT action,request_hash,state FROM journal_voice_catalog_mutations WHERE actor_id=? AND idempotency_key=? FOR UPDATE`, m.Actor, m.Key).Scan(&action, &stored, &state); err != nil {
		return err
	}
	if action != "read_aloud.voice.probe" || !bytes.Equal(stored, hash[:]) || state != "running" {
		return ErrCatalogConflict
	}
	state, outcome := "failed", "failed"
	if successful {
		state, outcome = "succeeded", "succeeded"
		proof, _ := json.Marshal(v.Verification)
		if _, err := tx.ExecContext(ctx, `INSERT INTO journal_voice_verifications(policy_sha256,document,verified_at) VALUES(?,?,?) ON DUPLICATE KEY UPDATE document=VALUES(document),verified_at=VALUES(verified_at)`, v.policyHash(), proof, v.Verification.VerifiedAt); err != nil {
			return err
		}
	}
	raw, _ := json.Marshal(v)
	if _, err := tx.ExecContext(ctx, `UPDATE journal_voice_catalog_mutations SET response_json=?,state=? WHERE actor_id=? AND idempotency_key=?`, raw, state, m.Actor, m.Key); err != nil {
		return err
	}
	if err := catalogAudit(ctx, tx, m, "read_aloud.voice.probe.finish", v.ID, outcome, now); err != nil {
		return err
	}
	return tx.Commit()
}
