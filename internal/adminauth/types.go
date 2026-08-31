package adminauth

import (
	"context"
	"slices"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
)

type Role string

const (
	RoleAdmin    Role = "admin"
	RoleOperator Role = "operator"
)

func (role Role) Valid() bool { return role == RoleAdmin || role == RoleOperator }

type UserStatus string

const (
	UserStatusActive   UserStatus = "active"
	UserStatusDisabled UserStatus = "disabled"
)

func (status UserStatus) Valid() bool {
	return status == UserStatusActive || status == UserStatusDisabled
}

type Permission string

const (
	PermissionPortalRead        Permission = "portal.read"
	PermissionOfferRead         Permission = "offers.read"
	PermissionOfferManage       Permission = "offers.manage"
	PermissionOfferCodeDownload Permission = "offer_codes.download"
	PermissionMetricsRead       Permission = "metrics.read"
	PermissionUsersManage       Permission = "users.manage"
	PermissionAuditReadAll      Permission = "audit.read.all"
	PermissionAIConfigManage    Permission = "ai_config.manage"
)

type User struct {
	ID             string
	Handle         []byte
	DisplayName    string
	Role           Role
	Status         UserStatus
	SessionVersion uint64
	AppIDs         []string
	Credentials    []webauthn.Credential
}

func (user User) WebAuthnID() []byte                         { return user.Handle }
func (user User) WebAuthnName() string                       { return user.DisplayName }
func (user User) WebAuthnDisplayName() string                { return user.DisplayName }
func (user User) WebAuthnCredentials() []webauthn.Credential { return user.Credentials }

func (user User) CanAccessApp(appID string) bool {
	if user.Status != UserStatusActive || appID == "" {
		return false
	}
	return user.Role == RoleAdmin || (user.Role == RoleOperator && slices.Contains(user.AppIDs, appID))
}

func (user User) Allows(permission Permission, appID string) bool {
	if user.Status != UserStatusActive || !user.Role.Valid() {
		return false
	}
	switch permission {
	case PermissionPortalRead:
		return appID == "" || user.CanAccessApp(appID)
	case PermissionOfferRead, PermissionOfferManage, PermissionOfferCodeDownload, PermissionMetricsRead:
		return user.CanAccessApp(appID)
	case PermissionUsersManage, PermissionAuditReadAll, PermissionAIConfigManage:
		return user.Role == RoleAdmin
	default:
		return false
	}
}

type CredentialSummary struct {
	ID          string     `json:"id"`
	DisplayName string     `json:"displayName"`
	CreatedAt   time.Time  `json:"createdAt"`
	LastUsedAt  *time.Time `json:"lastUsedAt,omitempty"`
}

type UserSummary struct {
	ID              string     `json:"id"`
	DisplayName     string     `json:"displayName"`
	Role            Role       `json:"role"`
	Status          UserStatus `json:"status"`
	AppIDs          []string   `json:"appIDs"`
	CredentialCount int        `json:"credentialCount"`
	CreatedAt       time.Time  `json:"createdAt"`
	UpdatedAt       time.Time  `json:"updatedAt"`
}

type UserUpdate struct {
	DisplayName string
	Role        Role
	Status      UserStatus
	AppIDs      []string
}

type InvitationKind string

const (
	InvitationKindCreate   InvitationKind = "create"
	InvitationKindRecovery InvitationKind = "recovery"
)

type Invitation struct {
	ID            string         `json:"id"`
	Kind          InvitationKind `json:"kind"`
	TargetUserID  string         `json:"targetUserID,omitempty"`
	InvitedByID   string         `json:"-"`
	InvitedByName string         `json:"invitedByName,omitempty"`
	DisplayName   string         `json:"displayName"`
	Role          Role           `json:"role"`
	AppIDs        []string       `json:"appIDs"`
	ExpiresAt     time.Time      `json:"expiresAt"`
	CreatedAt     time.Time      `json:"createdAt"`
}

type AuditEvent struct {
	UserID     string
	AppID      string
	RequestID  string
	Action     string
	TargetType string
	TargetID   string
	Outcome    string
	Metadata   map[string]any
}

type AuditRecord struct {
	ID          uint64         `json:"id"`
	UserID      string         `json:"userID,omitempty"`
	DisplayName string         `json:"displayName,omitempty"`
	AppID       string         `json:"appID,omitempty"`
	Action      string         `json:"action"`
	TargetType  string         `json:"targetType,omitempty"`
	TargetID    string         `json:"targetID,omitempty"`
	Outcome     string         `json:"outcome"`
	Metadata    map[string]any `json:"metadata"`
	CreatedAt   time.Time      `json:"createdAt"`
}

type Repository interface {
	Initialized(context.Context) (bool, error)
	CreateBootstrapToken(context.Context, [32]byte, time.Time) error
	BootstrapTokenValid(context.Context, [32]byte, time.Time) (bool, error)
	CompleteBootstrap(context.Context, [32]byte, User, webauthn.Credential, time.Time) error

	UserByID(context.Context, string) (User, bool, error)
	UserByHandle(context.Context, []byte) (User, bool, error)
	ListUsers(context.Context) ([]UserSummary, error)
	UpdateUser(context.Context, string, UserUpdate, time.Time) (User, error)

	CreateInvitation(context.Context, [32]byte, Invitation) error
	InvitationByToken(context.Context, [32]byte, time.Time) (Invitation, bool, error)
	ListInvitations(context.Context, time.Time) ([]Invitation, error)
	RevokeInvitation(context.Context, string, time.Time) error
	CompleteInvitation(context.Context, [32]byte, Invitation, User, webauthn.Credential, time.Time) (User, error)

	AddCredential(context.Context, string, string, webauthn.Credential, uint64) error
	UpdateCredential(context.Context, string, webauthn.Credential, time.Time) error
	ListCredentials(context.Context, string) ([]CredentialSummary, error)
	DeleteCredential(context.Context, string, []byte) error

	AppendAudit(context.Context, AuditEvent) error
	ListAudit(context.Context, string, bool, int) ([]AuditRecord, error)
}

type CeremonyState struct {
	Kind          string               `json:"kind"`
	User          User                 `json:"user"`
	Session       webauthn.SessionData `json:"session"`
	BootstrapHash [32]byte             `json:"bootstrapHash"`
	InviteHash    [32]byte             `json:"inviteHash"`
	Invitation    Invitation           `json:"invitation"`
	CreatedAt     time.Time            `json:"createdAt"`
}

type Session struct {
	UserID            string    `json:"userID"`
	SessionVersion    uint64    `json:"sessionVersion"`
	CSRFToken         string    `json:"csrfToken"`
	CreatedAt         time.Time `json:"createdAt"`
	LastSeenAt        time.Time `json:"lastSeenAt"`
	ExpiresAt         time.Time `json:"expiresAt"`
	ReauthenticatedAt time.Time `json:"reauthenticatedAt"`
}

type Authenticated struct {
	Token   string
	User    User
	Session Session
}

type StateStore interface {
	PutCeremony(context.Context, string, CeremonyState, time.Duration) error
	TakeCeremony(context.Context, string) (CeremonyState, bool, error)
	PutSession(context.Context, [32]byte, Session, time.Duration) error
	GetSession(context.Context, [32]byte) (Session, bool, error)
	DeleteSession(context.Context, [32]byte) error
}
