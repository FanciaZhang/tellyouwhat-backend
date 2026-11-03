package adminportal

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tellyouwhat/backend/internal/adminauth"
	"github.com/tellyouwhat/backend/internal/adminhttpapi"
	"github.com/tellyouwhat/backend/internal/aiconfig"
	"github.com/tellyouwhat/backend/internal/airollout"
	"github.com/tellyouwhat/backend/internal/contracts"
)

type rollingClaim struct {
	Actor    string             `json:"actor"`
	Input    airollout.Input    `json:"input"`
	Snapshot airollout.Snapshot `json:"snapshot"`
	Expires  int64              `json:"expires"`
}

func (s *Server) signRolling(claim rollingClaim) string {
	raw, _ := json.Marshal(claim)
	mac := hmac.New(sha256.New, s.config.PreviewSigningKey)
	mac.Write(raw)
	return base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
func (s *Server) readRollingToken(token string, actor string, in airollout.Input) (rollingClaim, bool) {
	var out rollingClaim
	if len(token) > 8192 || len(s.config.PreviewSigningKey) < 32 {
		return out, false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return out, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return out, false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return out, false
	}
	mac := hmac.New(sha256.New, s.config.PreviewSigningKey)
	mac.Write(raw)
	if !hmac.Equal(sig, mac.Sum(nil)) || json.Unmarshal(raw, &out) != nil {
		return out, false
	}
	return out, out.Actor == actor && out.Input == in && out.Expires > s.now().Unix()
}
func (s *Server) CreateAIRollingPreview(c *gin.Context, _ adminhttpapi.CreateAIRollingPreviewParams) {
	auth, ok := s.aiPreviewAccess(c)
	if !ok {
		return
	}
	if s.config.AI.Rollouts == nil || !s.config.AI.Rollouts.WritesEnabled || len(s.config.PreviewSigningKey) < 32 {
		aiFailure(c, aiconfig.ErrInvalid)
		return
	}
	var in airollout.Input
	d := json.NewDecoder(c.Request.Body)
	d.DisallowUnknownFields()
	if d.Decode(&in) != nil || d.Decode(new(any)) != io.EOF {
		aiFailure(c, aiconfig.ErrInvalid)
		return
	}
	snap, err := s.config.AI.Rollouts.Preview(c, in)
	if err != nil {
		aiFailure(c, err)
		return
	}
	claim := rollingClaim{Actor: auth.User.ID, Input: in, Snapshot: snap, Expires: s.now().Add(5 * time.Minute).Unix()}
	affected := []string{}
	for _, op := range contracts.OperationValues() {
		current, err := s.config.AI.Store.Current(c, op)
		if err != nil {
			aiFailure(c, err)
			return
		}
		ep := s.config.AI.Endpoints[op]
		if current != nil {
			ep = current.Policy.Endpoint
		}
		if ep == in.Endpoint {
			affected = append(affected, string(op))
		}
	}
	writeJSON(c.Writer, 200, map[string]any{"previewToken": s.signRolling(claim), "snapshot": snap, "input": in, "affectedOperations": affected, "expiresAt": claim.Expires, "notice": "灰度由火山自动推进，无需手动推进阶段。灰度中可请求回退或撤销；完成后切回原模型需重新检查并开始新的灰度。已排队任务保留参数，实际模型随灰度变化。"})
}
func (s *Server) SubmitAIRollingCommand(c *gin.Context, _ adminhttpapi.SubmitAIRollingCommandParams) {
	auth, ok := s.aiAccess(c, true)
	if !ok {
		return
	}
	if s.config.AI.Rollouts == nil || !s.config.AI.Rollouts.WritesEnabled {
		aiFailure(c, aiconfig.ErrInvalid)
		return
	}
	var body struct {
		Input        airollout.Input `json:"input"`
		PreviewToken string          `json:"previewToken"`
	}
	d := json.NewDecoder(c.Request.Body)
	d.DisallowUnknownFields()
	if d.Decode(&body) != nil || d.Decode(new(any)) != io.EOF {
		aiFailure(c, aiconfig.ErrInvalid)
		return
	}
	m, ok := aiMutation(c, auth.User.ID)
	if !ok {
		return
	}
	if !s.config.AI.Rollouts.Allowed(body.Input.Endpoint) {
		aiFailure(c, aiconfig.ErrInvalid)
		return
	}
	if old, err := s.config.AI.Rollouts.Store.Replay(c, m, body.Input); err != nil {
		aiFailure(c, err)
		return
	} else if old != nil {
		writeJSON(c.Writer, 202, old)
		return
	}
	claim, ok := s.readRollingToken(body.PreviewToken, auth.User.ID, body.Input)
	if !ok {
		aiFailure(c, aiconfig.ErrConflict)
		return
	}
	command, err := s.config.AI.Rollouts.Submit(c, m, body.Input, claim.Snapshot)
	if err != nil {
		aiFailure(c, err)
		return
	}
	writeJSON(c.Writer, 202, command)
}

// Preflight and unpublished drafts require the managing role and CSRF protection.
// Fresh Passkey verification is reserved for the final online mutation.
func (s *Server) aiPreviewAccess(c *gin.Context) (adminauth.Authenticated, bool) {
	auth, ok := s.aiAccess(c, false)
	if !ok {
		return auth, false
	}
	auth, ok = s.auth.RequirePermission(c.Writer, c.Request, adminauth.PermissionAIConfigManage, "health", true, false)
	if !ok {
		return auth, false
	}
	if !s.config.AI.WritesEnabled {
		writeFailure(c.Writer, 503, "writes_disabled", "管理写操作尚未启用")
		return auth, false
	}
	return auth, true
}
