package illustrationstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/tellyouwhat/backend/internal/journal/illustration"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
)

type Store struct {
	Database *sql.DB
	Cipher   *mysqlstore.PayloadCipher
}

func illustrationAAD(owner, id string) []byte {
	return []byte(fmt.Sprintf("journal-illustration:%d:%s:%s", len(owner), owner, id))
}

func (s Store) encode(job illustration.Job) ([]byte, []byte, error) {
	if job.Task.AppID != "journal" || job.Task.OwnerID == "" || len(job.Task.OwnerID) > 512 || job.Task.Revision == 0 {
		return nil, nil, illustration.ErrInput
	}
	data, err := json.Marshal(job)
	if err != nil {
		return nil, nil, err
	}
	if len(data) > 70_000_000 {
		return nil, nil, illustration.ErrInput
	}
	return s.Cipher.Encrypt(data, illustrationAAD(job.Task.OwnerID, job.Task.ID))
}

func (s Store) Create(ctx context.Context, job illustration.Job) (illustration.Job, error) {
	if job.Task.Revision != 1 || job.Task.State != illustration.TaskQueued || job.Input == nil || job.Result != nil {
		return illustration.Job{}, illustration.ErrInput
	}
	encrypted, nonce, err := s.encode(job)
	if err != nil {
		return illustration.Job{}, err
	}
	_, err = s.Database.ExecContext(ctx, `INSERT INTO journal_illustration_jobs
  (owner_key_id,request_id,revision,state,created_at,expires_at,has_payload,ciphertext,nonce) VALUES (?,?,1,'queued',?,?,true,?,?)`, job.Task.OwnerID, job.Task.ID, job.Task.CreatedAt, job.Task.ExpiresAt, encrypted, nonce)
	if err == nil {
		return job, nil
	}
	var duplicate *mysql.MySQLError
	if !errors.As(err, &duplicate) || duplicate.Number != 1062 {
		return illustration.Job{}, err
	}
	existing, err := s.Get(ctx, job.Task.OwnerID, job.Task.ID)
	if err != nil {
		return illustration.Job{}, err
	}
	if !existing.Task.SameRequest(job.Task) {
		return illustration.Job{}, illustration.ErrTaskConflict
	}
	return existing, nil
}

func (s Store) Get(ctx context.Context, owner, id string) (illustration.Job, error) {
	var encrypted, nonce []byte
	var revision uint64
	var state string
	err := s.Database.QueryRowContext(ctx, `SELECT ciphertext,nonce,revision,state FROM journal_illustration_jobs WHERE owner_key_id=? AND request_id=?`, owner, id).Scan(&encrypted, &nonce, &revision, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return illustration.Job{}, illustration.ErrNotFound
	}
	if err != nil {
		return illustration.Job{}, err
	}
	data, err := s.Cipher.Decrypt(encrypted, nonce, illustrationAAD(owner, id))
	if err != nil {
		return illustration.Job{}, err
	}
	var job illustration.Job
	if err = json.Unmarshal(data, &job); err != nil {
		return illustration.Job{}, err
	}
	if job.Task.AppID != "journal" || job.Task.OwnerID != owner || job.Task.ID != id || job.Task.Revision != revision || string(job.Task.State) != state {
		return illustration.Job{}, illustration.ErrTaskConflict
	}
	return job, nil
}

func (s Store) Save(ctx context.Context, job illustration.Job, previous uint64) error {
	if previous == 0 || job.Task.Revision != previous+1 {
		return illustration.ErrTaskConflict
	}
	encrypted, nonce, err := s.encode(job)
	if err != nil {
		return err
	}
	var lease any
	if !job.LeaseUntil.IsZero() {
		lease = job.LeaseUntil
	}
	result, err := s.Database.ExecContext(ctx, `UPDATE journal_illustration_jobs SET revision=?,state=?,expires_at=?,lease_until=?,has_payload=?,ciphertext=?,nonce=? WHERE owner_key_id=? AND request_id=? AND revision=?`, job.Task.Revision, job.Task.State, job.Task.ExpiresAt, lease, job.Input != nil || job.Result != nil, encrypted, nonce, job.Task.OwnerID, job.Task.ID, previous)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return illustration.ErrTaskConflict
	}
	return nil
}

func (s Store) Pending(ctx context.Context, now time.Time, limit int) ([]illustration.Task, error) {
	if limit < 1 || limit > 100 {
		return nil, illustration.ErrInput
	}
	rows, err := s.Database.QueryContext(ctx, `SELECT owner_key_id,request_id FROM journal_illustration_jobs WHERE (state='queued' AND (lease_until IS NULL OR lease_until<=?)) OR (state='running' AND lease_until<=?) OR (has_payload=true AND expires_at<=?) ORDER BY expires_at,request_id LIMIT ?`, now, now, now, limit)
	if err != nil {
		return nil, err
	}
	var identities [][2]string
	for rows.Next() {
		var identity [2]string
		if err = rows.Scan(&identity[0], &identity[1]); err != nil {
			rows.Close()
			return nil, err
		}
		identities = append(identities, identity)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	tasks := make([]illustration.Task, 0, len(identities))
	for _, identity := range identities {
		tasks = append(tasks, illustration.Task{OwnerID: identity[0], ID: identity[1]})
	}
	return tasks, nil
}

func (s Store) Ready(ctx context.Context) error {
	rows, err := s.Database.QueryContext(ctx, `SELECT owner_key_id,request_id,revision,state,expires_at,lease_until,has_payload,ciphertext,nonce FROM journal_illustration_jobs LIMIT 0`)
	if err != nil {
		return err
	}
	return rows.Close()
}

// Expire is also run by scheduled maintenance, even when image generation is disabled.
func (s Store) Expire(ctx context.Context, now time.Time) (int, error) {
	total := 0
	for {
		rows, err := s.Database.QueryContext(ctx, `SELECT owner_key_id,request_id FROM journal_illustration_jobs WHERE has_payload=true AND expires_at<=? ORDER BY expires_at LIMIT 100`, now)
		if err != nil {
			return total, err
		}
		var identities [][2]string
		for rows.Next() {
			var pair [2]string
			if err = rows.Scan(&pair[0], &pair[1]); err != nil {
				rows.Close()
				return total, err
			}
			identities = append(identities, pair)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return total, err
		}
		if len(identities) == 0 {
			return total, nil
		}
		service := illustration.Service{Store: s, Now: func() time.Time { return now }}
		for _, pair := range identities {
			if _, err := service.Get(ctx, pair[0], pair[1]); err != nil && !errors.Is(err, illustration.ErrNotFound) {
				return total, err
			}
			total++
		}
	}
}
