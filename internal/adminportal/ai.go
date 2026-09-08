package adminportal

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/adminauth"
	"github.com/tellyouwhat/backend/internal/adminhttpapi"
	"github.com/tellyouwhat/backend/internal/aiconfig"
	"github.com/tellyouwhat/backend/internal/airollout"
	"github.com/tellyouwhat/backend/internal/arkcontrol"
	"github.com/tellyouwhat/backend/internal/contracts"
)

type AIInventory interface {
	Endpoint(context.Context, string) (arkcontrol.Endpoint, error)
	Models(context.Context) ([]arkcontrol.Model, error)
}
type AIConfig struct {
	Rollouts        *airollout.Service
	cacheOnce       sync.Once
	cache           *aiInventoryCache
	WritesEnabled   bool
	SharedEndpoints map[string]bool
	TimeoutSeconds  int
	Store           aiconfig.Store
	Inventory       AIInventory
	Endpoints       map[contracts.Operation]string
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
	rows := make([]map[string]any, 0)
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
		rows = append(rows, map[string]any{"operation": op, "current": current, "history": history.Revisions, "nextCursor": history.NextCursor, "endpointID": id})
	}
	ctx, cancel := context.WithTimeout(c, 20*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	slots := make(chan struct{}, 4)
	for _, row := range rows {
		wg.Add(1)
		go func(row map[string]any) {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				row["syncError"] = "同步超时"
				row["endpoint"] = arkcontrol.Endpoint{}
				return
			}
			snapshot := s.config.AI.endpointSnapshot(ctx, row["endpointID"].(string))
			row["endpoint"] = snapshot.Value
			row["syncedAt"] = snapshot.SyncedAt
			row["syncError"] = snapshot.SyncError
			row["stale"] = snapshot.Stale
		}(row)
	}
	wg.Wait()
	owned := []string{}
	seen := map[string]bool{}
	for _, op := range contracts.OperationValues() {
		id := s.config.AI.Endpoints[op]
		if id != "" && !seen[id] && !s.config.AI.SharedEndpoints[id] {
			owned = append(owned, id)
			seen[id] = true
		}
	}
	writeJSON(c.Writer, 200, map[string]any{"operations": rows, "ownedEndpoints": owned, "timeoutSeconds": s.config.AI.TimeoutSeconds, "writesEnabled": s.config.AI.WritesEnabled, "rollingWritesEnabled": s.config.AI.Rollouts != nil && s.config.AI.Rollouts.WritesEnabled})
}
func (s *Server) ListAIModels(c *gin.Context) {
	if _, ok := s.aiAccess(c, false); !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c, 25*time.Second)
	defer cancel()
	cache := s.config.AI.inventoryCache()
	models := cache.models.get(ctx, s.config.AI.Inventory.Models)
	if models.SyncedAt == nil {
		writeFailure(c.Writer, 503, "ark_unavailable", "模型目录同步失败")
		return
	}
	writeJSON(c.Writer, 200, map[string]any{"models": models.Value, "syncedAt": models.SyncedAt, "stale": models.Stale, "syncError": models.SyncError})
}
func (s *Server) validateAIPolicy(ctx context.Context, op contracts.Operation, p contracts.ExecutionPolicy) error {
	if p.Validate(op) != nil || p.ReasoningEffort == "max" {
		return aiconfig.ErrInvalid
	}
	// Only exclusively owned Health bindings with a compatible model are accepted.
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
	if ep.Status != "Running" {
		return aiconfig.ErrInvalid
	}
	model := arkcontrol.FoundationModel{Name: ep.Model.FoundationModel.Name, Version: ep.Model.FoundationModel.Version}
	check := func(model arkcontrol.FoundationModel) error {
		if s.config.AI.Rollouts != nil {
			return s.config.AI.Rollouts.CheckModel(ctx, model, []airollout.Requirement{{Operation: op, Effort: p.ReasoningEffort, Search: p.WebSearchEnabled}})
		}
		if !airollout.Supports(model, op, p) {
			return aiconfig.ErrInvalid
		}
		return nil
	}
	if err := check(model); err != nil {
		return err
	}
	if service := s.config.AI.Rollouts; service != nil {
		pending, err := service.Store.Pending(ctx, p.Endpoint)
		if err != nil {
			return err
		}
		for _, command := range pending {
			if command.Input.Action == "start" {
				if err = check(command.Input.Target); err != nil {
					return err
				}
			}
		}
	}
	if ep.RollingID != "" {
		inventory, ok := s.config.AI.Inventory.(interface {
			Rolling(context.Context, string) (arkcontrol.Rolling, error)
		})
		if !ok {
			return aiconfig.ErrInvalid
		}
		rolling, err := inventory.Rolling(ctx, ep.RollingID)
		if err != nil {
			return err
		}
		if rolling.EndpointID != p.Endpoint {
			return aiconfig.ErrInvalid
		}
		if rolling.Gray < 100 && !(rolling.Gray == 0 && rolling.Status == "Reverted") {
			if err = check(rolling.In); err != nil {
				return err
			}
			if err = check(rolling.Out); err != nil {
				return err
			}
		}
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
	auth, ok := s.aiPreviewAccess(c)
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
	var published aiconfig.Revision
	publish := func() error {
		if err := s.validateAIPolicy(c, body.Operation, target.Policy); err != nil {
			return err
		}
		var err error
		published, err = s.config.AI.Store.Publish(c, body.Publication, mutation, s.now())
		return err
	}
	if service := s.config.AI.Rollouts; service != nil {
		err = service.Store.WithLock(c, target.Policy.Endpoint, publish)
	} else {
		err = publish()
	}
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
	var check *airollout.CheckError
	switch {
	case errors.Is(err, airollout.ErrChecking):
		writeJSON(c.Writer, 202, map[string]any{"checking": true, "message": "正在检查模型协议与当前功能参数", "retryAfterSeconds": 3})
	case errors.As(err, &check):
		writeJSON(c.Writer, 422, map[string]any{"error": map[string]any{"code": "ai_check_failed", "message": check.Message, "stage": check.Stage}})
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

func (s *Server) ListAIModelVersions(c *gin.Context, model string) {
	if _, ok := s.aiAccess(c, false); !ok {
		return
	}
	inventory, ok := s.config.AI.Inventory.(interface {
		Versions(context.Context, string) ([]arkcontrol.Version, error)
	})
	if !ok {
		aiFailure(c, arkcontrol.ErrUnavailable)
		return
	}
	versions, err := inventory.Versions(c, model)
	if err != nil {
		aiFailure(c, err)
		return
	}
	activation := inventorySnapshot[[]arkcontrol.Activation]{Stale: true}
	if prices, ok := s.config.AI.Inventory.(interface {
		ModelActivations(context.Context, []string) ([]arkcontrol.Activation, error)
	}); ok {
		activation = s.config.AI.modelPriceSnapshot(c, model, prices.ModelActivations)
	}
	writeJSON(c.Writer, 200, map[string]any{"versions": versions, "activation": activation, "syncedAt": s.now(), "catalogVersion": airollout.CatalogVersion})
}
func (s *Server) GetAIEndpoint(c *gin.Context, endpoint string) {
	if _, ok := s.aiAccess(c, false); !ok {
		return
	}
	allowed := false
	for _, id := range s.config.AI.Endpoints {
		if id != "" && id == endpoint {
			allowed = true
		}
	}
	if !allowed || s.config.AI.SharedEndpoints[endpoint] {
		aiFailure(c, aiconfig.ErrNotFound)
		return
	}
	ep, err := s.config.AI.Inventory.Endpoint(c, endpoint)
	if err != nil {
		aiFailure(c, err)
		return
	}
	var rolling *arkcontrol.Rolling
	if ep.RollingID != "" {
		inventory, ok := s.config.AI.Inventory.(interface {
			Rolling(context.Context, string) (arkcontrol.Rolling, error)
		})
		if !ok {
			aiFailure(c, arkcontrol.ErrUnavailable)
			return
		}
		value, err := inventory.Rolling(c, ep.RollingID)
		if err != nil {
			aiFailure(c, err)
			return
		}
		if value.EndpointID != endpoint {
			aiFailure(c, arkcontrol.ErrResponse)
			return
		}
		rolling = &value
	}
	var priceState *airollout.PriceState
	attempts := []airollout.ModelAttempt{}
	commands := []airollout.Command{}
	enabled := false
	if r := s.config.AI.Rollouts; r != nil {
		commands, err = r.Store.List(c, endpoint)
		if err != nil {
			aiFailure(c, err)
			return
		}
		priceState, err = r.Store.PriceState(c, endpoint)
		if err != nil {
			aiFailure(c, err)
			return
		}
		attempts, err = r.Store.Attempts(c, endpoint)
		if err != nil {
			aiFailure(c, err)
			return
		}
		enabled = r.WritesEnabled
	}
	writeJSON(c.Writer, 200, map[string]any{"endpoint": ep, "rolling": rolling, "syncedAt": s.now(), "writesEnabled": enabled, "commands": commands, "attempts": attempts, "priceState": priceState})
}
