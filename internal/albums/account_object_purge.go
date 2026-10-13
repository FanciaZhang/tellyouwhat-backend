package albums

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

const accountPurgeLease = 5 * time.Minute

type accountPurgePlan struct {
	Versions []accountPurgeVersion `json:"versions,omitempty"`
	Uploads  []accountPurgeUpload  `json:"uploads,omitempty"`
}

func (p accountPurgePlan) count() int { return len(p.Versions) + len(p.Uploads) }
func (p accountPurgePlan) validate(owner string, area int) error {
	if area < 0 || area >= len(accountPurgeAreas) || p.count() > 1000 || len(p.Versions) > 0 && len(p.Uploads) > 0 {
		return ErrConflict
	}
	prefix, err := accountPurgePrefix(owner, accountPurgeAreas[area])
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, v := range p.Versions {
		id := v.Target.Key + "\x00" + v.Version.VersionID
		if !v.Target.valid(owner) || !strings.HasPrefix(v.Target.Key, prefix) || seen[id] || validateCleanupPlan(DerivedCleanupPlan{Terminal: true, Versions: []DerivedObjectVersion{v.Version}}) != nil {
			return ErrConflict
		}
		seen[id] = true
	}
	for _, u := range p.Uploads {
		id := u.Target.Key + "\x00" + u.UploadID
		if !u.Target.valid(owner) || !strings.HasPrefix(u.Target.Key, prefix) || seen[id] || !validCOSMultipartID(u.UploadID) {
			return ErrConflict
		}
		seen[id] = true
	}
	return nil
}

type accountObjectPurge struct {
	Account, Request, State, Token string
	Area, Action                   int
	Until, RemovedAt               time.Time
	Plan                           *accountPurgePlan
}

const accountPurgeColumns = `account_id,request_id,state,area_index,action_index,lease_token,lease_until,objects_removed_at,plan_json`

func scanAccountPurge(row rowScanner) (accountObjectPurge, error) {
	var p accountObjectPurge
	var token sql.NullString
	var until, removed sql.NullTime
	var data []byte
	if err := row.Scan(&p.Account, &p.Request, &p.State, &p.Area, &p.Action, &token, &until, &removed, &data); err != nil {
		return p, err
	}
	p.Token, p.Until, p.RemovedAt = token.String, until.Time, removed.Time
	if len(data) > 0 {
		p.Plan = &accountPurgePlan{}
		if len(data) > 4<<20 || json.Unmarshal(data, p.Plan) != nil || p.Plan.validate(p.Account, p.Area) != nil {
			return p, ErrConflict
		}
	}
	if !validOwner(p.Account) || !validOwner(p.Request) || p.Area < 0 || p.Area > len(accountPurgeAreas) || p.Action < 0 ||
		(p.State != "queued" && p.State != "running" && p.State != "objects_removed") ||
		(p.State == "running") != (validOwner(p.Token) && !p.Until.IsZero()) ||
		(p.State != "running" && (p.Token != "" || !p.Until.IsZero())) ||
		(p.State == "objects_removed") != (p.Area == len(accountPurgeAreas)) ||
		(p.State == "objects_removed") != !p.RemovedAt.IsZero() ||
		(p.Plan == nil && p.Action != 0) || p.Plan != nil && p.Action > p.Plan.count() {
		return p, ErrConflict
	}
	return p, nil
}

// The accepted deletion fences all new write permits. Media quiescence is
// checked again at claim and prefix completion; every action binds the same
// request, storage fence and readiness receipt under account/storage locks.
func lockAccountPurgeScope(ctx context.Context, tx *sql.Tx, owner, request string, at time.Time) error {
	if !validOwner(owner) || !validOwner(request) {
		return ErrInvalidManifest
	}
	if err := lockDeletingAccount(ctx, tx, owner, request); err != nil {
		return err
	}
	if _, err := lockAccount(ctx, tx, owner); err != nil {
		return err
	}
	var requested time.Time
	var fenced, ready sql.NullTime
	if err := tx.QueryRowContext(ctx, `SELECT d.requested_at,s.deletion_requested_at,d.media_quiesced_at FROM album_account_deletions d JOIN album_storage_accounts s ON s.owner_id=d.account_id WHERE d.account_id=? AND d.request_id=? FOR UPDATE`, owner, request).Scan(&requested, &fenced, &ready); err != nil {
		return err
	}
	if !fenced.Valid || !requested.Equal(fenced.Time) || !ready.Valid || ready.Time.Before(requested) || at.Before(ready.Time) {
		return ErrDerivedPending
	}
	return nil
}

func (r *MySQLUploads) accountObjectPurge(ctx context.Context, owner string) (accountObjectPurge, error) {
	return scanAccountPurge(r.db.QueryRowContext(ctx, `SELECT `+accountPurgeColumns+` FROM album_account_object_purges WHERE account_id=?`, owner))
}

func (r *MySQLUploads) claimAccountObjectPurge(ctx context.Context, owner, request string, at time.Time) (accountObjectPurge, error) {
	var p accountObjectPurge
	at = at.UTC().Truncate(time.Microsecond)
	if at.IsZero() {
		return p, ErrInvalidManifest
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return p, err
	}
	defer tx.Rollback()
	if err = lockAccountPurgeScope(ctx, tx, owner, request, at); err != nil {
		return p, err
	}
	if err = requireAccountMediaQuiescence(ctx, tx, owner, request); err != nil {
		return p, err
	}
	p, err = scanAccountPurge(tx.QueryRowContext(ctx, `SELECT `+accountPurgeColumns+` FROM album_account_object_purges WHERE account_id=? FOR UPDATE`, owner))
	if errors.Is(err, sql.ErrNoRows) {
		p = accountObjectPurge{Account: owner, Request: request, State: "queued"}
		_, err = tx.ExecContext(ctx, `INSERT INTO album_account_object_purges(account_id,request_id,state,updated_at) VALUES (?,?,'queued',?)`, owner, request, at)
	}
	if err != nil {
		return p, err
	}
	if p.Request != request {
		return p, ErrConflict
	}
	if p.State == "objects_removed" {
		return p, tx.Commit()
	}
	if p.State == "running" && p.Until.After(at) {
		return p, ErrLease
	}
	p.State, p.Token, p.Until = "running", uuid.NewString(), at.Add(accountPurgeLease)
	if _, err = tx.ExecContext(ctx, `UPDATE album_account_object_purges SET state='running',lease_token=?,lease_until=?,updated_at=? WHERE account_id=?`, p.Token, p.Until, at, owner); err != nil {
		return p, err
	}
	return p, tx.Commit()
}

func sameAccountPurgeLease(a, b accountObjectPurge, at time.Time) bool {
	x, _ := json.Marshal(a.Plan)
	y, _ := json.Marshal(b.Plan)
	return a.Account == b.Account && a.Request == b.Request && a.Area == b.Area && a.Action == b.Action && a.State == "running" &&
		validOwner(b.Token) && a.Token == b.Token && a.Until.Equal(b.Until) && a.Until.After(at) && string(x) == string(y)
}

// Mutations use the full saved plan as their expected value, so an old worker
// cannot acknowledge a different batch or advance an empty plan after restart.
func (r *MySQLUploads) mutateAccountPurge(ctx context.Context, expected accountObjectPurge, at time.Time, change func(*sql.Tx, *accountObjectPurge) error) (accountObjectPurge, error) {
	var p accountObjectPurge
	at = at.UTC().Truncate(time.Microsecond)
	if at.IsZero() {
		return p, ErrInvalidManifest
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return p, err
	}
	defer tx.Rollback()
	if err = lockAccountPurgeScope(ctx, tx, expected.Account, expected.Request, at); err != nil {
		return p, err
	}
	p, err = scanAccountPurge(tx.QueryRowContext(ctx, `SELECT `+accountPurgeColumns+` FROM album_account_object_purges WHERE account_id=? FOR UPDATE`, expected.Account))
	if err != nil {
		return p, err
	}
	if !sameAccountPurgeLease(p, expected, at) {
		return p, ErrLease
	}
	if change == nil {
		return p, tx.Commit()
	}
	if err = change(tx, &p); err != nil {
		return p, err
	}
	oldPlan, _ := json.Marshal(expected.Plan)
	newPlan, _ := json.Marshal(p.Plan)
	planChanged := string(oldPlan) != string(newPlan)
	var plan, token, until, removed any
	if p.Plan != nil {
		data, e := json.Marshal(p.Plan)
		if e != nil || len(data) > 4<<20 || p.Plan.validate(p.Account, p.Area) != nil {
			return p, ErrConflict
		}
		if planChanged {
			plan = data
		}
	}
	if p.State == "running" {
		token, until = p.Token, p.Until
	}
	if !p.RemovedAt.IsZero() {
		removed = p.RemovedAt
	}
	_, err = tx.ExecContext(ctx, `UPDATE album_account_object_purges SET state=?,area_index=?,action_index=?,plan_json=IF(?,?,plan_json),lease_token=?,lease_until=?,objects_removed_at=?,updated_at=? WHERE account_id=?`, p.State, p.Area, p.Action, planChanged, plan, token, until, removed, at, p.Account)
	if err != nil {
		return p, err
	}
	return p, tx.Commit()
}

type accountPurgeStorage interface {
	listAccountPurgeVersions(context.Context, string, string) ([]accountPurgeVersion, error)
	listAccountPurgeUploads(context.Context, string, string) ([]accountPurgeUpload, error)
	deletePurgeVersion(context.Context, string, purgeTarget, DerivedObjectVersion) error
	abortPurgeUpload(context.Context, string, purgeTarget, string) error
}

func accountPrefixEmpty(ctx context.Context, s accountPurgeStorage, owner, area string) (bool, error) {
	u, err := s.listAccountPurgeUploads(ctx, owner, area)
	if err != nil {
		return false, err
	}
	v, err := s.listAccountPurgeVersions(ctx, owner, area)
	if err != nil {
		return false, err
	}
	return len(u) == 0 && len(v) == 0, nil
}

// Internal executor only. Activation still requires metadata/backup erasure,
// restricted deletion credentials and end-to-end acceptance. No HTTP entrypoint.
func (r *MySQLUploads) runAccountObjectPurgeStep(ctx context.Context, owner, request string, s accountPurgeStorage, now func() time.Time) error {
	if s == nil || now == nil {
		return ErrInvalidManifest
	}
	p, err := r.claimAccountObjectPurge(ctx, owner, request, now().UTC())
	if err != nil {
		return err
	}
	if p.State == "objects_removed" {
		return nil
	}
	area := accountPurgeAreas[p.Area]
	if p.Plan == nil {
		u, e := s.listAccountPurgeUploads(ctx, owner, area)
		if e != nil {
			return e
		}
		plan := accountPurgePlan{Uploads: u}
		if len(u) == 0 {
			plan.Versions, err = s.listAccountPurgeVersions(ctx, owner, area)
			if err != nil {
				return err
			}
		}
		p, err = r.mutateAccountPurge(ctx, p, now().UTC(), func(_ *sql.Tx, current *accountObjectPurge) error { current.Plan = &plan; return nil })
		if err != nil {
			return err
		}
	}
	// Limit each invocation while retaining a 1000-item inventory page.
	for count := 0; p.Action < p.Plan.count() && count < 100; count++ {
		p, err = r.mutateAccountPurge(ctx, p, now().UTC(), nil)
		if err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		if !p.Until.After(now().UTC()) {
			return ErrLease
		}
		if len(p.Plan.Uploads) > 0 {
			u := p.Plan.Uploads[p.Action]
			err = s.abortPurgeUpload(ctx, owner, u.Target, u.UploadID)
		} else {
			v := p.Plan.Versions[p.Action]
			err = s.deletePurgeVersion(ctx, owner, v.Target, v.Version)
		}
		if err != nil {
			return err
		}
		p, err = r.mutateAccountPurge(ctx, p, now().UTC(), func(_ *sql.Tx, current *accountObjectPurge) error { current.Action++; return nil })
		if err != nil {
			return err
		}
	}
	next := p.Area
	if p.Plan.count() == 0 {
		empty, e := accountPrefixEmpty(ctx, s, owner, area)
		if e != nil {
			return e
		}
		if empty {
			next++
		}
		// Recheck all areas before recording object removal. Persisted emptiness
		// from an earlier step or a crash cannot be used as a final observation.
		if next == len(accountPurgeAreas) {
			for i, area := range accountPurgeAreas {
				empty, e = accountPrefixEmpty(ctx, s, owner, area)
				if e != nil {
					return e
				}
				if !empty {
					next = i
					break
				}
			}
		}
	}
	_, err = r.mutateAccountPurge(ctx, p, now().UTC(), func(tx *sql.Tx, current *accountObjectPurge) error {
		if err := requireAccountMediaQuiescence(ctx, tx, owner, request); err != nil {
			return err
		}
		if current.Action == current.Plan.count() {
			current.Plan = nil
			current.Action = 0
		}
		current.Area = next
		current.State, current.Token, current.Until = "queued", "", time.Time{}
		if next == len(accountPurgeAreas) {
			current.State, current.RemovedAt = "objects_removed", now().UTC().Truncate(time.Microsecond)
		}
		return nil
	})
	return err
}
