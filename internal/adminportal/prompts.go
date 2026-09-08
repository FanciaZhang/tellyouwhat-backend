package adminportal

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tellyouwhat/backend/internal/adminauth"
	"github.com/tellyouwhat/backend/internal/adminhttpapi"
	"github.com/tellyouwhat/backend/internal/promptconfig"
)

func (s *Server) promptsAccess(c *gin.Context, write, reauth bool) (adminauth.Authenticated, bool) {
	auth, ok := s.auth.RequirePermission(c.Writer, c.Request, adminauth.PermissionAIConfigManage, "", write, reauth)
	if !ok {
		return auth, false
	}
	if s.config.Prompts == nil {
		writeFailure(c.Writer, 503, "prompts_unavailable", "提示词管理尚未配置")
		return auth, false
	}
	if write && !s.promptWritesEnabled() {
		writeFailure(c.Writer, 503, "writes_disabled", "管理写操作尚未启用")
		return auth, false
	}
	return auth, true
}
func promptsFailure(c *gin.Context, err error) {
	switch {
	case errors.Is(err, promptconfig.ErrInvalid):
		writeFailure(c.Writer, 422, "prompts_invalid", "请检查提示词、模型参数及默认风格是否有效")
	case errors.Is(err, promptconfig.ErrConflict):
		writeFailure(c.Writer, 409, "prompts_conflict", "配置已被修改或提交标识冲突，请刷新后重新预览")
	case errors.Is(err, promptconfig.ErrNotFound):
		writeFailure(c.Writer, 404, "prompts_not_found", "找不到这个配置版本")
	default:
		writeFailure(c.Writer, 503, "prompts_unavailable", "提示词管理暂时不可用，请稍后重试")
	}
}
func (s *Server) GetPromptConfig(c *gin.Context, params adminhttpapi.GetPromptConfigParams) {
	if _, ok := s.promptsAccess(c, false, false); !ok {
		return
	}
	r, err := s.config.Prompts.Current(c, params.Scope)
	if err != nil {
		promptsFailure(c, err)
		return
	}
	var sync any
	if cache := s.config.PromptCache; cache != nil {
		attempt, success, err := cache.Status()
		sync = map[string]any{"lastAttempt": attempt, "lastSuccess": success, "stale": err != nil}
	}
	writeJSON(c.Writer, 200, map[string]any{"current": r, "writesEnabled": s.promptWritesEnabled(), "scopes": promptconfig.Scopes(), "refreshSeconds": 60, "sync": sync})
}
func (s *Server) ListPromptHistory(c *gin.Context, params adminhttpapi.ListPromptHistoryParams) {
	if _, ok := s.promptsAccess(c, false, false); !ok {
		return
	}
	cursor := ""
	if params.Cursor != nil {
		cursor = *params.Cursor
	}
	h, err := s.config.Prompts.History(c, params.Scope, cursor)
	if err != nil {
		promptsFailure(c, err)
		return
	}
	writeJSON(c.Writer, 200, h)
}
func (s *Server) CreatePromptDraft(c *gin.Context, _ adminhttpapi.CreatePromptDraftParams) {
	auth, ok := s.promptsAccess(c, true, false)
	if !ok {
		return
	}
	var input promptconfig.DraftInput
	if !operationsBody(c, &input) {
		return
	}
	m, ok := aiMutation(c, auth.User.ID)
	if !ok {
		return
	}
	if replay, err := s.config.Prompts.Replay(c, m, "prompts.draft", input); err != nil {
		promptsFailure(c, err)
		return
	} else if replay != nil {
		writeJSON(c.Writer, 200, replay)
		return
	}
	if input.Policy.Validate(input.Scope) != nil {
		promptsFailure(c, promptconfig.ErrInvalid)
		return
	}
	raw, _ := json.Marshal(input.Policy)
	var resolved promptconfig.Policy
	_ = json.Unmarshal(raw, &resolved)
	if err := s.resolvePromptModels(c, &resolved); err != nil {
		promptsFailure(c, err)
		return
	}
	r, err := s.config.Prompts.DraftResolved(c, input, resolved, m, s.now())
	if err != nil {
		promptsFailure(c, err)
		return
	}
	writeJSON(c.Writer, 200, r)
}

type promptsClaim struct {
	Actor        string                   `json:"actor"`
	Publication  promptconfig.Publication `json:"publication"`
	PolicyDigest string                   `json:"policyDigest"`
	ExpiresAt    int64                    `json:"expiresAt"`
}

func promptsDigest(p promptconfig.Policy) string {
	raw, _ := json.Marshal(p)
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}
func (s *Server) signPromptClaim(claim promptsClaim) string {
	raw, _ := json.Marshal(claim)
	mac := hmac.New(sha256.New, s.config.PreviewSigningKey)
	mac.Write(raw)
	return base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
func (s *Server) verifyPromptClaim(token, actor string, p promptconfig.Publication, policy promptconfig.Policy) bool {
	if len(token) > 2048 || len(s.config.PreviewSigningKey) < 32 {
		return false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, s.config.PreviewSigningKey)
	mac.Write(raw)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return false
	}
	var claim promptsClaim
	return json.Unmarshal(raw, &claim) == nil && s.now().Unix() < claim.ExpiresAt && claim.Actor == actor && claim.Publication == p && claim.PolicyDigest == promptsDigest(policy)
}
func (s *Server) GetPromptRevision(c *gin.Context, revision string) {
	auth, ok := s.promptsAccess(c, false, false)
	if !ok {
		return
	}
	r, err := s.config.Prompts.Get(c, revision)
	if err != nil {
		promptsFailure(c, err)
		return
	}
	current, err := s.config.Prompts.Current(c, r.Scope)
	if err != nil {
		promptsFailure(c, err)
		return
	}
	canPublish := r.PublishedAt == nil && r.BaseVersion == current.ID && s.promptWritesEnabled()
	expires := s.now().Add(5 * time.Minute)
	token := ""
	if canPublish {
		token = s.signPromptClaim(promptsClaim{Actor: auth.User.ID, Publication: promptconfig.Publication{Scope: r.Scope, Revision: r.ID, BaseVersion: r.BaseVersion}, PolicyDigest: promptsDigest(r.Policy), ExpiresAt: expires.Unix()})
	}
	writeJSON(c.Writer, 200, map[string]any{"revision": r, "before": current.Policy, "after": r.Policy, "canPublish": canPublish, "previewToken": token, "expiresAt": expires})
}
func (s *Server) PublishPromptConfig(c *gin.Context, _ adminhttpapi.PublishPromptConfigParams) {
	auth, ok := s.promptsAccess(c, true, true)
	if !ok {
		return
	}
	var input struct {
		promptconfig.Publication
		PreviewToken string `json:"previewToken"`
	}
	if !operationsBody(c, &input) {
		return
	}
	m, ok := aiMutation(c, auth.User.ID)
	if !ok {
		return
	}
	if replay, err := s.config.Prompts.Replay(c, m, "prompts.publish", input.Publication); err != nil {
		promptsFailure(c, err)
		return
	} else if replay != nil {
		writeJSON(c.Writer, 200, replay)
		return
	}
	r, err := s.config.Prompts.Get(c, input.Revision)
	if err != nil {
		promptsFailure(c, err)
		return
	}
	if !s.verifyPromptClaim(input.PreviewToken, auth.User.ID, input.Publication, r.Policy) {
		writeFailure(c.Writer, 409, "prompts_preview_changed", "预览已过期或不匹配，请重新查看后再发布")
		return
	}
	r, err = s.config.Prompts.Publish(c, input.Publication, m, s.now())
	if err != nil {
		promptsFailure(c, err)
		return
	}
	writeJSON(c.Writer, 200, r)
}

func (s *Server) promptWritesEnabled() bool { return s.config.AI != nil && s.config.AI.WritesEnabled }
