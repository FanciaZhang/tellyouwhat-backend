package readaloud

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
)

var (
	ErrQuotaExceeded   = errors.New("read-aloud character allowance exceeded")
	ErrRequestConflict = errors.New("read-aloud request identifier conflict")
	ErrRequestNotFound = errors.New("read-aloud request not found")
	subjectPattern     = regexp.MustCompile(`^[A-Za-z0-9:_./=+\-]{1,128}$`)
)

type Subject struct{ AppID, ID string }

func (s Subject) Valid() bool { return s.AppID == "journal" && subjectPattern.MatchString(s.ID) }

// Grant comes exclusively from the server's verified entitlement or scoped
// test identity. Clients never supply their period or allowance.
type Grant struct {
	Subject    Subject
	Purpose    string
	Start, End time.Time
	Limit      int
}

func (g Grant) Valid(now time.Time) bool {
	return g.Subject.Valid() && (g.Purpose == "journal" || g.Purpose == "preview") && g.Limit >= 0 && g.Limit <= 100_000_000 && !g.Start.IsZero() &&
		!g.Start.After(now) && g.End.After(now) && g.End.Sub(g.Start) <= 370*24*time.Hour
}

func (g Grant) normalized() Grant {
	g.Start = g.Start.UTC().Truncate(time.Microsecond)
	g.End = g.End.UTC().Truncate(time.Microsecond)
	return g
}

type UnitRequest struct {
	ID         string
	Hash       [32]byte `json:"-"`
	Purpose    string
	Characters int
}

func NewUnitRequest(input Synthesis, voiceRevision, purpose string) (UnitRequest, error) {
	if !input.Valid() || voiceRevision == "" || (purpose != "journal" && purpose != "preview") {
		return UnitRequest{}, ErrInput
	}
	characters, _ := CharacterCount(input.Text)
	// A request ID binds all synthesis settings and the exact input. Only the
	// digest is stored; replay cannot change text, voice, revision or purpose.
	document, _ := json.Marshal(struct {
		Text, Voice, Revision, Style, Dialect, Language, Purpose string
		Pitch                                                    int
	}{input.Text, input.VoiceID, voiceRevision, input.Style, input.Dialect, input.Language, purpose, input.Pitch})
	return UnitRequest{uuid.MustParse(input.RequestID).String(), sha256.Sum256(document), purpose, characters}, nil
}
func (r UnitRequest) valid() bool {
	id, err := uuid.Parse(r.ID)
	return err == nil && id != uuid.Nil && r.ID == id.String() && r.Hash != [32]byte{} && r.Characters > 0 && r.Characters <= MaximumTextBytes && (r.Purpose == "journal" || r.Purpose == "preview")
}

type RequestRecord struct {
	UnitRequest
	Subject              Subject
	PeriodStart          time.Time
	State                string
	ActualCharacters     int
	UsageKnown           bool
	CreatedAt, ExpiresAt time.Time
}
type CharacterUsage struct {
	Limit     int       `json:"limit"`
	Used      int       `json:"used"`
	Reserved  int       `json:"reserved"`
	Remaining int       `json:"remaining"`
	PeriodEnd time.Time `json:"periodEnd"`
}

type QuotaStore struct{ DB *sql.DB }
type quotaQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func requestFrom(ctx context.Context, q quotaQueryer, id string, locked bool) (RequestRecord, error) {
	var r RequestRecord
	var digest []byte
	suffix := ""
	if locked {
		suffix = " FOR UPDATE"
	}
	err := q.QueryRowContext(ctx, `SELECT id,app_id,subject_id,period_start,input_hash,purpose,reserved_characters,actual_characters,usage_known,state,created_at,expires_at FROM journal_read_aloud_requests WHERE id=?`+suffix, id).
		Scan(&r.ID, &r.Subject.AppID, &r.Subject.ID, &r.PeriodStart, &digest, &r.Purpose, &r.Characters, &r.ActualCharacters, &r.UsageKnown, &r.State, &r.CreatedAt, &r.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrRequestNotFound
	}
	if err == nil && len(digest) != sha256.Size {
		return r, ErrInput
	}
	copy(r.Hash[:], digest)
	return r, err
}
func (s QuotaStore) Get(ctx context.Context, subject Subject, id string) (RequestRecord, error) {
	if !subject.Valid() || uuid.Validate(id) != nil {
		return RequestRecord{}, ErrInput
	}
	id = uuid.MustParse(id).String()
	r, err := requestFrom(ctx, s.DB, id, false)
	if err == nil && r.Subject != subject {
		return RequestRecord{}, ErrRequestNotFound
	}
	return r, err
}

func sameRequest(r RequestRecord, g Grant, input UnitRequest) bool {
	return r.Subject == g.Subject && r.Hash == input.Hash && r.Purpose == input.Purpose && g.Purpose == input.Purpose && r.Characters == input.Characters
}

// Begin is the only synthesis claim. Exactly one caller receives replay=false;
// every later caller inspects the existing state, including after a restart.
func (s QuotaStore) Begin(ctx context.Context, grant Grant, input UnitRequest, now time.Time) (record RequestRecord, replay bool, err error) {
	grant = grant.normalized()
	if !grant.Valid(now) || !input.valid() || input.Purpose != grant.Purpose {
		return record, false, ErrInput
	}
	if existing, e := requestFrom(ctx, s.DB, input.ID, false); e == nil {
		if !sameRequest(existing, grant, input) {
			return record, true, ErrRequestConflict
		}
		return existing, true, nil
	} else if !errors.Is(e, ErrRequestNotFound) {
		return record, false, e
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return record, false, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO journal_read_aloud_accounts(app_id,subject_id,purpose,period_start,period_end,character_limit) VALUES(?,?,?,?,?,?) ON DUPLICATE KEY UPDATE app_id=VALUES(app_id)`, grant.Subject.AppID, grant.Subject.ID, grant.Purpose, grant.Start, grant.End, grant.Limit)
	if err != nil {
		return record, false, err
	}
	var used, reserved int
	var end time.Time
	err = tx.QueryRowContext(ctx, `SELECT used_characters,reserved_characters,period_end FROM journal_read_aloud_accounts WHERE app_id=? AND subject_id=? AND purpose=? AND period_start=? FOR UPDATE`, grant.Subject.AppID, grant.Subject.ID, grant.Purpose, grant.Start).Scan(&used, &reserved, &end)
	if err != nil {
		return record, false, err
	}
	if !end.Equal(grant.End.UTC()) {
		return record, false, ErrRequestConflict
	}
	if existing, e := requestFrom(ctx, tx, input.ID, true); e == nil {
		if !sameRequest(existing, grant, input) {
			return record, true, ErrRequestConflict
		}
		return existing, true, tx.Commit()
	} else if !errors.Is(e, ErrRequestNotFound) {
		return record, false, e
	}
	if input.Characters > grant.Limit-used-reserved {
		return record, false, ErrQuotaExceeded
	}
	record = RequestRecord{UnitRequest: input, Subject: grant.Subject, PeriodStart: grant.Start.UTC(), State: "reserved", CreatedAt: now.UTC(), ExpiresAt: now.Add(2 * time.Minute).UTC()}
	_, err = tx.ExecContext(ctx, `INSERT INTO journal_read_aloud_requests(id,app_id,subject_id,period_start,input_hash,purpose,reserved_characters,state,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, record.ID, record.Subject.AppID, record.Subject.ID, record.PeriodStart, record.Hash[:], record.Purpose, record.Characters, record.State, record.CreatedAt, record.ExpiresAt)
	if err != nil {
		var duplicate *mysql.MySQLError
		if errors.As(err, &duplicate) && duplicate.Number == 1062 {
			err = ErrRequestConflict
		}
		return record, false, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE journal_read_aloud_accounts SET character_limit=?,reserved_characters=reserved_characters+? WHERE app_id=? AND subject_id=? AND purpose=? AND period_start=?`, grant.Limit, input.Characters, grant.Subject.AppID, grant.Subject.ID, grant.Purpose, grant.Start)
	if err != nil {
		return record, false, err
	}
	return record, false, tx.Commit()
}

func (s QuotaStore) Usage(ctx context.Context, grant Grant, now time.Time) (CharacterUsage, error) {
	grant = grant.normalized()
	result := CharacterUsage{Limit: grant.Limit, Remaining: grant.Limit, PeriodEnd: grant.End}
	if !grant.Valid(now) {
		return CharacterUsage{}, ErrInput
	}
	err := s.DB.QueryRowContext(ctx, `SELECT used_characters,reserved_characters FROM journal_read_aloud_accounts WHERE app_id=? AND subject_id=? AND purpose=? AND period_start=?`, grant.Subject.AppID, grant.Subject.ID, grant.Purpose, grant.Start).Scan(&result.Used, &result.Reserved)
	if errors.Is(err, sql.ErrNoRows) {
		return result, nil
	}
	result.Remaining = max(0, grant.Limit-result.Used-result.Reserved)
	return result, err
}

func (s QuotaStore) MarkRunning(ctx context.Context, subject Subject, id string) error {
	if !subject.Valid() || uuid.Validate(id) != nil {
		return ErrInput
	}
	id = uuid.MustParse(id).String()
	result, err := s.DB.ExecContext(ctx, `UPDATE journal_read_aloud_requests SET state='running' WHERE id=? AND app_id=? AND subject_id=? AND state='reserved' AND expires_at>UTC_TIMESTAMP(6)`, id, subject.AppID, subject.ID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrRequestConflict
	}
	return nil
}

// Unknown usage keeps its reservation even when audio succeeded. Result state
// and billing certainty are independent; authoritative usage settles only once.
func (s QuotaStore) Finish(ctx context.Context, subject Subject, id, state string, actual int, known bool) error {
	if !subject.Valid() || actual < 0 || actual > MaximumTextBytes*2 ||
		(state != "succeeded" && state != "failed" && state != "cancelled" && state != "unknown") || (!known && actual != 0) {
		return ErrInput
	}
	before, err := s.Get(ctx, subject, id)
	if err != nil {
		return err
	}
	id = before.ID
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var reserved int
	err = tx.QueryRowContext(ctx, `SELECT reserved_characters FROM journal_read_aloud_accounts WHERE app_id=? AND subject_id=? AND purpose=? AND period_start=? FOR UPDATE`, subject.AppID, subject.ID, before.Purpose, before.PeriodStart).Scan(&reserved)
	if err != nil {
		return err
	}
	r, err := requestFrom(ctx, tx, id, true)
	if err != nil {
		return err
	}
	if r.UsageKnown {
		if !known || r.State != state || r.ActualCharacters != actual {
			return ErrRequestConflict
		}
		return tx.Commit()
	}
	if known {
		if reserved < r.Characters {
			return ErrRequestConflict
		}
		_, err = tx.ExecContext(ctx, `UPDATE journal_read_aloud_accounts SET reserved_characters=reserved_characters-?,used_characters=used_characters+? WHERE app_id=? AND subject_id=? AND purpose=? AND period_start=?`, r.Characters, actual, subject.AppID, subject.ID, r.Purpose, r.PeriodStart)
		if err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE journal_read_aloud_requests SET state=?,actual_characters=?,usage_known=? WHERE id=?`, state, actual, known, id)
	if err != nil {
		return err
	}
	return tx.Commit()
}
