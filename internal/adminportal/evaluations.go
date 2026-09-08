package adminportal

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/adminauth"
	"github.com/tellyouwhat/backend/internal/adminhttpapi"
	"github.com/tellyouwhat/backend/internal/promptconfig"
	"github.com/tellyouwhat/backend/internal/prompteval"
	"strings"
	"time"
)

type evaluationInput struct {
	SampleIDs  []string `json:"sampleIDs"`
	Candidates []struct {
		Revision      string                   `json:"revision"`
		Label         string                   `json:"label"`
		ModelOverride *promptconfig.Parameters `json:"modelOverride,omitempty"`
	} `json:"candidates"`
}

func (s *Server) evaluationAccess(c *gin.Context, write bool) (adminauth.Authenticated, bool) {
	a, ok := s.promptsAccess(c, write, false)
	if !ok {
		return a, false
	}
	if s.config.Evaluations == nil {
		writeFailure(c.Writer, 503, "evaluations_unavailable", "效果评测尚未配置")
		return a, false
	}
	return a, true
}
func evaluationFailure(c *gin.Context, err error) {
	switch {
	case errors.Is(err, prompteval.ErrInvalid), errors.Is(err, promptconfig.ErrInvalid):
		writeFailure(c.Writer, 422, "evaluation_invalid", "请检查样例、版本与模型参数")
	case errors.Is(err, prompteval.ErrNotFound), errors.Is(err, promptconfig.ErrNotFound):
		writeFailure(c.Writer, 404, "evaluation_not_found", "记录不存在或已超过七天保留期")
	case errors.Is(err, prompteval.ErrConflict):
		writeFailure(c.Writer, 409, "evaluation_conflict", "提交标识已用于其他内容，请重新预览")
	default:
		writeFailure(c.Writer, 503, "evaluation_unavailable", "评测暂时无法执行，请检查项目预算或稍后重试")
	}
}
func builtinSample(id string) (prompteval.Sample, bool) {
	for _, v := range prompteval.Builtins() {
		if v.ID == id {
			return v, true
		}
	}
	return prompteval.Sample{}, false
}
func (s *Server) evaluationSample(c *gin.Context, id string) (prompteval.Sample, error) {
	if v, ok := builtinSample(id); ok {
		return v, nil
	}
	return s.config.Evaluations.Sample(c, id, s.now())
}
func (s *Server) ListEvaluationSamples(c *gin.Context) {
	if _, ok := s.evaluationAccess(c, false); !ok {
		return
	}
	items, err := s.config.Evaluations.Samples(c, s.now())
	if err != nil {
		evaluationFailure(c, err)
		return
	}
	builtins := []prompteval.Sample{}
	for _, v := range prompteval.Builtins() {
		builtins = append(builtins, prompteval.Sample{ID: v.ID, Name: v.Name, Kind: v.Kind})
	}
	writeJSON(c.Writer, 200, map[string]any{"builtins": builtins, "samples": items, "retentionDays": 7})
}
func (s *Server) GetEvaluationSample(c *gin.Context, id uuid.UUID) {
	if _, ok := s.evaluationAccess(c, false); !ok {
		return
	}
	v, err := s.evaluationSample(c, id.String())
	if err != nil {
		evaluationFailure(c, err)
		return
	}
	writeJSON(c.Writer, 200, v)
}
func (s *Server) SaveEvaluationSample(c *gin.Context, _ adminhttpapi.SaveEvaluationSampleParams) {
	a, ok := s.evaluationAccess(c, true)
	if !ok {
		return
	}
	var v prompteval.Sample
	if !operationsBody(c, &v) {
		return
	}
	if _, ok := builtinSample(v.ID); ok {
		evaluationFailure(c, prompteval.ErrInvalid)
		return
	}
	if err := s.config.Evaluations.SaveSample(c, v, a.User.ID, s.now()); err != nil {
		evaluationFailure(c, err)
		return
	}
	writeJSON(c.Writer, 200, v)
}
func (s *Server) DeleteEvaluationSample(c *gin.Context, id uuid.UUID, _ adminhttpapi.DeleteEvaluationSampleParams) {
	if _, ok := s.evaluationAccess(c, true); !ok {
		return
	}
	if _, ok := builtinSample(id.String()); ok {
		evaluationFailure(c, prompteval.ErrInvalid)
		return
	}
	if err := s.config.Evaluations.DeleteSample(c, id.String()); err != nil {
		evaluationFailure(c, err)
		return
	}
	writeJSON(c.Writer, 200, map[string]bool{"deleted": true})
}
func (s *Server) evaluationPlan(c *gin.Context, in evaluationInput) (prompteval.Plan, error) {
	var empty prompteval.Plan
	if len(in.SampleIDs) < 1 || len(in.SampleIDs) > 20 || len(in.Candidates) < 1 || len(in.Candidates) > 2 {
		return empty, prompteval.ErrInvalid
	}
	samples := []prompteval.Sample{}
	for _, id := range in.SampleIDs {
		v, err := s.evaluationSample(c, id)
		if err != nil {
			return empty, err
		}
		samples = append(samples, v)
	}
	candidates := []prompteval.Candidate{}
	for _, v := range in.Candidates {
		r, err := s.config.Prompts.Get(c, v.Revision)
		if err != nil {
			return empty, err
		}
		if r.Scope != "journal" || len(v.Label) > 100 {
			return empty, prompteval.ErrInvalid
		}
		source := ""
		if v.ModelOverride != nil {
			if v.ModelOverride.Validate() != nil {
				return empty, prompteval.ErrInvalid
			}
			source = r.ID
			r.Policy.Journal.Organize.Lite = *v.ModelOverride
			r.Policy.Journal.Organize.Pro = *v.ModelOverride
			r.Policy.Journal.Voice.Parameters = *v.ModelOverride
		}
		if err := s.resolvePromptModels(c, &r.Policy); err != nil {
			return empty, err
		}
		if source != "" {
			r.ID = "comparison:" + evaluationDigest(struct {
				ID     string
				Policy promptconfig.Policy
			}{source, r.Policy})
		}
		candidates = append(candidates, prompteval.Candidate{Revision: r, Label: v.Label, SourceRevision: source})
	}
	current, err := s.config.Prompts.Current(c, "journal")
	if err != nil {
		return empty, err
	}
	if err = s.resolvePromptModels(c, &current.Policy); err != nil {
		return empty, err
	}
	judge := current.Policy.Journal.Organize.Pro
	if judge.MaxOutputTokens > 4096 {
		judge.MaxOutputTokens = 4096
	}
	return prompteval.Prepare(samples, candidates, judge, s.config.EvaluationSpeechPrice)
}
func evaluationDigest(v any) string {
	raw, _ := json.Marshal(v)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

type evaluationClaim struct {
	Actor   string `json:"actor"`
	Digest  string `json:"digest"`
	Expires int64  `json:"expires"`
}

func (s *Server) signEvaluation(actor string, p prompteval.Plan) string {
	raw, _ := json.Marshal(evaluationClaim{actor, evaluationDigest(p), s.now().Add(5 * time.Minute).Unix()})
	mac := hmac.New(sha256.New, s.config.PreviewSigningKey)
	mac.Write(raw)
	return base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
func (s *Server) verifyEvaluation(token, actor string, p prompteval.Plan) bool {
	if len(token) > 2048 || len(s.config.PreviewSigningKey) < 32 {
		return false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return false
	}
	raw, e := base64.RawURLEncoding.DecodeString(parts[0])
	if e != nil {
		return false
	}
	sig, e := base64.RawURLEncoding.DecodeString(parts[1])
	if e != nil {
		return false
	}
	mac := hmac.New(sha256.New, s.config.PreviewSigningKey)
	mac.Write(raw)
	var claim evaluationClaim
	return hmac.Equal(sig, mac.Sum(nil)) && json.Unmarshal(raw, &claim) == nil && claim.Actor == actor && claim.Digest == evaluationDigest(p) && claim.Expires > s.now().Unix()
}
func (s *Server) PreviewEvaluation(c *gin.Context, _ adminhttpapi.PreviewEvaluationParams) {
	a, ok := s.evaluationAccess(c, true)
	if !ok {
		return
	}
	var in evaluationInput
	if !operationsBody(c, &in) {
		return
	}
	p, err := s.evaluationPlan(c, in)
	if err != nil {
		evaluationFailure(c, err)
		return
	}
	token := s.signEvaluation(a.User.ID, p)
	for i := range p.Samples {
		p.Samples[i].Audio = nil
	}
	writeJSON(c.Writer, 200, map[string]any{"plan": p, "previewToken": token})
}
func (s *Server) StartEvaluation(c *gin.Context, _ adminhttpapi.StartEvaluationParams) {
	a, ok := s.evaluationAccess(c, true)
	if !ok {
		return
	}
	var in struct {
		evaluationInput
		PreviewToken string `json:"previewToken"`
	}
	if !operationsBody(c, &in) {
		return
	}
	m, ok := aiMutation(c, a.User.ID)
	if !ok {
		return
	}
	if replay, err := s.config.Evaluations.Replay(c, m, in.evaluationInput, s.now()); err != nil {
		evaluationFailure(c, err)
		return
	} else if replay != nil {
		writeJSON(c.Writer, 200, evaluationRunView(*replay))
		return
	}
	p, err := s.evaluationPlan(c, in.evaluationInput)
	if err != nil {
		evaluationFailure(c, err)
		return
	}
	if !s.verifyEvaluation(in.PreviewToken, a.User.ID, p) {
		writeFailure(c.Writer, 409, "evaluation_preview_changed", "预览已过期、价格或样例已变化，请重新预览")
		return
	}
	r, err := s.config.Evaluations.StartInput(c, p, m, in.evaluationInput, s.now())
	if err != nil {
		evaluationFailure(c, err)
		return
	}
	writeJSON(c.Writer, 200, evaluationRunView(r))
}
func evaluationRunView(r prompteval.Run) prompteval.Run {
	r.Plan.Requests = nil
	for i := range r.Plan.Samples {
		v := r.Plan.Samples[i]
		r.Plan.Samples[i] = prompteval.Sample{ID: v.ID, Name: v.Name, Kind: v.Kind}
	}
	return r
}
func (s *Server) ListEvaluationRuns(c *gin.Context) {
	if _, ok := s.evaluationAccess(c, false); !ok {
		return
	}
	v, err := s.config.Evaluations.List(c, s.now())
	if err != nil {
		evaluationFailure(c, err)
		return
	}
	writeJSON(c.Writer, 200, map[string]any{"runs": v})
}
func (s *Server) GetEvaluationRun(c *gin.Context, id uuid.UUID) {
	if _, ok := s.evaluationAccess(c, false); !ok {
		return
	}
	v, err := s.config.Evaluations.Get(c, id.String(), s.now())
	if err != nil {
		evaluationFailure(c, err)
		return
	}
	writeJSON(c.Writer, 200, evaluationRunView(v))
}
func (s *Server) GetEvaluationItem(c *gin.Context, id uuid.UUID, item int) {
	if _, ok := s.evaluationAccess(c, false); !ok {
		return
	}
	v, err := s.config.Evaluations.Result(c, id.String(), item, s.now())
	if err != nil {
		evaluationFailure(c, err)
		return
	}
	writeJSON(c.Writer, 200, v)
}
func (s *Server) CancelEvaluation(c *gin.Context, id uuid.UUID, _ adminhttpapi.CancelEvaluationParams) {
	if _, ok := s.evaluationAccess(c, true); !ok {
		return
	}
	err := s.config.Evaluations.Cancel(c, id.String(), s.now())
	if err != nil {
		evaluationFailure(c, err)
		return
	}
	writeJSON(c.Writer, 200, map[string]bool{"cancelled": true})
}
