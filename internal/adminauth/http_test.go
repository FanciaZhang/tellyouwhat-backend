package adminauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
)

func TestRolePolicySeparatesGlobalAdministrationFromAssignedApps(t *testing.T) {
	t.Parallel()
	admin := User{Role: RoleAdmin, Status: UserStatusActive}
	if !admin.Allows(PermissionUsersManage, "") || !admin.Allows(PermissionOfferManage, "journal") {
		t.Fatal("administrator did not receive global administration and app permissions")
	}
	operator := User{Role: RoleOperator, Status: UserStatusActive, AppIDs: []string{"health"}}
	if !operator.Allows(PermissionOfferManage, "health") || operator.Allows(PermissionOfferRead, "journal") ||
		operator.Allows(PermissionUsersManage, "") || operator.Allows(PermissionAuditReadAll, "") {
		t.Fatal("operator permission escaped its assigned app or role")
	}
	operator.Status = UserStatusDisabled
	if operator.Allows(PermissionPortalRead, "") || operator.Allows(PermissionOfferRead, "health") {
		t.Fatal("disabled operator retained permissions")
	}
}

func TestServiceRejectsInsecureOrAmbiguousPasskeyOrigins(t *testing.T) {
	t.Parallel()
	repository := &testRepository{}
	store := NewMemoryStateStore(time.Now)
	for _, test := range []struct {
		name   string
		config Config
	}{
		{"http", Config{RPID: "admin.example.com", Origin: "http://admin.example.com", AppIDs: []string{"health"}}},
		{"wrong-host", Config{RPID: "admin.example.com", Origin: "https://other.example.com", AppIDs: []string{"health"}}},
		{"port", Config{RPID: "admin.example.com", Origin: "https://admin.example.com:443", AppIDs: []string{"health"}}},
		{"path", Config{RPID: "admin.example.com", Origin: "https://admin.example.com/login", AppIDs: []string{"health"}}},
		{"no-apps", Config{RPID: "admin.example.com", Origin: "https://admin.example.com"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewService(repository, store, test.config, time.Now); err == nil {
				t.Fatal("unsafe admin authentication configuration was accepted")
			}
		})
	}
}

func TestDiscoverableLoginDoesNotRequireAUsernameOrCredentialAllowList(t *testing.T) {
	t.Parallel()
	service, _, _ := newTestService(t, User{})
	mux := http.NewServeMux()
	service.RegisterRoutes(mux)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login/options", bytes.NewReader([]byte(`{}`)))
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("login options = %d %s", response.Code, response.Body.String())
	}
	var payload struct {
		PublicKey struct {
			AllowCredentials []any  `json:"allowCredentials"`
			UserVerification string `json:"userVerification"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.PublicKey.AllowCredentials) != 0 || payload.PublicKey.UserVerification != "required" {
		t.Fatalf("login is not discoverable with required verification: %#v", payload.PublicKey)
	}
}

func TestSessionRejectsDisabledUsersAndAuthorizationVersionChanges(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		user User
	}{
		{"disabled", User{ID: "user", Role: RoleAdmin, Status: UserStatusDisabled, SessionVersion: 1}},
		{"version", User{ID: "user", Role: RoleAdmin, Status: UserStatusActive, SessionVersion: 2}},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, store, repository := newTestService(t, test.user)
			token := seedTestSession(t, service, store, 1, true)
			request := authenticatedRequest(http.MethodGet, "/api/v1/session", token, false)
			response := httptest.NewRecorder()
			service.sessionStatus(response, request)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("session remained valid: %d %s", response.Code, response.Body.String())
			}
			if _, found, err := store.GetSession(context.Background(), TokenHash(token)); err != nil || found {
				t.Fatalf("invalid session was not removed: found=%v err=%v", found, err)
			}
			if len(repository.audits) != 0 {
				t.Fatal("ordinary expired session unexpectedly emitted sensitive audit metadata")
			}
		})
	}
}

func TestTemporaryUserStoreFailureDoesNotDestroyAValidSession(t *testing.T) {
	t.Parallel()
	user := User{ID: "user", Role: RoleAdmin, Status: UserStatusActive, SessionVersion: 1}
	service, store, repository := newTestService(t, user)
	token := seedTestSession(t, service, store, 1, true)
	repository.userByIDErr = errors.New("database unavailable")
	request := authenticatedRequest(http.MethodGet, "/api/v1/session", token, false)
	response := httptest.NewRecorder()
	service.sessionStatus(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("database outage returned %d %s", response.Code, response.Body.String())
	}
	if _, found, err := store.GetSession(context.Background(), TokenHash(token)); err != nil || !found {
		t.Fatalf("temporary outage destroyed the session: found=%v err=%v", found, err)
	}
}

func TestPermissionDenialIsServerEnforcedAndAudited(t *testing.T) {
	t.Parallel()
	operator := User{ID: "operator", DisplayName: "运营", Role: RoleOperator,
		Status: UserStatusActive, SessionVersion: 1, AppIDs: []string{"health"}}
	service, store, repository := newTestService(t, operator)
	token := seedTestSession(t, service, store, 1, true)
	request := authenticatedRequest(http.MethodGet, "/api/v1/admin/users", token, false)
	request.Header.Set("X-Request-ID", "not-a-database-safe-request-id")
	response := httptest.NewRecorder()
	service.listUsers(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("operator opened user administration: %d %s", response.Code, response.Body.String())
	}
	if len(repository.audits) != 1 || repository.audits[0].Outcome != "denied" ||
		repository.audits[0].TargetID != string(PermissionUsersManage) ||
		len(repository.audits[0].RequestID) != 36 {
		t.Fatalf("permission denial was not audited: %#v", repository.audits)
	}
}

func TestReauthenticationRequiresSameOriginCSRF(t *testing.T) {
	t.Parallel()
	admin := User{ID: "admin", DisplayName: "管理员", Role: RoleAdmin,
		Status: UserStatusActive, SessionVersion: 1}
	service, store, _ := newTestService(t, admin)
	token := seedTestSession(t, service, store, 1, true)
	request := authenticatedRequest(http.MethodPost, "/api/v1/auth/reauth/options", token, false)
	response := httptest.NewRecorder()
	service.beginReauthentication(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("cross-origin reauthentication start returned %d %s", response.Code, response.Body.String())
	}
}

func TestDeletePasskeyDistinguishesConflictsMissingRowsAndStorageFailures(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		repository error
		status     int
	}{
		{"last", ErrLastCredential, http.StatusConflict},
		{"missing", ErrCredentialNotFound, http.StatusNotFound},
		{"storage", errors.New("database offline"), http.StatusServiceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			admin := User{ID: "admin", DisplayName: "管理员", Role: RoleAdmin,
				Status: UserStatusActive, SessionVersion: 1}
			service, store, repository := newTestService(t, admin)
			repository.deleteCredentialErr = test.repository
			token := seedTestSession(t, service, store, 1, true)
			request := authenticatedRequest(http.MethodDelete, "/api/v1/security/passkeys/Y3JlZA", token, true)
			request.SetPathValue("credentialID", "Y3JlZA")
			response := httptest.NewRecorder()
			service.deletePasskey(response, request)
			if response.Code != test.status {
				t.Fatalf("delete error %v returned %d %s", test.repository, response.Code, response.Body.String())
			}
		})
	}
}

func TestSessionCookieIsAlwaysHostBoundAndSecure(t *testing.T) {
	t.Parallel()
	service, _, _ := newTestService(t, User{})
	response := httptest.NewRecorder()
	service.setSessionCookie(response, "01234567890123456789012345678901")
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != adminSessionCookie || !cookies[0].Secure ||
		!cookies[0].HttpOnly || cookies[0].Path != "/" || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("unsafe session cookie: %#v", cookies)
	}
}

func TestFutureReauthenticationTimestampIsNotAccepted(t *testing.T) {
	t.Parallel()
	service, _, _ := newTestService(t, User{})
	if service.recentlyReauthenticated(Session{ReauthenticatedAt: service.now().Add(time.Second)}) {
		t.Fatal("future reauthentication timestamp was accepted")
	}
}

func TestDisplayNamesRejectControlAndInvisibleFormattingCharacters(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"Alice\nAdmin", "Alice\u202eAdmin", "Alice\u200bAdmin"} {
		if cleanDisplayName(value) != "" {
			t.Fatalf("unsafe display name was accepted: %q", value)
		}
	}
	if cleanDisplayName("  小林  ") != "小林" {
		t.Fatal("ordinary display name was not normalized")
	}
}

func TestAdminCanCreateScopedInvitationOnlyAfterRecentPasskeyVerification(t *testing.T) {
	t.Parallel()
	admin := User{ID: "admin", DisplayName: "管理员", Role: RoleAdmin,
		Status: UserStatusActive, SessionVersion: 1}
	service, store, repository := newTestService(t, admin)
	mux := http.NewServeMux()
	service.RegisterRoutes(mux)
	token := seedTestSession(t, service, store, 1, true)
	body := []byte(`{"displayName":"小林","role":"operator","appIDs":["health"]}`)
	request := authenticatedRequest(http.MethodPost, "/api/v1/admin/invitations", token, true)
	request.Body = ioNopCloser{bytes.NewReader(body)}
	request.ContentLength = int64(len(body))
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create invitation = %d %s", response.Code, response.Body.String())
	}
	if repository.createdInvitation == nil || repository.createdInvitation.Role != RoleOperator ||
		!slices.Equal(repository.createdInvitation.AppIDs, []string{"health"}) {
		t.Fatalf("stored invitation = %#v", repository.createdInvitation)
	}
	var payload struct {
		EnrollmentURL string `json:"enrollmentURL"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil ||
		len(payload.EnrollmentURL) < len("https://admin.example.com/enroll#token=") {
		t.Fatalf("enrollment URL = %q, err = %v", payload.EnrollmentURL, err)
	}

	staleToken := seedTestSession(t, service, store, 1, false)
	staleRequest := authenticatedRequest(http.MethodPost, "/api/v1/admin/invitations", staleToken, true)
	staleRequest.Body = ioNopCloser{bytes.NewReader(body)}
	staleRequest.ContentLength = int64(len(body))
	staleResponse := httptest.NewRecorder()
	mux.ServeHTTP(staleResponse, staleRequest)
	if staleResponse.Code != http.StatusUnauthorized {
		t.Fatalf("invitation did not require recent passkey verification: %d", staleResponse.Code)
	}
}

func TestPasswordAndRecoveryCodeRoutesDoNotExist(t *testing.T) {
	t.Parallel()
	service, _, _ := newTestService(t, User{})
	mux := http.NewServeMux()
	service.RegisterRoutes(mux)
	for _, path := range []string{"/api/v1/auth/password", "/api/v1/auth/recovery", "/api/v1/security/recovery-codes"} {
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, nil))
		if response.Code != http.StatusNotFound {
			t.Fatalf("legacy secret route %s returned %d", path, response.Code)
		}
	}
}

type ioNopCloser struct{ *bytes.Reader }

func (ioNopCloser) Close() error { return nil }

func newTestService(t *testing.T, user User) (*Service, *MemoryStateStore, *testRepository) {
	t.Helper()
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	repository := &testRepository{user: user}
	store := NewMemoryStateStore(func() time.Time { return now })
	service, err := NewService(repository, store, Config{
		RPID: "admin.example.com", Origin: "https://admin.example.com",
		AppIDs: []string{"health", "journal"},
	}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	return service, store, repository
}

func seedTestSession(t *testing.T, service *Service, store *MemoryStateStore, version uint64, recent bool) string {
	t.Helper()
	token := "01234567890123456789012345678901" + map[bool]string{true: "a", false: "b"}[recent]
	now := service.now()
	session := Session{
		UserID: service.repository.(*testRepository).user.ID, SessionVersion: version,
		CSRFToken: "csrf-token", CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour),
	}
	if recent {
		session.ReauthenticatedAt = now
	}
	if err := store.PutSession(context.Background(), TokenHash(token), session, time.Hour); err != nil {
		t.Fatal(err)
	}
	return token
}

func authenticatedRequest(method, path, token string, csrf bool) *http.Request {
	request := httptest.NewRequest(method, path, nil)
	request.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: token})
	if csrf {
		request.Header.Set("Origin", "https://admin.example.com")
		request.Header.Set("X-Admin-CSRF", "csrf-token")
	}
	return request
}

type testRepository struct {
	user                User
	audits              []AuditEvent
	createdInvitation   *Invitation
	deleteCredentialErr error
	userByIDErr         error
}

func (repository *testRepository) Initialized(context.Context) (bool, error) { return false, nil }
func (repository *testRepository) CreateBootstrapToken(context.Context, [32]byte, time.Time) error {
	return nil
}
func (repository *testRepository) BootstrapTokenValid(context.Context, [32]byte, time.Time) (bool, error) {
	return true, nil
}
func (repository *testRepository) CompleteBootstrap(context.Context, [32]byte, User, webauthn.Credential, time.Time) error {
	return nil
}
func (repository *testRepository) UserByID(_ context.Context, id string) (User, bool, error) {
	if repository.userByIDErr != nil {
		return User{}, false, repository.userByIDErr
	}
	return repository.user, repository.user.ID != "" && repository.user.ID == id, nil
}
func (repository *testRepository) UserByHandle(_ context.Context, handle []byte) (User, bool, error) {
	return repository.user, len(handle) > 0 && slices.Equal(handle, repository.user.Handle), nil
}
func (repository *testRepository) ListUsers(context.Context) ([]UserSummary, error) { return nil, nil }
func (repository *testRepository) UpdateUser(context.Context, string, UserUpdate, time.Time) (User, error) {
	return User{}, ErrUserNotFound
}
func (repository *testRepository) CreateInvitation(_ context.Context, _ [32]byte, invitation Invitation) error {
	copy := invitation
	repository.createdInvitation = &copy
	return nil
}
func (repository *testRepository) InvitationByToken(context.Context, [32]byte, time.Time) (Invitation, bool, error) {
	return Invitation{}, false, nil
}
func (repository *testRepository) ListInvitations(context.Context, time.Time) ([]Invitation, error) {
	return nil, nil
}
func (repository *testRepository) RevokeInvitation(context.Context, string, time.Time) error {
	return nil
}
func (repository *testRepository) CompleteInvitation(context.Context, [32]byte, Invitation, User, webauthn.Credential, time.Time) (User, error) {
	return User{}, errors.New("not implemented")
}
func (repository *testRepository) AddCredential(context.Context, string, string, webauthn.Credential, uint64) error {
	return nil
}
func (repository *testRepository) UpdateCredential(context.Context, string, webauthn.Credential, time.Time) error {
	return nil
}
func (repository *testRepository) ListCredentials(context.Context, string) ([]CredentialSummary, error) {
	return nil, nil
}
func (repository *testRepository) DeleteCredential(context.Context, string, []byte) error {
	return repository.deleteCredentialErr
}
func (repository *testRepository) AppendAudit(_ context.Context, event AuditEvent) error {
	repository.audits = append(repository.audits, event)
	return nil
}
func (repository *testRepository) ListAudit(context.Context, string, bool, int) ([]AuditRecord, error) {
	return nil, nil
}

var _ Repository = (*testRepository)(nil)
