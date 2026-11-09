package adminportal

import (
	"context"
	"errors"
	"github.com/gin-gonic/gin"
	"github.com/tellyouwhat/backend/internal/adminauth"
	"github.com/tellyouwhat/backend/internal/adminhttpapi"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/journal/readaloud"
)

type VoiceDiscovery interface {
	Candidates(context.Context) (readaloud.DiscoveredVoices, error)
}

func (s *Server) voiceCatalogAccess(c *gin.Context, write, reauth bool) (adminauth.Authenticated, bool) {
	auth, ok := s.auth.RequirePermission(c.Writer, c.Request, adminauth.PermissionAIConfigManage, "journal", write, reauth)
	if !ok {
		return auth, false
	}
	if s.config.VoiceCatalog == nil {
		writeFailure(c.Writer, 503, "voice_catalog_unavailable", "音色目录尚未配置")
		return auth, false
	}
	if write && !s.config.VoiceWritesEnabled {
		writeFailure(c.Writer, 503, "writes_disabled", "管理写操作尚未启用")
		return auth, false
	}
	return auth, true
}
func voiceCatalogFailure(c *gin.Context, err error) {
	switch {
	case errors.Is(err, readaloud.ErrCatalog), errors.Is(err, readaloud.ErrInput):
		writeFailure(c.Writer, 422, "voice_catalog_invalid", "请核对音色、费用类别、默认声音及已验证能力")
	case errors.Is(err, readaloud.ErrCatalogNotFound):
		writeFailure(c.Writer, 404, "voice_catalog_not_found", "找不到这个音色目录版本")
	case errors.Is(err, readaloud.ErrCatalogConflict), errors.Is(err, readaloud.ErrRequestConflict):
		writeFailure(c.Writer, 409, "voice_catalog_conflict", "配置已变化或验证请求已经开始，请刷新检查原结果；重新验证需要主动再次提交")
	case errors.Is(err, readaloud.ErrQuotaExceeded):
		writeFailure(c.Writer, 429, "voice_probe_quota_exceeded", "本日音色验证字符额度已用尽")
	case errors.Is(err, costcontrol.ErrBudgetExceeded), errors.Is(err, costcontrol.ErrProtectionActive):
		writeFailure(c.Writer, 503, "voice_budget_protected", "项目费用保护已生效，未提交新的合成")
	case errors.Is(err, costcontrol.ErrConcurrencyExceeded):
		writeFailure(c.Writer, 429, "voice_service_busy", "服务正在处理其他请求，请稍后主动重试")
	default:
		writeFailure(c.Writer, 503, "voice_catalog_unavailable", "音色服务暂不可用，已发布目录保持不变")
	}
}
func (s *Server) GetVoiceCatalog(c *gin.Context) {
	if _, ok := s.voiceCatalogAccess(c, false, false); !ok {
		return
	}
	current, err := s.config.VoiceCatalog.Current(c.Request.Context())
	var value any = current
	if errors.Is(err, readaloud.ErrCatalogNotFound) {
		value = nil
	} else if err != nil {
		voiceCatalogFailure(c, err)
		return
	}
	writeJSON(c.Writer, 200, map[string]any{"current": value, "writesEnabled": s.config.VoiceWritesEnabled, "syncEnabled": s.config.VoiceDiscovery != nil, "probesEnabled": s.config.VoiceProbe != nil})
}
func (s *Server) ListVoiceCatalogHistory(c *gin.Context, params adminhttpapi.ListVoiceCatalogHistoryParams) {
	if _, ok := s.voiceCatalogAccess(c, false, false); !ok {
		return
	}
	cursor := ""
	if params.Cursor != nil {
		cursor = *params.Cursor
	}
	history, err := s.config.VoiceCatalog.History(c.Request.Context(), cursor)
	if err != nil {
		voiceCatalogFailure(c, err)
		return
	}
	writeJSON(c.Writer, 200, history)
}
func (s *Server) GetVoiceCatalogRevision(c *gin.Context, id string) {
	if _, ok := s.voiceCatalogAccess(c, false, false); !ok {
		return
	}
	r, err := s.config.VoiceCatalog.Get(c.Request.Context(), id)
	if err != nil {
		voiceCatalogFailure(c, err)
		return
	}
	writeJSON(c.Writer, 200, r)
}
func (s *Server) CreateVoiceCatalogDraft(c *gin.Context, _ adminhttpapi.CreateVoiceCatalogDraftParams) {
	auth, ok := s.voiceCatalogAccess(c, true, false)
	if !ok {
		return
	}
	var input readaloud.CatalogDraft
	if !operationsBody(c, &input) {
		return
	}
	m, ok := aiMutation(c, auth.User.ID)
	if !ok {
		return
	}
	r, err := s.config.VoiceCatalog.Draft(c.Request.Context(), input, m, s.now())
	if err != nil {
		voiceCatalogFailure(c, err)
		return
	}
	writeJSON(c.Writer, 200, r)
}
func (s *Server) PublishVoiceCatalog(c *gin.Context, _ adminhttpapi.PublishVoiceCatalogParams) {
	auth, ok := s.voiceCatalogAccess(c, true, true)
	if !ok {
		return
	}
	var input readaloud.CatalogPublication
	if !operationsBody(c, &input) {
		return
	}
	m, ok := aiMutation(c, auth.User.ID)
	if !ok {
		return
	}
	r, err := s.config.VoiceCatalog.Publish(c.Request.Context(), input, m, s.now())
	if err != nil {
		voiceCatalogFailure(c, err)
		return
	}
	writeJSON(c.Writer, 200, r)
}
func (s *Server) SyncVoiceCatalog(c *gin.Context, _ adminhttpapi.SyncVoiceCatalogParams) {
	if _, ok := s.voiceCatalogAccess(c, true, false); !ok {
		return
	}
	if s.config.VoiceDiscovery == nil {
		voiceCatalogFailure(c, readaloud.ErrDiscovery)
		return
	}
	inventory, err := s.config.VoiceDiscovery.Candidates(c.Request.Context())
	if err != nil {
		voiceCatalogFailure(c, err)
		return
	}
	writeJSON(c.Writer, 200, inventory)
}
func (s *Server) ProbeVoiceCatalogVoice(c *gin.Context, _ adminhttpapi.ProbeVoiceCatalogVoiceParams) {
	auth, ok := s.voiceCatalogAccess(c, true, true)
	if !ok {
		return
	}
	if s.config.VoiceProbe == nil {
		voiceCatalogFailure(c, readaloud.ErrProvider)
		return
	}
	var input readaloud.VoiceConfiguration
	if !operationsBody(c, &input) {
		return
	}
	m, ok := aiMutation(c, auth.User.ID)
	if !ok {
		return
	}
	r, err := s.config.VoiceProbe.Verify(c.Request.Context(), input, m)
	if err != nil {
		voiceCatalogFailure(c, err)
		return
	}
	writeJSON(c.Writer, 200, r)
}
