package adminauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

const (
	adminSessionCookie = "__Host-tellyouwhat_admin_session"
	ceremonyLifetime   = 5 * time.Minute
	invitationLifetime = 15 * time.Minute
	sessionIdleLimit   = 30 * time.Minute
	sessionLifetime    = 12 * time.Hour
	reauthLifetime     = 5 * time.Minute
	maximumJSONBody    = 1 << 20
)

type Config struct {
	RPID        string
	Origin      string
	DisplayName string
	AppIDs      []string
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
		config.DisplayName = "Tellyouwhat 管理后台"
	}
	config.RPID = strings.ToLower(strings.TrimSpace(config.RPID))
	config.Origin = strings.TrimRight(strings.TrimSpace(config.Origin), "/")
	config.AppIDs = uniqueStrings(config.AppIDs)
	parsedOrigin, err := url.Parse(config.Origin)
	if err != nil || parsedOrigin.Scheme != "https" || parsedOrigin.Hostname() == "" || parsedOrigin.Port() != "" ||
		parsedOrigin.User != nil || parsedOrigin.RawQuery != "" || parsedOrigin.Fragment != "" ||
		parsedOrigin.Path != "" || !strings.EqualFold(parsedOrigin.Hostname(), config.RPID) || len(config.AppIDs) == 0 {
		return nil, errors.New("admin origin must be an HTTPS origin whose host matches the RP ID")
	}
	config.Origin = "https://" + config.RPID
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
	mux.HandleFunc("POST /api/v1/auth/enroll/options", service.beginEnrollment)
	mux.HandleFunc("POST /api/v1/auth/enroll/finish", service.finishEnrollment)
	mux.HandleFunc("POST /api/v1/auth/logout", service.logout)
	mux.HandleFunc("GET /api/v1/session", service.sessionStatus)
	mux.HandleFunc("POST /api/v1/auth/reauth/options", service.beginReauthentication)
	mux.HandleFunc("POST /api/v1/auth/reauth/finish", service.finishReauthentication)
	mux.HandleFunc("GET /api/v1/security/passkeys", service.listPasskeys)
	mux.HandleFunc("POST /api/v1/security/passkeys/options", service.beginAdditionalPasskey)
	mux.HandleFunc("POST /api/v1/security/passkeys/finish", service.finishAdditionalPasskey)
	mux.HandleFunc("DELETE /api/v1/security/passkeys/{credentialID}", service.deletePasskey)
	service.registerManagementRoutes(mux)
}

func (service *Service) beginSetup(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		BootstrapToken string `json:"bootstrapToken"`
		DisplayName    string `json:"displayName"`
	}
	if !decodeJSON(writer, request, &input) {
		return
	}
	if initialized, err := service.repository.Initialized(request.Context()); err != nil {
		writeFailure(writer, http.StatusServiceUnavailable, "temporarily_unavailable", "管理服务暂时不可用")
		return
	} else if initialized {
		writeFailure(writer, http.StatusConflict, "already_initialized", "管理后台已初始化")
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
	user, ok := service.newUser(writer, input.DisplayName, RoleAdmin, nil)
	if !ok {
		return
	}
	options, session, err := service.webauthn.BeginRegistration(user,
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
		webauthn.WithConveyancePreference(protocol.PreferNoAttestation))
	if err != nil {
		writeFailure(writer, http.StatusServiceUnavailable, "passkey_unavailable", "无法开始通行密钥注册")
		return
	}
	service.saveCeremony(writer, request, CeremonyState{
		Kind: "setup", User: user, Session: *session, BootstrapHash: hash, CreatedAt: service.now(),
	}, options.Response)
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
	if err := service.repository.CompleteBootstrap(
		request.Context(), ceremony.BootstrapHash, ceremony.User, *credential, service.now(),
	); err != nil {
		if errors.Is(err, ErrBootstrapInvalid) || errors.Is(err, ErrAlreadyInitialized) ||
			errors.Is(err, ErrCredentialExists) {
			writeFailure(writer, http.StatusConflict, "bootstrap_failed", "管理员初始化未完成")
			return
		}
		writeFailure(writer, http.StatusServiceUnavailable, "bootstrap_unavailable", "管理服务暂时不可用")
		return
	}
	user := ceremony.User
	user.Role, user.Status, user.SessionVersion = RoleAdmin, UserStatusActive, 1
	service.completeAuthentication(writer, request, user, "admin.bootstrap", http.StatusCreated)
}

func (service *Service) beginLogin(writer http.ResponseWriter, request *http.Request) {
	if request.ContentLength > 0 {
		var empty struct{}
		if !decodeJSON(writer, request, &empty) {
			return
		}
	}
	options, session, err := service.webauthn.BeginDiscoverableLogin(
		webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		writeFailure(writer, http.StatusServiceUnavailable, "passkey_unavailable", "无法开始通行密钥验证")
		return
	}
	service.saveCeremony(writer, request, CeremonyState{
		Kind: "login", Session: *session, CreatedAt: service.now(),
	}, options.Response)
}

func (service *Service) finishLogin(writer http.ResponseWriter, request *http.Request) {
	ceremony, ok := service.takeCeremony(writer, request, "login")
	if !ok {
		return
	}
	var resolved User
	userValue, credential, err := service.webauthn.FinishPasskeyLogin(
		func(_, userHandle []byte) (webauthn.User, error) {
			user, found, err := service.repository.UserByHandle(request.Context(), userHandle)
			if err != nil || !found || user.Status != UserStatusActive || !user.Role.Valid() || len(user.Credentials) == 0 {
				return nil, errors.New("admin user is unavailable")
			}
			resolved = user
			return user, nil
		}, ceremony.Session, request)
	if err != nil {
		service.audit(request.Context(), resolved.ID, "", request, "admin.login", "denied", "", "", nil)
		writeFailure(writer, http.StatusUnauthorized, "passkey_invalid", "通行密钥验证失败")
		return
	}
	user, valid := userValue.(User)
	if !valid || user.ID == "" || user.ID != resolved.ID {
		writeFailure(writer, http.StatusUnauthorized, "passkey_invalid", "通行密钥验证失败")
		return
	}
	if err := service.repository.UpdateCredential(request.Context(), user.ID, *credential, service.now()); err != nil {
		writeFailure(writer, http.StatusServiceUnavailable, "passkey_update_failed", "无法更新通行密钥状态")
		return
	}
	service.completeAuthentication(writer, request, user, "admin.login", http.StatusOK)
}

func (service *Service) beginEnrollment(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		Token string `json:"token"`
	}
	if !decodeJSON(writer, request, &input) {
		return
	}
	hash := TokenHash(strings.TrimSpace(input.Token))
	invitation, found, err := service.repository.InvitationByToken(request.Context(), hash, service.now())
	if err != nil {
		writeFailure(writer, http.StatusServiceUnavailable, "temporarily_unavailable", "管理服务暂时不可用")
		return
	}
	if !found {
		writeFailure(writer, http.StatusUnauthorized, "invitation_invalid", "邀请链接无效或已过期")
		return
	}
	var user User
	switch invitation.Kind {
	case InvitationKindCreate:
		user, found = service.newUser(writer, invitation.DisplayName, invitation.Role, invitation.AppIDs)
		if !found {
			return
		}
	case InvitationKindRecovery:
		user, found, err = service.repository.UserByID(request.Context(), invitation.TargetUserID)
		if err != nil || !found || user.Status != UserStatusActive {
			writeFailure(writer, http.StatusUnauthorized, "invitation_invalid", "恢复邀请无效或账号已停用")
			return
		}
	default:
		writeFailure(writer, http.StatusUnauthorized, "invitation_invalid", "邀请链接无效或已过期")
		return
	}
	options, session, err := service.webauthn.BeginRegistration(user,
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
		webauthn.WithConveyancePreference(protocol.PreferNoAttestation))
	if err != nil {
		writeFailure(writer, http.StatusServiceUnavailable, "passkey_unavailable", "无法开始通行密钥注册")
		return
	}
	service.saveCeremony(writer, request, CeremonyState{
		Kind: "enroll", User: user, Session: *session, InviteHash: hash,
		Invitation: invitation, CreatedAt: service.now(),
	}, options.Response)
}

func (service *Service) finishEnrollment(writer http.ResponseWriter, request *http.Request) {
	ceremony, ok := service.takeCeremony(writer, request, "enroll")
	if !ok {
		return
	}
	credential, err := service.webauthn.FinishRegistration(ceremony.User, ceremony.Session, request)
	if err != nil {
		writeFailure(writer, http.StatusUnauthorized, "passkey_invalid", "通行密钥验证失败")
		return
	}
	user, err := service.repository.CompleteInvitation(
		request.Context(), ceremony.InviteHash, ceremony.Invitation, ceremony.User, *credential, service.now())
	if err != nil {
		if errors.Is(err, ErrInvitationInvalid) || errors.Is(err, ErrInvitationUsed) {
			writeFailure(writer, http.StatusConflict, "invitation_invalid", "邀请已失效，请联系管理员重新签发")
			return
		}
		if errors.Is(err, ErrCredentialExists) {
			writeFailure(writer, http.StatusConflict, "passkey_exists", "该通行密钥已绑定其他后台账号")
			return
		}
		writeFailure(writer, http.StatusServiceUnavailable, "enrollment_unavailable", "暂时无法完成通行密钥登记")
		return
	}
	action := "admin.user.enroll"
	if ceremony.Invitation.Kind == InvitationKindRecovery {
		action = "admin.passkey.recover"
	}
	service.completeAuthentication(writer, request, user, action, http.StatusCreated)
}

func (service *Service) beginReauthentication(writer http.ResponseWriter, request *http.Request) {
	authenticated, ok := service.RequireAuthenticated(writer, request, true, false)
	if !ok {
		return
	}
	options, session, err := service.webauthn.BeginLogin(authenticated.User,
		webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		writeFailure(writer, http.StatusServiceUnavailable, "passkey_unavailable", "无法开始通行密钥验证")
		return
	}
	service.saveCeremony(writer, request, CeremonyState{
		Kind: "reauth", User: authenticated.User, Session: *session, CreatedAt: service.now(),
	}, options.Response)
}

func (service *Service) finishReauthentication(writer http.ResponseWriter, request *http.Request) {
	authenticated, ok := service.RequireAuthenticated(writer, request, true, false)
	if !ok {
		return
	}
	ceremony, ok := service.takeCeremony(writer, request, "reauth")
	if !ok {
		return
	}
	if authenticated.User.ID != ceremony.User.ID {
		service.audit(request.Context(), authenticated.User.ID, "", request,
			"admin.reauthenticate", "denied", "admin_user", ceremony.User.ID, nil)
		writeFailure(writer, http.StatusForbidden, "reauthentication_mismatch", "必须使用当前账号的通行密钥")
		return
	}
	credential, err := service.webauthn.FinishLogin(ceremony.User, ceremony.Session, request)
	if err != nil {
		service.audit(request.Context(), ceremony.User.ID, "", request, "admin.reauthenticate", "denied", "", "", nil)
		writeFailure(writer, http.StatusUnauthorized, "passkey_invalid", "通行密钥验证失败")
		return
	}
	if err := service.repository.UpdateCredential(request.Context(), ceremony.User.ID, *credential, service.now()); err != nil {
		writeFailure(writer, http.StatusServiceUnavailable, "passkey_update_failed", "无法更新通行密钥状态")
		return
	}
	authenticated.Session.ReauthenticatedAt = service.now()
	if err := service.saveSession(request.Context(), authenticated.Token, authenticated.Session); err != nil {
		writeFailure(writer, http.StatusServiceUnavailable, "session_unavailable", "无法更新管理会话")
		return
	}
	service.audit(request.Context(), ceremony.User.ID, "", request, "admin.reauthenticate", "succeeded", "", "", nil)
	writeJSON(writer, http.StatusOK, map[string]any{
		"reauthenticatedUntil": service.now().Add(reauthLifetime),
	})
}

func (service *Service) listPasskeys(writer http.ResponseWriter, request *http.Request) {
	authenticated, ok := service.RequireAuthenticated(writer, request, false, false)
	if !ok {
		return
	}
	credentials, err := service.repository.ListCredentials(request.Context(), authenticated.User.ID)
	if err != nil {
		writeFailure(writer, http.StatusServiceUnavailable, "passkeys_unavailable", "暂时无法读取通行密钥")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"passkeys": credentials})
}

func (service *Service) beginAdditionalPasskey(writer http.ResponseWriter, request *http.Request) {
	authenticated, ok := service.RequireAuthenticated(writer, request, true, true)
	if !ok {
		return
	}
	var input struct {
		DisplayName string `json:"displayName"`
	}
	if !decodeJSON(writer, request, &input) {
		return
	}
	displayName := cleanDisplayName(input.DisplayName)
	if displayName == "" {
		writeFailure(writer, http.StatusBadRequest, "invalid_passkey_name", "请输入通行密钥名称")
		return
	}
	options, session, err := service.webauthn.BeginRegistration(authenticated.User,
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
		webauthn.WithConveyancePreference(protocol.PreferNoAttestation))
	if err != nil {
		writeFailure(writer, http.StatusServiceUnavailable, "passkey_unavailable", "无法开始通行密钥注册")
		return
	}
	service.saveCeremony(writer, request, CeremonyState{
		Kind: "additional", User: authenticated.User, Session: *session,
		Invitation: Invitation{DisplayName: displayName}, CreatedAt: service.now(),
	}, options.Response)
}

func (service *Service) finishAdditionalPasskey(writer http.ResponseWriter, request *http.Request) {
	authenticated, ok := service.RequireAuthenticated(writer, request, true, true)
	if !ok {
		return
	}
	ceremony, ok := service.takeCeremony(writer, request, "additional")
	if !ok || ceremony.User.ID != authenticated.User.ID {
		if ok {
			writeFailure(writer, http.StatusForbidden, "passkey_user_mismatch", "通行密钥登记账号不一致")
		}
		return
	}
	credential, err := service.webauthn.FinishRegistration(ceremony.User, ceremony.Session, request)
	if err != nil {
		writeFailure(writer, http.StatusUnauthorized, "passkey_invalid", "通行密钥验证失败")
		return
	}
	if err := service.repository.AddCredential(request.Context(), authenticated.User.ID,
		ceremony.Invitation.DisplayName, *credential, authenticated.Session.SessionVersion); err != nil {
		if errors.Is(err, ErrAuthorizationChanged) || errors.Is(err, ErrUserNotFound) {
			service.audit(request.Context(), authenticated.User.ID, "", request,
				"admin.passkey.add", "denied", "passkey", "", map[string]any{"reason": "authorization_changed"})
			service.expireSession(writer, request.Context(), TokenHash(authenticated.Token))
			return
		}
		if errors.Is(err, ErrCredentialExists) {
			writeFailure(writer, http.StatusConflict, "passkey_exists", "该通行密钥已经登记")
			return
		}
		writeFailure(writer, http.StatusServiceUnavailable, "passkey_unavailable", "暂时无法保存通行密钥")
		return
	}
	service.audit(request.Context(), authenticated.User.ID, "", request, "admin.passkey.add", "succeeded", "passkey", "", nil)
	writeJSON(writer, http.StatusCreated, map[string]any{"created": true})
}

func (service *Service) deletePasskey(writer http.ResponseWriter, request *http.Request) {
	authenticated, ok := service.RequireAuthenticated(writer, request, true, true)
	if !ok {
		return
	}
	credentialID, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(request.PathValue("credentialID")))
	if err != nil || len(credentialID) == 0 || len(credentialID) > 1024 {
		writeFailure(writer, http.StatusBadRequest, "invalid_passkey", "通行密钥标识无效")
		return
	}
	if err := service.repository.DeleteCredential(request.Context(), authenticated.User.ID, credentialID); err != nil {
		switch {
		case errors.Is(err, ErrLastCredential):
			service.audit(request.Context(), authenticated.User.ID, "", request,
				"admin.passkey.remove", "denied", "passkey", "", map[string]any{"reason": "last_passkey"})
			writeFailure(writer, http.StatusConflict, "last_passkey", "至少保留一个通行密钥")
		case errors.Is(err, ErrCredentialNotFound):
			writeFailure(writer, http.StatusNotFound, "passkey_not_found", "没有找到这个通行密钥")
		default:
			writeFailure(writer, http.StatusServiceUnavailable, "passkey_unavailable", "暂时无法移除通行密钥")
		}
		return
	}
	service.audit(request.Context(), authenticated.User.ID, "", request, "admin.passkey.remove", "succeeded", "passkey", "", nil)
	_ = service.state.DeleteSession(request.Context(), TokenHash(authenticated.Token))
	service.clearSessionCookie(writer)
	writer.WriteHeader(http.StatusNoContent)
}

func (service *Service) sessionStatus(writer http.ResponseWriter, request *http.Request) {
	authenticated, ok := service.RequireAuthenticated(writer, request, false, false)
	if !ok {
		return
	}
	writeJSON(writer, http.StatusOK, service.sessionPayload(authenticated.User, authenticated.Session))
}

func (service *Service) logout(writer http.ResponseWriter, request *http.Request) {
	authenticated, ok := service.RequireAuthenticated(writer, request, true, false)
	if !ok {
		return
	}
	_ = service.state.DeleteSession(request.Context(), TokenHash(authenticated.Token))
	service.clearSessionCookie(writer)
	service.audit(request.Context(), authenticated.User.ID, "", request, "admin.logout", "succeeded", "", "", nil)
	writer.WriteHeader(http.StatusNoContent)
}

func (service *Service) RequireAuthenticated(
	writer http.ResponseWriter,
	request *http.Request,
	csrf bool,
	reauthenticated bool,
) (Authenticated, bool) {
	token, session, user, ok := service.requireSession(writer, request, csrf)
	if !ok {
		return Authenticated{}, false
	}
	if reauthenticated && !service.recentlyReauthenticated(session) {
		writeFailure(writer, http.StatusUnauthorized, "reauthentication_required", "请再次验证通行密钥")
		return Authenticated{}, false
	}
	return Authenticated{Token: token, User: user, Session: session}, true
}

func (service *Service) RequirePermission(
	writer http.ResponseWriter,
	request *http.Request,
	permission Permission,
	appID string,
	csrf bool,
	reauthenticated bool,
) (Authenticated, bool) {
	authenticated, ok := service.RequireAuthenticated(writer, request, csrf, reauthenticated)
	if !ok {
		return Authenticated{}, false
	}
	if !authenticated.User.Allows(permission, appID) {
		service.audit(request.Context(), authenticated.User.ID, appID, request,
			"admin.permission."+string(permission), "denied", "permission", string(permission), nil)
		writeFailure(writer, http.StatusForbidden, "permission_denied", "当前角色没有执行此操作的权限")
		return Authenticated{}, false
	}
	return authenticated, true
}

func (service *Service) RecordAudit(
	ctx context.Context,
	userID string,
	appID string,
	request *http.Request,
	action string,
	outcome string,
	targetType string,
	targetID string,
	metadata map[string]any,
) {
	service.audit(ctx, userID, appID, request, action, outcome, targetType, targetID, metadata)
}

func (service *Service) requireSession(
	writer http.ResponseWriter,
	request *http.Request,
	csrf bool,
) (string, Session, User, bool) {
	token := sessionToken(request)
	if token == "" {
		writeFailure(writer, http.StatusUnauthorized, "authentication_required", "请使用通行密钥登录")
		return "", Session{}, User{}, false
	}
	hash := TokenHash(token)
	session, found, err := service.state.GetSession(request.Context(), hash)
	now := service.now()
	if err != nil {
		writeFailure(writer, http.StatusServiceUnavailable, "session_unavailable", "管理会话暂时不可用")
		return "", Session{}, User{}, false
	}
	idle := now.Sub(session.LastSeenAt)
	if !found || session.UserID == "" || !now.Before(session.ExpiresAt) ||
		idle < 0 || idle > sessionIdleLimit {
		service.expireSession(writer, request.Context(), hash)
		return "", Session{}, User{}, false
	}
	user, found, err := service.repository.UserByID(request.Context(), session.UserID)
	if err != nil {
		writeFailure(writer, http.StatusServiceUnavailable, "session_unavailable", "管理会话暂时不可用")
		return "", Session{}, User{}, false
	}
	if !found || user.Status != UserStatusActive || !user.Role.Valid() ||
		user.SessionVersion != session.SessionVersion {
		service.expireSession(writer, request.Context(), hash)
		return "", Session{}, User{}, false
	}
	if csrf && (request.Header.Get("Origin") != service.config.Origin ||
		request.Header.Get("X-Admin-CSRF") == "" || request.Header.Get("X-Admin-CSRF") != session.CSRFToken) {
		writeFailure(writer, http.StatusForbidden, "csrf_invalid", "请求安全验证失败")
		return "", Session{}, User{}, false
	}
	session.LastSeenAt = now
	if err := service.saveSession(request.Context(), token, session); err != nil {
		writeFailure(writer, http.StatusServiceUnavailable, "session_unavailable", "管理会话暂时不可用")
		return "", Session{}, User{}, false
	}
	return token, session, user, true
}

func (service *Service) completeAuthentication(
	writer http.ResponseWriter,
	request *http.Request,
	user User,
	action string,
	status int,
) {
	token, session, err := service.createSession(request.Context(), user, true)
	if err != nil {
		writeFailure(writer, http.StatusServiceUnavailable, "session_unavailable", "无法创建管理会话")
		return
	}
	service.setSessionCookie(writer, token)
	service.audit(request.Context(), user.ID, "", request, action, "succeeded", "admin_user", user.ID, nil)
	writeJSON(writer, status, service.sessionPayload(user, session))
}

func (service *Service) createSession(ctx context.Context, user User, reauthenticated bool) (string, Session, error) {
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
		UserID: user.ID, SessionVersion: user.SessionVersion, CSRFToken: csrf,
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
	elapsed := service.now().Sub(session.ReauthenticatedAt)
	return !session.ReauthenticatedAt.IsZero() && elapsed >= 0 && elapsed <= reauthLifetime
}

func (service *Service) saveCeremony(writer http.ResponseWriter, request *http.Request, state CeremonyState, publicKey any) {
	ceremonyID, err := RandomToken(24)
	if err != nil || service.state.PutCeremony(request.Context(), ceremonyID, state, ceremonyLifetime) != nil {
		writeFailure(writer, http.StatusServiceUnavailable, "passkey_unavailable", "无法保存通行密钥验证状态")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"ceremonyID": ceremonyID, "publicKey": publicKey})
}

func (service *Service) takeCeremony(writer http.ResponseWriter, request *http.Request, kind string) (CeremonyState, bool) {
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

func (service *Service) newUser(writer http.ResponseWriter, displayName string, role Role, appIDs []string) (User, bool) {
	displayName = cleanDisplayName(displayName)
	appIDs = uniqueStrings(appIDs)
	if displayName == "" || !role.Valid() || role == RoleOperator && len(appIDs) == 0 {
		writeFailure(writer, http.StatusBadRequest, "invalid_user", "人员名称或角色无效")
		return User{}, false
	}
	userID, err := NewUUID()
	if err != nil {
		writeFailure(writer, http.StatusServiceUnavailable, "temporarily_unavailable", "无法创建管理账号")
		return User{}, false
	}
	handle, err := RandomBytes(32)
	if err != nil {
		writeFailure(writer, http.StatusServiceUnavailable, "temporarily_unavailable", "无法创建管理账号")
		return User{}, false
	}
	return User{
		ID: userID, Handle: handle, DisplayName: displayName, Role: role,
		Status: UserStatusActive, SessionVersion: 1, AppIDs: append([]string{}, appIDs...),
	}, true
}

func (service *Service) sessionPayload(user User, session Session) map[string]any {
	return map[string]any{
		"authenticated":           true,
		"csrfToken":               session.CSRFToken,
		"expiresAt":               session.ExpiresAt,
		"recentlyReauthenticated": service.recentlyReauthenticated(session),
		"user": map[string]any{
			"id": user.ID, "displayName": user.DisplayName, "role": user.Role,
			"appIDs": append([]string{}, user.AppIDs...),
		},
	}
}

func (service *Service) expireSession(writer http.ResponseWriter, ctx context.Context, hash [32]byte) {
	_ = service.state.DeleteSession(ctx, hash)
	service.clearSessionCookie(writer)
	writeFailure(writer, http.StatusUnauthorized, "session_expired", "管理会话已过期")
}

func (service *Service) setSessionCookie(writer http.ResponseWriter, token string) {
	http.SetCookie(writer, &http.Cookie{
		Name: adminSessionCookie, Value: token, Path: "/", MaxAge: int(sessionLifetime.Seconds()),
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
}

func (service *Service) clearSessionCookie(writer http.ResponseWriter) {
	http.SetCookie(writer, &http.Cookie{
		Name: adminSessionCookie, Value: "", Path: "/", MaxAge: -1,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
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
	appID string,
	request *http.Request,
	action string,
	outcome string,
	targetType string,
	targetID string,
	metadata map[string]any,
) {
	requestID := validRequestID(request.Header.Get("X-Request-ID"))
	if requestID == "" {
		requestID, _ = NewUUID()
	}
	if metadata == nil {
		metadata = map[string]any{}
	}
	if err := service.repository.AppendAudit(ctx, AuditEvent{
		UserID: userID, AppID: appID, RequestID: requestID, Action: action,
		TargetType: targetType, TargetID: targetID, Outcome: outcome, Metadata: metadata,
	}); err != nil {
		log.Printf("admin audit append failed: request_id=%s action=%s outcome=%s: %v", requestID, action, outcome, err)
	}
}

func (service *Service) normalizedAppIDs(role Role, values []string) ([]string, bool) {
	if role == RoleAdmin {
		return []string{}, true
	}
	result := uniqueStrings(values)
	if len(result) == 0 {
		return nil, false
	}
	for _, appID := range result {
		if !slices.Contains(service.config.AppIDs, appID) {
			return nil, false
		}
	}
	return result, true
}

func validRequestID(value string) string {
	value = strings.TrimSpace(value)
	if len(value) != 36 {
		return ""
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if character != '-' {
				return ""
			}
			continue
		}
		if character < '0' || character > '9' && character < 'a' || character > 'f' {
			return ""
		}
	}
	return value
}

func cleanDisplayName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len([]rune(value)) > 64 {
		return ""
	}
	for _, character := range value {
		if unicode.IsControl(character) || unicode.In(character, unicode.Cf) {
			return ""
		}
	}
	return value
}

func uniqueStrings(values []string) []string {
	result := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	slices.Sort(result)
	return result
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
