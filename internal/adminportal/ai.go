package adminportal

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/adminauth"
	"github.com/tellyouwhat/backend/internal/adminhttpapi"
	"github.com/tellyouwhat/backend/internal/aiconfig"
	"github.com/tellyouwhat/backend/internal/arkcontrol"
	"github.com/tellyouwhat/backend/internal/contracts"
	"io"
	"net/http"
)

type AIInventory interface {
	Endpoint(context.Context, string) (arkcontrol.Endpoint, error)
	Models(context.Context) ([]arkcontrol.Model, error)
}
type AIConfig struct {
	WritesEnabled         bool
	EndpointWritesEnabled bool
	SharedEndpoints       map[string]bool
	TimeoutSeconds        int
	Store                 aiconfig.Store
	Inventory             AIInventory
	Endpoints             map[contracts.Operation]string
}

func (s *Server) aiAccess(c *gin.Context, write bool) (adminauth.Authenticated, bool) {
	auth, ok := s.auth.RequirePermission(c.Writer, c.Request, adminauth.PermissionAIConfigManage, "health", write, write)
	if !ok {
		return auth, false
	}
	if s.config.AI == nil || s.config.AI.Store == nil || s.config.AI.Inventory == nil {
		writeFailure(c.Writer, 503, "ai_not_configured", "尚未配置火山管理服务身份")
		return auth, false
	}
	if write && !s.config.AI.WritesEnabled {
		writeFailure(c.Writer, 503, "writes_disabled", "管理写操作尚未启用")
		return auth, false
	}
	return auth, true
}
func (s *Server) GetHealthAIConfig(c *gin.Context) {
	if _, ok := s.aiAccess(c, false); !ok {
		return
	}
	rows := make([]any, 0)
	for _, op := range contracts.OperationValues() {
		current, err := s.config.AI.Store.Current(c, op)
		if err != nil {
			writeFailure(c.Writer, 503, "ai_config_unavailable", "配置读取失败")
			return
		}
		history, err := s.config.AI.Store.HistoryPage(c, op, "")
		if err != nil {
			writeFailure(c.Writer, 503, "ai_config_unavailable", "配置记录读取失败")
			return
		}
		id := s.config.AI.Endpoints[op]
		if current != nil {
			id = current.Policy.Endpoint
		}
		endpoint, err := s.config.AI.Inventory.Endpoint(c, id)
		status := ""
		if err != nil {
			status = "接入点同步失败"
		}
		rows = append(rows, map[string]any{"operation": op, "current": current, "history": history.Revisions, "nextCursor": history.NextCursor, "endpoint": endpoint, "syncError": status})
	}
	writeJSON(c.Writer, 200, map[string]any{"operations": rows, "syncedAt": s.now(), "timeoutSeconds": s.config.AI.TimeoutSeconds, "writesEnabled": s.config.AI.WritesEnabled, "rollingWritesEnabled": false})
}
func (s *Server) ListAIModels(c *gin.Context) {
	if _, ok := s.aiAccess(c, false); !ok {
		return
	}
	models, err := s.config.AI.Inventory.Models(c)
	if err != nil {
		writeFailure(c.Writer, 503, "ark_unavailable", "模型目录同步失败")
		return
	}
	writeJSON(c.Writer, 200, map[string]any{"models": models, "syncedAt": s.now()})
}
func (s *Server) validateAIPolicy(ctx context.Context, op contracts.Operation, p contracts.ExecutionPolicy) error {
	if p.Validate(op) != nil {
		return aiconfig.ErrInvalid
	}
	// Only existing health bindings with the verified baseline model are accepted.
	// Native model changes remain unavailable until capability/cost certification.
	allowed := false
	for _, id := range s.config.AI.Endpoints {
		if id != "" && p.Endpoint == id {
			allowed = true
		}
	}
	if !allowed || s.config.AI.SharedEndpoints[p.Endpoint] {
		return aiconfig.ErrInvalid
	}
	ep, err := s.config.AI.Inventory.Endpoint(ctx, p.Endpoint)
	if err != nil {
		return err
	}
	if ep.Status != "Running" || ep.RollingID != "" || ep.Model.FoundationModel.Name != "doubao-seed-2-0-mini" || ep.Model.FoundationModel.Version != "260428" {
		return aiconfig.ErrInvalid
	}
	if p.ReasoningEffort == "max" || p.ReasoningEffort == "" {
		return aiconfig.ErrInvalid
	}
	return nil
}
func (s *Server) CreateHealthAIDraft(c *gin.Context, _ adminhttpapi.CreateHealthAIDraftParams) {
	s.createAIDraft(c)
}
func (s *Server) PublishHealthAIConfig(c *gin.Context, _ adminhttpapi.PublishHealthAIConfigParams) {
	s.publishAIConfig(c)
}

func (s *Server) createAIDraft(c *gin.Context) {
	auth, ok := s.aiAccess(c, true)
	if !ok {
		return
	}
	var body struct {
		Operation   contracts.Operation       `json:"operation"`
		BaseVersion string                    `json:"baseVersion"`
		Policy      contracts.ExecutionPolicy `json:"policy"`
	}
	d := json.NewDecoder(c.Request.Body)
	d.DisallowUnknownFields()
	if d.Decode(&body) != nil || d.Decode(new(any)) != io.EOF {
		writeFailure(c.Writer, 400, "invalid_request", "配置格式无效")
		return
	}
	id := uuid.NewString()
	body.Policy.Version = id
	if s.config.AI.TimeoutSeconds > 0 {
		body.Policy.TimeoutSeconds = s.config.AI.TimeoutSeconds
	}

	r := aiconfig.Revision{ID: id, Operation: body.Operation, BaseVersion: body.BaseVersion, Policy: body.Policy, CreatedBy: auth.User.ID, CreatedAt: s.now()}
	mutation, ok := aiMutation(c, auth.User.ID)
	if !ok {
		return
	}
	if s.replayAI(c, mutation, aiconfig.DraftAction, aiconfig.DraftInput(r)) {
		return
	}
	if err := s.validateAIPolicy(c, body.Operation, body.Policy); err != nil {
		aiFailure(c, err)
		return
	}
	saved, err := s.config.AI.Store.Draft(c, r, mutation)
	if err != nil {
		aiFailure(c, err)
		return
	}
	writeJSON(c.Writer, 200, saved)
}
func (s *Server) publishAIConfig(c *gin.Context) {
	auth, ok := s.aiAccess(c, true)
	if !ok {
		return
	}
	var body struct {
		aiconfig.Publication
		PreviewToken string `json:"previewToken"`
	}
	d := json.NewDecoder(c.Request.Body)
	d.DisallowUnknownFields()
	if d.Decode(&body) != nil || d.Decode(new(any)) != io.EOF {
		writeFailure(c.Writer, 400, "invalid_request", "发布参数无效")
		return
	}
	mutation, ok := aiMutation(c, auth.User.ID)
	if !ok {
		return
	}
	if s.replayAI(c, mutation, aiconfig.PublishAction, body.Publication) {
		return
	}
	target, err := s.config.AI.Store.Get(c, body.Operation, body.Revision)
	if err != nil {
		aiFailure(c, err)
		return
	}
	if err = s.validateAIPolicy(c, body.Operation, target.Policy); err != nil {
		aiFailure(c, err)
		return
	}
	preview, err := s.aiPreview(c, auth.User.ID, target)
	if err != nil {
		aiFailure(c, err)
		return
	}
	if !s.verifyAIPreview(preview.claim, body.PreviewToken) {
		writeFailure(c.Writer, 409, "ai_preview_changed", "确认内容已变化或过期，请重新查看发布预览")
		return
	}
	published, err := s.config.AI.Store.Publish(c, body.Publication, mutation, s.now())
	if err != nil {
		aiFailure(c, err)
		return
	}
	writeJSON(c.Writer, 200, published)
}

func aiMutation(c *gin.Context, actor string) (aiconfig.Mutation, bool) {
	m := aiconfig.Mutation{Actor: actor, Key: c.GetHeader("Idempotency-Key"), RequestID: uuid.NewString()}
	if !aiconfig.ValidMutation(m) {
		writeFailure(c.Writer, 400, "idempotency_key_required", "缺少有效的防重复提交标识")
		return m, false
	}
	return m, true
}
func (s *Server) replayAI(c *gin.Context, m aiconfig.Mutation, action string, body any) bool {
	r, err := s.config.AI.Store.Replay(c, m, action, body)
	if err != nil {
		aiFailure(c, err)
		return true
	}
	if r != nil {
		writeJSON(c.Writer, 200, r)
		return true
	}
	return false
}
func aiFailure(c *gin.Context, err error) {
	switch {
	case errors.Is(err, aiconfig.ErrConflict):
		writeFailure(c.Writer, 409, "ai_version_conflict", "配置已变化或提交标识被重复使用，请刷新后重试")
	case errors.Is(err, aiconfig.ErrNotFound):
		writeFailure(c.Writer, 404, "ai_revision_not_found", "找不到该配置版本")
	case errors.Is(err, aiconfig.ErrInvalid), errors.Is(err, contracts.ErrContractViolation):
		writeFailure(c.Writer, 422, "ai_policy_unsupported", "配置不受支持，请检查功能与模型能力")
	default:
		writeFailure(c.Writer, http.StatusServiceUnavailable, "ai_service_unavailable", "配置或火山服务暂时不可用，请稍后重试")
	}
}
