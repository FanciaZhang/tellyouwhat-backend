package media

import (
	"context"
	"errors"
	"time"

	"github.com/tellyouwhat/backend/internal/contracts"
)

var (
	ErrAuthorizationConflict = errors.New("media authorization conflict")
	ErrNotAuthorized         = errors.New("media is not authorized")
	ErrIdempotencyReplay     = errors.New("request was already committed")
	ErrIdempotencyConflict   = errors.New("request idempotency conflict")
	ErrUnavailable           = errors.New("media infrastructure unavailable")
)

type AttemptRecord struct {
	RequestID  string
	OwnerKeyID string
	BodyDigest string
	ExpiresAt  time.Time
	CreatedAt  time.Time
}

type Record struct {
	ObjectID      string
	OwnerKeyID    string
	OwnerDeviceID string
	RequestID     string
	Operation     contracts.Operation
	MediaID       string
	Kind          string
	MIMEType      string
	SHA256        string
	SizeBytes     int64
	ExpiresAt     time.Time
	DeletedAt     *time.Time
	ConsumedAt    *time.Time
}

type Registry interface {
	Register(context.Context, Record) error
	Get(context.Context, string) (Record, error)
	CommitAttempt(context.Context, []Record, AttemptRecord, time.Time) (AttemptRecord, bool, error)
}
