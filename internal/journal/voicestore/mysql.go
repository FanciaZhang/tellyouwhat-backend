package voicestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/tellyouwhat/backend/internal/journal/voice"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
)

type Store struct {
	Database *sql.DB
	Cipher   *mysqlstore.PayloadCipher
}

func aad(owner, id string) []byte {
	return []byte(fmt.Sprintf("journal-voice-organization:%d:%s:%s", len(owner), owner, id))
}
func (s Store) encode(job voice.OrganizationJob) ([]byte, []byte, error) {
	if job.OwnerID == "" || job.Principal.AppID != "journal" || job.Principal.KeyID != job.OwnerID || job.Revision < 1 {
		return nil, nil, voice.ErrInvalid
	}
	data, err := json.Marshal(job)
	if err != nil {
		return nil, nil, err
	}
	if len(data) > 8*voice.MaxOrganizationBytes {
		return nil, nil, voice.ErrInvalid
	}
	return s.Cipher.Encrypt(data, aad(job.OwnerID, job.ID))
}

func (s Store) Create(ctx context.Context, job voice.OrganizationJob) (voice.OrganizationJob, error) {
	if job.Revision != 1 || !(job.State == "queued" && job.Plan != nil || job.State == "cancelled" && job.Plan == nil) {
		return voice.OrganizationJob{}, voice.ErrInvalid
	}
	encrypted, nonce, err := s.encode(job)
	if err != nil {
		return voice.OrganizationJob{}, err
	}
	tx, err := s.Database.BeginTx(ctx, nil)
	if err != nil {
		return voice.OrganizationJob{}, err
	}
	defer tx.Rollback()
	// Serialize admission per owner and bound queued private input, without limiting other users.
	var owner string
	if err = tx.QueryRowContext(ctx, `SELECT key_id FROM app_attest_keys WHERE app_id='journal' AND key_id=? FOR UPDATE`, job.OwnerID).Scan(&owner); err != nil {
		return voice.OrganizationJob{}, err
	}
	var exists int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM journal_voice_organization_jobs WHERE owner_key_id=? AND request_id=?`, job.OwnerID, job.ID).Scan(&exists); err != nil {
		return voice.OrganizationJob{}, err
	}
	if exists == 0 && job.State == "queued" {
		var active int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM journal_voice_organization_jobs WHERE owner_key_id=? AND state IN ('queued','running') AND expires_at>?`, job.OwnerID, job.CreatedAt).Scan(&active); err != nil {
			return voice.OrganizationJob{}, err
		}
		if active >= 4 {
			return voice.OrganizationJob{}, voice.ErrOrganizationConflict
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO journal_voice_organization_jobs
      (owner_key_id,request_id,request_digest,revision,state,created_at,expires_at,available_at,has_payload,ciphertext,nonce)
      VALUES (?,?,?,1,?,?,?,?,true,?,?)`, job.OwnerID, job.ID, job.Digest, job.State, job.CreatedAt, job.ExpiresAt, job.AvailableAt, encrypted, nonce)
	if err == nil {
		if err = tx.Commit(); err != nil {
			return voice.OrganizationJob{}, err
		}
		return job, nil
	}
	var duplicate *mysql.MySQLError
	if !errors.As(err, &duplicate) || duplicate.Number != 1062 {
		return voice.OrganizationJob{}, err
	}
	_ = tx.Rollback()
	existing, err := s.Get(ctx, job.OwnerID, job.ID)
	if err == nil && existing.Digest != job.Digest {
		return voice.OrganizationJob{}, voice.ErrOrganizationConflict
	}
	return existing, err
}

func (s Store) Get(ctx context.Context, owner, id string) (voice.OrganizationJob, error) {
	var job voice.OrganizationJob
	var encrypted, nonce []byte
	err := s.Database.QueryRowContext(ctx, `SELECT request_digest,revision,state,created_at,expires_at,available_at,lease_until,ciphertext,nonce FROM journal_voice_organization_jobs WHERE owner_key_id=? AND request_id=?`, owner, id).Scan(&job.Digest, &job.Revision, &job.State, &job.CreatedAt, &job.ExpiresAt, &job.AvailableAt, new(sql.NullTime), &encrypted, &nonce)
	if errors.Is(err, sql.ErrNoRows) {
		return job, voice.ErrOrganizationNotFound
	}
	if err != nil {
		return job, err
	}
	job.ID, job.OwnerID = id, owner
	if len(encrypted) == 0 {
		if job.State != "expired" {
			return voice.OrganizationJob{}, voice.ErrOrganizationConflict
		}
		return job, nil
	}
	data, err := s.Cipher.Decrypt(encrypted, nonce, aad(owner, id))
	if err != nil {
		return voice.OrganizationJob{}, err
	}
	var decoded voice.OrganizationJob
	if json.Unmarshal(data, &decoded) != nil || decoded.ID != id || decoded.OwnerID != owner || decoded.Principal.AppID != "journal" ||
		decoded.Principal.KeyID != owner || decoded.Revision != job.Revision || decoded.State != job.State || decoded.Digest != job.Digest {
		return voice.OrganizationJob{}, voice.ErrOrganizationConflict
	}
	return decoded, nil
}

func (s Store) Save(ctx context.Context, job voice.OrganizationJob, previous uint64) error {
	if previous == 0 || job.Revision != previous+1 {
		return voice.ErrOrganizationConflict
	}
	encrypted, nonce, err := s.encode(job)
	if err != nil {
		return err
	}
	var lease any
	if !job.LeaseUntil.IsZero() {
		lease = job.LeaseUntil
	}
	hasPayload := job.Plan != nil || len(job.Progress.Results) > 0 || len(job.Progress.Paragraphs) > 0
	result, err := s.Database.ExecContext(ctx, `UPDATE journal_voice_organization_jobs SET revision=?,state=?,available_at=?,lease_until=?,has_payload=?,ciphertext=?,nonce=? WHERE owner_key_id=? AND request_id=? AND revision=? AND request_digest=?`, job.Revision, job.State, job.AvailableAt, lease, hasPayload, encrypted, nonce, job.OwnerID, job.ID, previous, job.Digest)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return voice.ErrOrganizationConflict
	}
	return nil
}

func (s Store) Pending(ctx context.Context, now time.Time, limit int) ([][2]string, error) {
	if limit < 1 || limit > 100 {
		return nil, voice.ErrInvalid
	}
	rows, err := s.Database.QueryContext(ctx, `SELECT owner_key_id,request_id FROM journal_voice_organization_jobs WHERE (state='queued' AND available_at<=? AND expires_at>?) OR (state='running' AND lease_until<=?) OR (has_payload=true AND expires_at<=?) ORDER BY available_at,created_at LIMIT ?`, now, now, now, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := [][2]string{}
	for rows.Next() {
		var pair [2]string
		if err = rows.Scan(&pair[0], &pair[1]); err != nil {
			return nil, err
		}
		result = append(result, pair)
	}
	return result, rows.Err()
}

func (s Store) ForgetOwner(ctx context.Context, owner string) error {
	if owner == "" {
		return voice.ErrInvalid
	}
	_, err := s.Database.ExecContext(ctx, `DELETE FROM journal_voice_organization_jobs WHERE owner_key_id=?`, owner)
	return err
}

func (s Store) Ready(ctx context.Context) error {
	rows, err := s.Database.QueryContext(ctx, `SELECT owner_key_id,request_id,request_digest,revision,state,available_at,expires_at,lease_until,has_payload,ciphertext,nonce FROM journal_voice_organization_jobs LIMIT 0`)
	if err != nil {
		return err
	}
	return rows.Close()
}

// Maintenance scrubs private input/results even when voice generation is disabled.
func (s Store) Expire(ctx context.Context, now time.Time) (int64, error) {
	result, err := s.Database.ExecContext(ctx, `UPDATE journal_voice_organization_jobs SET state='expired',revision=revision+1,has_payload=false,ciphertext=NULL,nonce=NULL,lease_until=NULL WHERE expires_at<=? AND (has_payload=true OR state<>'expired')`, now)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	_, err = s.Database.ExecContext(ctx, `DELETE FROM journal_voice_organization_jobs WHERE expires_at<?`, now.Add(-7*24*time.Hour))
	return n, err
}
