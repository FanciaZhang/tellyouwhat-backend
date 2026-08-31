package adminauth

import (
	"context"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
)

const OwnerRole = "owner"

type User struct {
	ID          string
	Handle      []byte
	DisplayName string
	Credentials []webauthn.Credential
}

func (user User) WebAuthnID() []byte                         { return user.Handle }
func (user User) WebAuthnName() string                       { return user.DisplayName }
func (user User) WebAuthnDisplayName() string                { return user.DisplayName }
func (user User) WebAuthnCredentials() []webauthn.Credential { return user.Credentials }

type AuditEvent struct {
	UserID     string
	RequestID  string
	Action     string
	TargetType string
	TargetID   string
	Outcome    string
	Metadata   map[string]any
}

type Repository interface {
	Owner(context.Context) (User, bool, error)
	CreateBootstrapToken(context.Context, [32]byte, time.Time) error
	BootstrapTokenValid(context.Context, [32]byte, time.Time) (bool, error)
	CompleteBootstrap(context.Context, [32]byte, User, webauthn.Credential, []string, time.Time) error
	AddCredential(context.Context, string, string, webauthn.Credential) error
	UpdateCredential(context.Context, string, webauthn.Credential, time.Time) error
	ConsumeRecoveryCode(context.Context, string, time.Time) (User, bool, error)
	ReplaceRecoveryCodes(context.Context, string, []string, time.Time) error
	AppendAudit(context.Context, AuditEvent) error
}

type CeremonyState struct {
	Kind          string               `json:"kind"`
	User          User                 `json:"user"`
	Session       webauthn.SessionData `json:"session"`
	BootstrapHash [32]byte             `json:"bootstrapHash"`
	CreatedAt     time.Time            `json:"createdAt"`
}

type Session struct {
	UserID            string    `json:"userID"`
	CSRFToken         string    `json:"csrfToken"`
	RecoveryOnly      bool      `json:"recoveryOnly"`
	CreatedAt         time.Time `json:"createdAt"`
	LastSeenAt        time.Time `json:"lastSeenAt"`
	ExpiresAt         time.Time `json:"expiresAt"`
	ReauthenticatedAt time.Time `json:"reauthenticatedAt"`
}

type StateStore interface {
	PutCeremony(context.Context, string, CeremonyState, time.Duration) error
	TakeCeremony(context.Context, string) (CeremonyState, bool, error)
	PutSession(context.Context, [32]byte, Session, time.Duration) error
	GetSession(context.Context, [32]byte) (Session, bool, error)
	DeleteSession(context.Context, [32]byte) error
}
