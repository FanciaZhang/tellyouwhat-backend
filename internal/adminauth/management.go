package adminauth

import (
	"errors"
	"net/http"
	"strings"
)

func (service *Service) registerManagementRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/admin/users", service.listUsers)
	mux.HandleFunc("PATCH /api/v1/admin/users/{userID}", service.updateUser)
	mux.HandleFunc("POST /api/v1/admin/invitations", service.createUserInvitation)
	mux.HandleFunc("DELETE /api/v1/admin/invitations/{invitationID}", service.revokeInvitation)
	mux.HandleFunc("POST /api/v1/admin/users/{userID}/recovery-invitations", service.createRecoveryInvitation)
	mux.HandleFunc("GET /api/v1/admin/audit", service.listAudit)
}

func (service *Service) listUsers(writer http.ResponseWriter, request *http.Request) {
	if _, ok := service.RequirePermission(writer, request, PermissionUsersManage, "", false, false); !ok {
		return
	}
	users, err := service.repository.ListUsers(request.Context())
	if err != nil {
		writeFailure(writer, http.StatusServiceUnavailable, "users_unavailable", "暂时无法读取后台人员")
		return
	}
	invitations, err := service.repository.ListInvitations(request.Context(), service.now())
	if err != nil {
		writeFailure(writer, http.StatusServiceUnavailable, "invitations_unavailable", "暂时无法读取待处理邀请")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"users": users, "invitations": invitations})
}

func (service *Service) createUserInvitation(writer http.ResponseWriter, request *http.Request) {
	authenticated, ok := service.RequirePermission(writer, request, PermissionUsersManage, "", true, true)
	if !ok {
		return
	}
	var input struct {
		DisplayName string   `json:"displayName"`
		Role        Role     `json:"role"`
		AppIDs      []string `json:"appIDs"`
	}
	if !decodeJSON(writer, request, &input) {
		return
	}
	displayName := cleanDisplayName(input.DisplayName)
	appIDs, valid := service.normalizedAppIDs(input.Role, input.AppIDs)
	if displayName == "" || !input.Role.Valid() || !valid {
		writeFailure(writer, http.StatusBadRequest, "invalid_invitation", "人员名称、角色或 App 范围无效")
		return
	}
	invitation, token, err := service.issueInvitation(
		request, authenticated.User.ID, InvitationKindCreate, "", displayName, input.Role, appIDs)
	if err != nil {
		writeFailure(writer, http.StatusServiceUnavailable, "invitation_unavailable", "暂时无法创建邀请")
		return
	}
	service.audit(request.Context(), authenticated.User.ID, "", request, "admin.invitation.create",
		"succeeded", "invitation", invitation.ID, map[string]any{"role": input.Role, "appIDs": appIDs})
	writeJSON(writer, http.StatusCreated, map[string]any{
		"invitation": invitation, "enrollmentURL": service.config.Origin + "/enroll#token=" + token,
	})
}

func (service *Service) createRecoveryInvitation(writer http.ResponseWriter, request *http.Request) {
	authenticated, ok := service.RequirePermission(writer, request, PermissionUsersManage, "", true, true)
	if !ok {
		return
	}
	targetID := cleanIdentifier(request.PathValue("userID"))
	if targetID == "" {
		writeFailure(writer, http.StatusBadRequest, "invalid_user", "人员标识无效")
		return
	}
	if targetID == authenticated.User.ID {
		service.audit(request.Context(), authenticated.User.ID, "", request,
			"admin.passkey.recovery_invite", "denied", "admin_user", targetID,
			map[string]any{"reason": "self_recovery"})
		writeFailure(writer, http.StatusConflict, "self_recovery_forbidden", "不能为当前登录账号签发恢复邀请")
		return
	}
	target, found, err := service.repository.UserByID(request.Context(), targetID)
	if err != nil {
		writeFailure(writer, http.StatusServiceUnavailable, "users_unavailable", "暂时无法读取后台人员")
		return
	}
	if !found || target.Status != UserStatusActive {
		writeFailure(writer, http.StatusNotFound, "user_not_found", "没有找到可恢复的后台人员")
		return
	}
	invitation, token, err := service.issueInvitation(
		request, authenticated.User.ID, InvitationKindRecovery, target.ID,
		target.DisplayName, target.Role, target.AppIDs)
	if err != nil {
		writeFailure(writer, http.StatusServiceUnavailable, "invitation_unavailable", "暂时无法创建恢复邀请")
		return
	}
	service.audit(request.Context(), authenticated.User.ID, "", request, "admin.passkey.recovery_invite",
		"succeeded", "admin_user", target.ID, nil)
	writeJSON(writer, http.StatusCreated, map[string]any{
		"invitation": invitation, "enrollmentURL": service.config.Origin + "/enroll#token=" + token,
	})
}

func (service *Service) issueInvitation(
	request *http.Request,
	actorID string,
	kind InvitationKind,
	targetID string,
	displayName string,
	role Role,
	appIDs []string,
) (Invitation, string, error) {
	id, err := NewUUID()
	if err != nil {
		return Invitation{}, "", err
	}
	token, err := RandomToken(32)
	if err != nil {
		return Invitation{}, "", err
	}
	now := service.now()
	invitation := Invitation{
		ID: id, Kind: kind, TargetUserID: targetID, InvitedByID: actorID,
		DisplayName: displayName, Role: role, AppIDs: appIDs,
		ExpiresAt: now.Add(invitationLifetime), CreatedAt: now,
	}
	if err := service.repository.CreateInvitation(request.Context(), TokenHash(token), invitation); err != nil {
		return Invitation{}, "", err
	}
	return invitation, token, nil
}

func (service *Service) revokeInvitation(writer http.ResponseWriter, request *http.Request) {
	authenticated, ok := service.RequirePermission(writer, request, PermissionUsersManage, "", true, true)
	if !ok {
		return
	}
	invitationID := cleanIdentifier(request.PathValue("invitationID"))
	if invitationID == "" {
		writeFailure(writer, http.StatusBadRequest, "invalid_invitation", "邀请标识无效")
		return
	}
	if err := service.repository.RevokeInvitation(request.Context(), invitationID, service.now()); err != nil {
		if errors.Is(err, ErrInvitationUsed) {
			writeFailure(writer, http.StatusNotFound, "invitation_not_found", "邀请不存在或已经失效")
			return
		}
		writeFailure(writer, http.StatusServiceUnavailable, "invitation_unavailable", "暂时无法撤销邀请")
		return
	}
	service.audit(request.Context(), authenticated.User.ID, "", request, "admin.invitation.revoke",
		"succeeded", "invitation", invitationID, nil)
	writer.WriteHeader(http.StatusNoContent)
}

func (service *Service) updateUser(writer http.ResponseWriter, request *http.Request) {
	authenticated, ok := service.RequirePermission(writer, request, PermissionUsersManage, "", true, true)
	if !ok {
		return
	}
	userID := cleanIdentifier(request.PathValue("userID"))
	if userID == "" {
		writeFailure(writer, http.StatusBadRequest, "invalid_user", "人员标识无效")
		return
	}
	var input struct {
		DisplayName string     `json:"displayName"`
		Role        Role       `json:"role"`
		Status      UserStatus `json:"status"`
		AppIDs      []string   `json:"appIDs"`
	}
	if !decodeJSON(writer, request, &input) {
		return
	}
	displayName := cleanDisplayName(input.DisplayName)
	appIDs, valid := service.normalizedAppIDs(input.Role, input.AppIDs)
	if displayName == "" || !input.Role.Valid() || !input.Status.Valid() || !valid {
		writeFailure(writer, http.StatusBadRequest, "invalid_user", "人员名称、角色、状态或 App 范围无效")
		return
	}
	updated, err := service.repository.UpdateUser(request.Context(), userID, UserUpdate{
		DisplayName: displayName, Role: input.Role, Status: input.Status, AppIDs: appIDs,
	}, service.now())
	if err != nil {
		switch {
		case errors.Is(err, ErrLastAdmin):
			service.audit(request.Context(), authenticated.User.ID, "", request,
				"admin.user.update", "denied", "admin_user", userID,
				map[string]any{"reason": "last_admin"})
			writeFailure(writer, http.StatusConflict, "last_admin", "必须至少保留一名启用中的管理员")
		case errors.Is(err, ErrUserNotFound):
			writeFailure(writer, http.StatusNotFound, "user_not_found", "没有找到这个后台人员")
		default:
			writeFailure(writer, http.StatusServiceUnavailable, "user_update_failed", "暂时无法更新后台人员")
		}
		return
	}
	service.audit(request.Context(), authenticated.User.ID, "", request, "admin.user.update",
		"succeeded", "admin_user", userID, map[string]any{
			"role": updated.Role, "status": updated.Status, "appIDs": updated.AppIDs,
		})
	selfSessionInvalidated := userID == authenticated.User.ID &&
		updated.SessionVersion != authenticated.User.SessionVersion
	response := map[string]any{"user": userSummary(updated), "sessionInvalidated": selfSessionInvalidated}
	if selfSessionInvalidated {
		_ = service.state.DeleteSession(request.Context(), TokenHash(authenticated.Token))
		service.clearSessionCookie(writer)
	}
	writeJSON(writer, http.StatusOK, response)
}

func (service *Service) listAudit(writer http.ResponseWriter, request *http.Request) {
	authenticated, ok := service.RequireAuthenticated(writer, request, false, false)
	if !ok {
		return
	}
	all := authenticated.User.Allows(PermissionAuditReadAll, "")
	records, err := service.repository.ListAudit(request.Context(), authenticated.User.ID, all, 100)
	if err != nil {
		writeFailure(writer, http.StatusServiceUnavailable, "audit_unavailable", "暂时无法读取操作记录")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"events": records, "scope": map[bool]string{true: "all", false: "self"}[all]})
}

func userSummary(user User) map[string]any {
	return map[string]any{
		"id": user.ID, "displayName": user.DisplayName, "role": user.Role,
		"status": user.Status, "appIDs": user.AppIDs, "credentialCount": len(user.Credentials),
	}
}

func cleanIdentifier(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return ""
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '-' || character == '_' {
			continue
		}
		return ""
	}
	return value
}
