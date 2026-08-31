package adminauth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

const (
	adminSessionCookie = "__Host-health_admin_session"
	ceremonyLifetime   = 5 * time.Minute
	sessionIdleLimit   = 30 * time.Minute
	sessionLifetime    = 12 * time.Hour
	reauthLifetime     = 5 * time.Minute
	maximumJSONBody    = 1 << 20
)

type Config struct {
	RPID         string
	Origin       string
	DisplayName  string
	CookieSecure bool
}

type Service struct {
	repository Repository
	state      StateStore
	webauthn   *webauthn.WebAuthn
	now        func() time.Time
	config     Config
}

func NewService(repository Repository, state StateStore, config Config, now func() time.Time) (*Service, error) {
	if repository == nil || state == nil || config.RPID == "" || config.Origin == "" {
		return nil, errors.New("invalid admin authentication configuration")
	}
	if now == nil {
		now = time.Now
	}
	if config.DisplayName == "" {
		config.DisplayName = "告你健康管理后台"
	}
	webAuthn, err := webauthn.New(&webauthn.Config{
		RPID:                  config.RPID,
		RPDisplayName:         config.DisplayName,
		RPOrigins:             []string{config.Origin},
		AttestationPreference: protocol.PreferNoAttestation,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementRequired,
			UserVerification: protocol.VerificationRequired,
		},
	})
	if err != nil {
		return nil, err
	}
	return &Service{repository: repository, state: state, webauthn: webAuthn, now: now, config: config}, nil
}

func (service *Service) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/auth/setup/options", service.beginSetup)
	mux.HandleFunc("POST /api/v1/auth/setup/finish", service.finishSetup)
	mux.HandleFunc("POST /api/v1/auth/login/options", service.beginLogin)
	mux.HandleFunc("POST /api/v1/auth/login/finish", service.finishLogin)
	mux.HandleFunc("POST /api/v1/auth/recovery", service.recover)
	mux.HandleFunc("POST /api/v1/auth/logout", service.logout)
	mux.HandleFunc("GET /api/v1/session", service.sessionStatus)
	mux.HandleFunc("POST /api/v1/auth/reauth/options", service.beginReauthentication)
	mux.HandleFunc("POST /api/v1/auth/reauth/finish", service.finishReauthentication)
	mux.HandleFunc("POST /api/v1/security/passkeys/options", service.beginAdditionalPasskey)
	mux.HandleFunc("POST /api/v1/security/passkeys/finish", service.finishAdditionalPasskey)
	mux.HandleFunc("POST /api/v1/security/recovery-codes", service.regenerateRecoveryCodes)
}

func (service *Service) beginSetup(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		BootstrapToken string `json:"bootstrapToken"`
		DisplayName    string `json:"displayName"`
	}
	if !decodeJSON(writer, request, &input) {
		return
	}
	if _, exists, err := service.repository.Owner(request.Context()); err != nil {
		writeFailure(writer, http.StatusServiceUnavailable, "temporarily_unavailable", "管理服务暂时不可用")
		return
	} else if exists {
		writeFailure(writer, http.StatusConflict, "already_initialized", "管理员已初始化")
		return
	}
	hash := TokenHash(strings.TrimSpace(input.BootstrapToken))
	valid, err := service.repository.BootstrapTokenValid(request.Context(), hash, service.now())
	if err != nil {
		writeFailure(writer, http.StatusServiceUnavailable, "temporarily_unavailable", "管理服务暂时不可用")
		return
	}
	if !valid {
		writeFailure(writer, http.StatusUnauthorized, "bootstrap_invalid", "初始化链接无效或已过期")
		return
	}
	userID, err := NewUUID()
	if err != nil {
		writeFailure(writer, http.StatusServiceUnavailable, "temporarily_unavailable", "无法创建管理员")
		return
	}
	handle, err := RandomBytes(32)
	if err != nil {
		writeFailure(writer, http.StatusServiceUnavailable, "temporarily_unavailable", "无法创建管理员")
		return
	}
	displayName := strings.TrimSpace(input.DisplayName)
	if displayName == "" || len([]rune(displayName)) > 64 {
		displayName = "管理员"
	}
	user := User{ID: userID, Handle: handle, DisplayName: displayName}
	options, session, err := service.webauthn.BeginRegistration(user,
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
		webauthn.WithConveyancePreference(protocol.PreferNoAttestation),
	)
	if err != nil {
		writeFailure(writer, http.StatusServiceUnavailable, "passkey_unavailable", "无法开始通行密钥注册")
		return
	}
	ceremonyID, err := RandomToken(24)
	if err != nil || service.state.PutCeremony(request.Context(), ceremonyID, CeremonyState{
		Kind: "setup", User: user, Session: *session, BootstrapHash: hash, CreatedAt: service.now(),
	}, ceremonyLifetime) != nil {
		writeFailure(writer, http.StatusServiceUnavailable, "passkey_unavailable", "无法保存通行密钥注册状态")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"ceremonyID": ceremonyID, "publicKey": options.Response})
}

func (service *Service) finishSetup(writer http.ResponseWriter, request *http.Request) {
	ceremony, ok := service.takeCeremony(writer, request, "setup")
	if !ok {
		return
	}
	credential, err := service.webauthn.FinishRegistration(ceremony.User, ceremony.Session, request)
	if err != nil {
		writeFailure(writer, http.StatusUnauthorized, "passkey_invalid", "通行密钥验证失败")
		return
	}
	plainCodes, hashes, err := NewRecoveryCodes(10)
	if err != nil || service.repository.CompleteBootstrap(
		request.Context(), ceremony.BootstrapHash, ceremony.User, *credential, hashes, service.now(),
	) != nil {
		writeFailure(writer, http.StatusConflict, "bootstrap_failed", "管理员初始化未完成")
		return
	}
	token, session, err := service.createSession(request.Context(), ceremony.User.ID, false, true)
	if err != nil {
		writeFailure(writer, http.StatusServiceUnavailable, "session_unavailable", "无法创建管理会话")
		return
	}
	service.setSessionCookie(writer, token)
	service.audit(request.Context(), ceremony.User.ID, request, "admin.bootstrap", "succeeded", nil)
	writeJSON(writer, http.StatusCreated, map[string]any{
		"csrfToken": session.CSRFToken, "recoveryCodes": plainCodes,
	})
}

func (service *Service) beginLogin(writer http.ResponseWriter, request *http.Request) {
	service.beginUserLogin(writer, request, "login")
}

func (service *Service) beginReauthentication(writer http.ResponseWriter, request *http.Request) {
	if _, _, ok := service.requireSession(writer, request, false, true); !ok {
		return
	}
	service.beginUserLogin(writer, request, "reauth")
}

func (service *Service) beginUserLogin(writer http.ResponseWriter, request *http.Request, kind string) {
	user, exists, err := service.repository.Owner(request.Context())
	if err != nil || !exists || len(user.Credentials) == 0 {
		writeFailure(writer, http.StatusUnauthorized, "login_unavailable", "通行密钥登录不可用")
		return
	}
	options, session, err := service.webauthn.BeginLogin(user,
		webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		writeFailure(writer, http.StatusServiceUnavailable, "passkey_unavailable", "无法开始通行密钥验证")
		return
	}
	ceremonyID, err := RandomToken(24)
	if err != nil || service.state.PutCeremony(request.Context(), ceremonyID, CeremonyState{
		Kind: kind, User: user, Session: *session, CreatedAt: service.now(),
	}, ceremonyLifetime) != nil {
		writeFailure(writer, http.StatusServiceUnavailable, "passkey_unavailable", "无法保存通行密钥验证状态")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"ceremonyID": ceremonyID, "publicKey": options.Response})
}

func (service *Service) finishLogin(writer http.ResponseWriter, request *http.Request) {
	service.finishUserLogin(writer, request, "login", false)
}

func (service *Service) finishReauthentication(writer http.ResponseWriter, request *http.Request) {
	service.finishUserLogin(writer, request, "reauth", true)
}

func (service *Service) finishUserLogin(writer http.ResponseWriter, request *http.Request, kind string, reauth bool) {
	ceremony, ok := service.takeCeremony(writer, request, kind)
	if !ok {
		return
	}
	credential, err := service.webauthn.FinishLogin(ceremony.User, ceremony.Session, request)
	if err != nil {
		service.audit(request.Context(), ceremony.User.ID, request, "admin."+kind, "denied", nil)
		writeFailure(writer, http.StatusUnauthorized, "passkey_invalid", "通行密钥验证失败")
		return
	}
	if err := service.repository.UpdateCredential(request.Context(), ceremony.User.ID, *credential, service.now()); err != nil {
		writeFailure(writer, http.StatusServiceUnavailable, "passkey_update_failed", "无法更新通行密钥状态")
		return
	}
	if reauth {
		token, session, ok := service.requireSession(writer, request, false, false)
		if !ok {
			return
		}
		session.ReauthenticatedAt = service.now()
		if err := service.saveSession(request.Context(), token, session); err != nil {
			writeFailure(writer, http.StatusServiceUnavailable, "session_unavailable", "无法更新管理会话")
			return
		}
		service.audit(request.Context(), ceremony.User.ID, request, "admin.reauthenticate", "succeeded", nil)
		writeJSON(writer, http.StatusOK, map[string]any{"reauthenticatedUntil": service.now().Add(reauthLifetime)})
		return
	}
	token, session, err := service.createSession(request.Context(), ceremony.User.ID, false, true)
	if err != nil {
		writeFailure(writer, http.StatusServiceUnavailable, "session_unavailable", "无法创建管理会话")
		return
	}
	service.setSessionCookie(writer, token)
	service.audit(request.Context(), ceremony.User.ID, request, "admin.login", "succeeded", nil)
	writeJSON(writer, http.StatusOK, map[string]any{"csrfToken": session.CSRFToken})
}

func (service *Service) recover(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		RecoveryCode string `json:"recoveryCode"`
	}
	if !decodeJSON(writer, request, &input) {
		return
	}
	user, valid, err := service.repository.ConsumeRecoveryCode(request.Context(), input.RecoveryCode, service.now())
	if err != nil || !valid {
		writeFailure(writer, http.StatusUnauthorized, "recovery_invalid", "恢复码无效或已使用")
		return
	}
	token, session, err := service.createSession(request.Context(), user.ID, true, false)
	if err != nil {
		writeFailure(writer, http.StatusServiceUnavailable, "session_unavailable", "无法创建恢复会话")
		return
	}
	service.setSessionCookie(writer, token)
	service.audit(request.Context(), user.ID, request, "admin.recovery", "succeeded", nil)
	writeJSON(writer, http.StatusOK, map[string]any{"csrfToken": session.CSRFToken, "recoveryOnly": true})
}

func (service *Service) beginAdditionalPasskey(writer http.ResponseWriter, request *http.Request) {
	_, session, ok := service.requireSession(writer, request, true, true)
	if !ok {
		return
	}
	user, exists, err := service.repository.Owner(request.Context())
	if err != nil || !exists || user.ID != session.UserID {
		writeFailure(writer, http.StatusUnauthorized, "session_invalid", "管理会话无效")
		return
	}
	options, ceremony, err := service.webauthn.BeginRegistration(user,
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
		webauthn.WithConveyancePreference(protocol.PreferNoAttestation))
	if err != nil {
		writeFailure(writer, http.StatusServiceUnavailable, "passkey_unavailable", "无法开始通行密钥注册")
		return
	}
	ceremonyID, err := RandomToken(24)
	if err != nil || service.state.PutCeremony(request.Context(), ceremonyID, CeremonyState{
		Kind: "additional", User: user, Session: *ceremony, CreatedAt: service.now(),
	}, ceremonyLifetime) != nil {
		writeFailure(writer, http.StatusServiceUnavailable, "passkey_unavailable", "无法保存通行密钥注册状态")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"ceremonyID": ceremonyID, "publicKey": options.Response})
}

func (service *Service) finishAdditionalPasskey(writer http.ResponseWriter, request *http.Request) {
	_, session, ok := service.requireSession(writer, request, true, true)
	if !ok {
		return
	}
	ceremony, ok := service.takeCeremony(writer, request, "additional")
	if !ok || ceremony.User.ID != session.UserID {
		return
	}
	credential, err := service.webauthn.FinishRegistration(ceremony.User, ceremony.Session, request)
	if err != nil {
		writeFailure(writer, http.StatusUnauthorized, "passkey_invalid", "通行密钥验证失败")
		return
	}
	if err := service.repository.AddCredential(request.Context(), session.UserID, "备用通行密钥", *credential); err != nil {
		writeFailure(writer, http.StatusConflict, "passkey_exists", "该通行密钥已经登记")
		return
	}
	if session.RecoveryOnly {
		token := sessionToken(request)
		session.RecoveryOnly = false
		session.ReauthenticatedAt = service.now()
		if err := service.saveSession(request.Context(), token, session); err != nil {
			writeFailure(writer, http.StatusServiceUnavailable, "session_unavailable", "无法升级恢复会话")
			return
		}
	}
	service.audit(request.Context(), session.UserID, request, "admin.passkey.add", "succeeded", nil)
	writeJSON(writer, http.StatusCreated, map[string]any{"created": true})
}

func (service *Service) regenerateRecoveryCodes(writer http.ResponseWriter, request *http.Request) {
	_, session, ok := service.requireSession(writer, request, true, true)
	if !ok || !service.recentlyReauthenticated(session) {
		if ok {
			writeFailure(writer, http.StatusUnauthorized, "reauthentication_required", "请再次验证通行密钥")
		}
		return
	}
	plain, hashes, err := NewRecoveryCodes(10)
	if err != nil || service.repository.ReplaceRecoveryCodes(request.Context(), session.UserID, hashes, service.now()) != nil {
		writeFailure(writer, http.StatusServiceUnavailable, "recovery_unavailable", "无法重新生成恢复码")
		return
	}
	service.audit(request.Context(), session.UserID, request, "admin.recovery.regenerate", "succeeded", nil)
	writeJSON(writer, http.StatusCreated, map[string]any{"recoveryCodes": plain})
}

func (service *Service) sessionStatus(writer http.ResponseWriter, request *http.Request) {
	_, session, ok := service.requireSession(writer, request, false, true)
	if !ok {
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"authenticated":           true,
		"csrfToken":               session.CSRFToken,
		"recoveryOnly":            session.RecoveryOnly,
		"expiresAt":               session.ExpiresAt,
		"recentlyReauthenticated": service.recentlyReauthenticated(session),
	})
}

func (service *Service) logout(writer http.ResponseWriter, request *http.Request) {
	token, session, ok := service.requireSession(writer, request, true, true)
	if !ok {
		return
	}
	_ = service.state.DeleteSession(request.Context(), TokenHash(token))
	service.clearSessionCookie(writer)
	service.audit(request.Context(), session.UserID, request, "admin.logout", "succeeded", nil)
	writer.WriteHeader(http.StatusNoContent)
}

func (service *Service) RequireAuthenticated(
	writer http.ResponseWriter,
	request *http.Request,
	csrf bool,
	reauthenticated bool,
) (Session, bool) {
	_, session, ok := service.requireSession(writer, request, csrf, false)
	if !ok {
		return Session{}, false
	}
	if session.RecoveryOnly {
		writeFailure(writer, http.StatusForbidden, "passkey_registration_required", "请先重新登记通行密钥")
		return Session{}, false
	}
	if reauthenticated && !service.recentlyReauthenticated(session) {
		writeFailure(writer, http.StatusUnauthorized, "reauthentication_required", "请再次验证通行密钥")
		return Session{}, false
	}
	return session, true
}

func (service *Service) requireSession(
	writer http.ResponseWriter,
	request *http.Request,
	csrf bool,
	allowRecovery bool,
) (string, Session, bool) {
	token := sessionToken(request)
	if token == "" {
		writeFailure(writer, http.StatusUnauthorized, "authentication_required", "请使用通行密钥登录")
		return "", Session{}, false
	}
	hash := TokenHash(token)
	session, found, err := service.state.GetSession(request.Context(), hash)
	now := service.now()
	if err != nil || !found || !now.Before(session.ExpiresAt) || now.Sub(session.LastSeenAt) > sessionIdleLimit {
		_ = service.state.DeleteSession(request.Context(), hash)
		service.clearSessionCookie(writer)
		writeFailure(writer, http.StatusUnauthorized, "session_expired", "管理会话已过期")
		return "", Session{}, false
	}
	if session.RecoveryOnly && !allowRecovery {
		writeFailure(writer, http.StatusForbidden, "passkey_registration_required", "请先重新登记通行密钥")
		return "", Session{}, false
	}
	if csrf && (request.Header.Get("Origin") != service.config.Origin ||
		request.Header.Get("X-Admin-CSRF") == "" || request.Header.Get("X-Admin-CSRF") != session.CSRFToken) {
		writeFailure(writer, http.StatusForbidden, "csrf_invalid", "请求安全验证失败")
		return "", Session{}, false
	}
	session.LastSeenAt = now
	if err := service.saveSession(request.Context(), token, session); err != nil {
		writeFailure(writer, http.StatusServiceUnavailable, "session_unavailable", "管理会话暂时不可用")
		return "", Session{}, false
	}
	return token, session, true
}

func (service *Service) createSession(
	ctx context.Context,
	userID string,
	recoveryOnly bool,
	reauthenticated bool,
) (string, Session, error) {
	token, err := RandomToken(32)
	if err != nil {
		return "", Session{}, err
	}
	csrf, err := RandomToken(24)
	if err != nil {
		return "", Session{}, err
	}
	now := service.now()
	session := Session{
		UserID: userID, CSRFToken: csrf, RecoveryOnly: recoveryOnly,
		CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(sessionLifetime),
	}
	if reauthenticated {
		session.ReauthenticatedAt = now
	}
	if err := service.state.PutSession(ctx, TokenHash(token), session, sessionLifetime); err != nil {
		return "", Session{}, err
	}
	return token, session, nil
}

func (service *Service) saveSession(ctx context.Context, token string, session Session) error {
	remaining := session.ExpiresAt.Sub(service.now())
	if remaining <= 0 {
		return errors.New("session expired")
	}
	return service.state.PutSession(ctx, TokenHash(token), session, remaining)
}

func (service *Service) recentlyReauthenticated(session Session) bool {
	return !session.ReauthenticatedAt.IsZero() && service.now().Sub(session.ReauthenticatedAt) <= reauthLifetime
}

func (service *Service) takeCeremony(
	writer http.ResponseWriter,
	request *http.Request,
	kind string,
) (CeremonyState, bool) {
	id := strings.TrimSpace(request.Header.Get("X-Admin-Ceremony-ID"))
	if id == "" || len(id) > 128 {
		writeFailure(writer, http.StatusBadRequest, "ceremony_missing", "缺少通行密钥验证状态")
		return CeremonyState{}, false
	}
	state, found, err := service.state.TakeCeremony(request.Context(), id)
	if err != nil || !found || state.Kind != kind {
		writeFailure(writer, http.StatusUnauthorized, "ceremony_expired", "通行密钥验证已过期，请重试")
		return CeremonyState{}, false
	}
	return state, true
}

func (service *Service) setSessionCookie(writer http.ResponseWriter, token string) {
	http.SetCookie(writer, &http.Cookie{
		Name: adminSessionCookie, Value: token, Path: "/", MaxAge: int(sessionLifetime.Seconds()),
		Secure: service.config.CookieSecure, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
}

func (service *Service) clearSessionCookie(writer http.ResponseWriter) {
	http.SetCookie(writer, &http.Cookie{
		Name: adminSessionCookie, Value: "", Path: "/", MaxAge: -1,
		Secure: service.config.CookieSecure, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
}

func sessionToken(request *http.Request) string {
	cookie, err := request.Cookie(adminSessionCookie)
	if err != nil || len(cookie.Value) < 32 || len(cookie.Value) > 256 {
		return ""
	}
	return cookie.Value
}

func (service *Service) audit(
	ctx context.Context,
	userID string,
	request *http.Request,
	action string,
	outcome string,
	metadata map[string]any,
) {
	requestID := request.Header.Get("X-Request-ID")
	if requestID == "" {
		requestID, _ = NewUUID()
	}
	if metadata == nil {
		metadata = map[string]any{}
	}
	_ = service.repository.AppendAudit(ctx, AuditEvent{
		UserID: userID, RequestID: requestID, Action: action, Outcome: outcome, Metadata: metadata,
	})
}

func decodeJSON(writer http.ResponseWriter, request *http.Request, destination any) bool {
	request.Body = http.MaxBytesReader(writer, request.Body, maximumJSONBody)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		writeFailure(writer, http.StatusBadRequest, "invalid_request", "请求格式无效")
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeFailure(writer, http.StatusBadRequest, "invalid_request", "请求只能包含一个 JSON 对象")
		return false
	}
	return true
}

func writeFailure(writer http.ResponseWriter, status int, code string, message string) {
	writeJSON(writer, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
