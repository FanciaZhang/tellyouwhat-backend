package adminportal

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/adminauth"
	"github.com/tellyouwhat/backend/internal/adminhttpapi"
	"github.com/tellyouwhat/backend/internal/promptconfig"
)

// Debug access never grants prompt publication or account-wide AI management.
func (s *Server) voiceDebugAccess(c *gin.Context, write bool) (adminauth.Authenticated, bool) {
	a, ok := s.auth.RequirePermission(c.Writer, c.Request, adminauth.PermissionVoiceDebug, "journal", write, false)
	if !ok {
		return a, false
	}
	if s.config.Prompts == nil {
		writeFailure(c.Writer, 503, "prompts_unavailable", "语音调试尚未配置")
		return a, false
	}
	if write && !s.promptWritesEnabled("journal") {
		writeFailure(c.Writer, 503, "writes_disabled", "此环境未启用语音调试")
		return a, false
	}
	c.Set("voice-debug-auth", a)
	return a, true
}
func voiceDebugOwner(c *gin.Context) string {
	value, ok := c.Get("voice-debug-auth")
	if !ok {
		return ""
	}
	auth := value.(adminauth.Authenticated)
	if auth.User.Role == adminauth.RoleAdmin {
		return ""
	}
	return auth.User.ID
}
func (s *Server) GetVoiceDebugConfig(c *gin.Context) {
	if _, ok := s.voiceDebugAccess(c, false); !ok {
		return
	}
	s.promptConfigView(c, "journal")
}
func (s *Server) CreateVoiceDebugDraft(c *gin.Context, _ adminhttpapi.CreateVoiceDebugDraftParams) {
	auth, ok := s.voiceDebugAccess(c, true)
	if !ok {
		return
	}
	s.createPromptDraft(c, auth, true)
}

func (s *Server) voiceDebugModelsAllowed(c *gin.Context, policy promptconfig.Policy) bool {
	if policy.Journal == nil {
		return false
	}
	current, err := s.config.Prompts.Current(c.Request.Context(), "journal")
	if err != nil {
		return false
	}
	allowed := map[string]bool{}
	for id := range s.config.ConfiguredPromptModels {
		allowed[id] = true
	}
	for _, p := range []promptconfig.Parameters{current.Policy.Journal.Organize.Lite, current.Policy.Journal.Organize.Pro, current.Policy.Journal.Voice.Parameters} {
		allowed[p.Model] = true
	}
	for _, p := range []promptconfig.Parameters{policy.Journal.Organize.Lite, policy.Journal.Organize.Pro, policy.Journal.Voice.Parameters} {
		if !allowed[p.Model] {
			return false
		}
	}
	return true
}

func (s *Server) ReconcileVoiceRecording(c *gin.Context, run uuid.UUID, item int, _ adminhttpapi.ReconcileVoiceRecordingParams) {
	if _, ok := s.evaluationAccess(c, true); !ok {
		return
	}
	if err := s.config.Evaluations.ReconcileRecording(c.Request.Context(), run.String(), item, voiceDebugOwner(c), s.now()); err != nil {
		evaluationFailure(c, err)
		return
	}
	writeJSON(c.Writer, 200, map[string]bool{"queued": true})
}
