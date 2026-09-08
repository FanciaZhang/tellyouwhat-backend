package adminportal

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tellyouwhat/backend/internal/adminauth"
	"github.com/tellyouwhat/backend/internal/adminhttpapi"
	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/platformops"
)

func (s *Server) operationsAccess(c *gin.Context, write, reauth bool) (adminauth.Authenticated, bool) {
	auth, ok := s.auth.RequirePermission(c.Writer, c.Request, adminauth.PermissionOperationsManage, "", write, reauth)
	if !ok {
		return auth, false
	}
	if s.config.Operations == nil {
		writeFailure(c.Writer, 503, "operations_unavailable", "服务管理尚未配置")
		return auth, false
	}
	if write && !s.config.OperationsWritesEnabled {
		writeFailure(c.Writer, 503, "writes_disabled", "管理写操作尚未启用")
		return auth, false
	}
	return auth, true
}
func operationsFailure(c *gin.Context, err error) {
	switch {
	case errors.Is(err, platformops.ErrInvalid):
		writeFailure(c.Writer, 422, "operations_invalid", "配置超出允许范围，请检查预算、额度与功能范围")
	case errors.Is(err, platformops.ErrConflict):
		writeFailure(c.Writer, 409, "operations_conflict", "配置已被修改或提交标识冲突，请刷新后重新预览")
	case errors.Is(err, platformops.ErrNotFound):
		writeFailure(c.Writer, 404, "operations_not_found", "找不到这个配置版本")
	default:
		writeFailure(c.Writer, 503, "operations_unavailable", "服务管理暂时不可用，请稍后重试")
	}
}
func operationsBody(c *gin.Context, target any) bool {
	d := json.NewDecoder(c.Request.Body)
	d.DisallowUnknownFields()
	if d.Decode(target) != nil || d.Decode(new(any)) != io.EOF {
		writeFailure(c.Writer, 400, "invalid_request", "请求格式无效")
		return false
	}
	return true
}
func (s *Server) GetOperationsConfig(c *gin.Context) {
	if _, ok := s.operationsAccess(c, false, false); !ok {
		return
	}
	r, err := s.config.Operations.Current(c)
	if err != nil {
		operationsFailure(c, err)
		return
	}
	writeJSON(c.Writer, 200, map[string]any{"current": r, "freeSessionReservationTokens": contracts.MaxFreeRecognitionSessionReservationTokens, "writesEnabled": s.config.OperationsWritesEnabled, "operations": map[string][]string{"health": platformops.Operations("health"), "journal": platformops.Operations("journal")}})
}
func (s *Server) GetOperationsMetrics(c *gin.Context) {
	if _, ok := s.operationsAccess(c, false, false); !ok {
		return
	}
	m, err := s.config.Operations.Metrics(c, s.now())
	if err != nil {
		operationsFailure(c, err)
		return
	}
	writeJSON(c.Writer, 200, struct {
		platformops.Metrics
		Billing any `json:"billing"`
	}{m, s.config.Billing.Snapshot(s.now())})
}
func (s *Server) ListOperationsHistory(c *gin.Context, params adminhttpapi.ListOperationsHistoryParams) {
	if _, ok := s.operationsAccess(c, false, false); !ok {
		return
	}
	cursor := ""
	if params.Cursor != nil {
		cursor = *params.Cursor
	}
	h, err := s.config.Operations.History(c, cursor)
	if err != nil {
		operationsFailure(c, err)
		return
	}
	writeJSON(c.Writer, 200, h)
}
func (s *Server) CreateOperationsDraft(c *gin.Context, _ adminhttpapi.CreateOperationsDraftParams) {
	auth, ok := s.operationsAccess(c, true, false)
	if !ok {
		return
	}
	var input platformops.DraftInput
	if !operationsBody(c, &input) {
		return
	}
	m, ok := aiMutation(c, auth.User.ID)
	if !ok {
		return
	}
	r, err := s.config.Operations.Draft(c, input, m, s.now())
	if err != nil {
		operationsFailure(c, err)
		return
	}
	writeJSON(c.Writer, 200, r)
}

type operationsClaim struct {
	Actor        string                  `json:"actor"`
	Publication  platformops.Publication `json:"publication"`
	PolicyDigest string                  `json:"policyDigest"`
	ExpiresAt    int64                   `json:"expiresAt"`
}

func operationsDigest(p platformops.Policy) string {
	raw, _ := json.Marshal(p)
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}
func (s *Server) signOperationsClaim(claim operationsClaim) string {
	raw, _ := json.Marshal(claim)
	mac := hmac.New(sha256.New, s.config.PreviewSigningKey)
	mac.Write(raw)
	return base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
func (s *Server) verifyOperationsClaim(token, actor string, p platformops.Publication, policy platformops.Policy) bool {
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
	var claim operationsClaim
	return json.Unmarshal(raw, &claim) == nil && s.now().Unix() < claim.ExpiresAt && claim.Actor == actor && claim.Publication == p && claim.PolicyDigest == operationsDigest(policy)
}
func (s *Server) GetOperationsRevision(c *gin.Context, revision string) {
	auth, ok := s.operationsAccess(c, false, false)
	if !ok {
		return
	}
	r, err := s.config.Operations.Get(c, revision)
	if err != nil {
		operationsFailure(c, err)
		return
	}
	current, err := s.config.Operations.Current(c)
	if err != nil {
		operationsFailure(c, err)
		return
	}
	canPublish := r.PublishedAt == nil && r.BaseVersion == current.ID && s.config.OperationsWritesEnabled
	expires := s.now().Add(5 * time.Minute)
	token := ""
	if canPublish {
		token = s.signOperationsClaim(operationsClaim{Actor: auth.User.ID, Publication: platformops.Publication{Revision: r.ID, BaseVersion: r.BaseVersion}, PolicyDigest: operationsDigest(r.Policy), ExpiresAt: expires.Unix()})
	}
	writeJSON(c.Writer, 200, map[string]any{"revision": r, "before": current.Policy, "after": r.Policy, "canPublish": canPublish, "previewToken": token, "expiresAt": expires})
}
func (s *Server) PublishOperationsConfig(c *gin.Context, _ adminhttpapi.PublishOperationsConfigParams) {
	auth, ok := s.operationsAccess(c, true, true)
	if !ok {
		return
	}
	var input struct {
		platformops.Publication
		PreviewToken string `json:"previewToken"`
	}
	if !operationsBody(c, &input) {
		return
	}
	m, ok := aiMutation(c, auth.User.ID)
	if !ok {
		return
	}
	if replay, err := s.config.Operations.Replay(c, m, "operations.publish", input.Publication); err != nil {
		operationsFailure(c, err)
		return
	} else if replay != nil {
		writeJSON(c.Writer, 200, replay)
		return
	}
	r, err := s.config.Operations.Get(c, input.Revision)
	if err != nil {
		operationsFailure(c, err)
		return
	}
	if !s.verifyOperationsClaim(input.PreviewToken, auth.User.ID, input.Publication, r.Policy) {
		writeFailure(c.Writer, 409, "operations_preview_changed", "预览已过期或不匹配，请重新查看后再发布")
		return
	}
	r, err = s.config.Operations.Publish(c, input.Publication, m, s.now())
	if err != nil {
		operationsFailure(c, err)
		return
	}
	writeJSON(c.Writer, 200, r)
}
