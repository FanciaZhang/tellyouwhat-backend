package adminportal

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tellyouwhat/backend/internal/adminhttpapi"
	"github.com/tellyouwhat/backend/internal/aiconfig"
	"github.com/tellyouwhat/backend/internal/arkcontrol"
	"github.com/tellyouwhat/backend/internal/contracts"
)

type aiPreviewClaim struct {
	Actor       string                    `json:"actor"`
	Publication aiconfig.Publication      `json:"publication"`
	Policy      contracts.ExecutionPolicy `json:"policy"`
	Endpoint    arkcontrol.Endpoint       `json:"endpoint"`
	ExpiresAt   int64                     `json:"expiresAt"`
}
type aiPreview struct {
	Revision     *aiconfig.Revision         `json:"revision"`
	Before       *contracts.ExecutionPolicy `json:"before"`
	After        contracts.ExecutionPolicy  `json:"after"`
	Endpoint     arkcontrol.Endpoint        `json:"endpoint"`
	CanPublish   bool                       `json:"canPublish"`
	PreviewToken string                     `json:"previewToken,omitempty"`
	ExpiresAt    *time.Time                 `json:"expiresAt,omitempty"`
	claim        aiPreviewClaim
}

func (s *Server) GetHealthAIHistory(c *gin.Context, operation string, params adminhttpapi.GetHealthAIHistoryParams) {
	if _, ok := s.aiAccess(c, false); !ok {
		return
	}
	op := contracts.Operation(operation)
	if _, ok := contracts.PolicyFor(op); !ok {
		aiFailure(c, aiconfig.ErrInvalid)
		return
	}
	cursor := ""
	if params.Cursor != nil {
		cursor = *params.Cursor
	}
	result, err := s.config.AI.Store.HistoryPage(c, op, cursor)
	if err != nil {
		aiFailure(c, err)
		return
	}
	writeJSON(c.Writer, 200, result)
}
func (s *Server) GetHealthAIRevision(c *gin.Context, operation, revision string) {
	auth, ok := s.aiAccess(c, false)
	if !ok {
		return
	}
	r, err := s.config.AI.Store.Get(c, contracts.Operation(operation), revision)
	if err != nil {
		aiFailure(c, err)
		return
	}
	preview, err := s.aiPreview(c, auth.User.ID, r)
	if err != nil {
		aiFailure(c, err)
		return
	}
	writeJSON(c.Writer, 200, preview)
}
func (s *Server) aiPreview(ctx context.Context, actor string, r *aiconfig.Revision) (aiPreview, error) {
	out := aiPreview{Revision: r, After: r.Policy}
	current, err := s.config.AI.Store.Current(ctx, r.Operation)
	if err != nil {
		return out, err
	}
	base := ""
	if current != nil {
		out.Before = &current.Policy
		base = current.ID
	}
	out.Endpoint, err = s.config.AI.Inventory.Endpoint(ctx, r.Policy.Endpoint)
	if err != nil {
		return out, err
	}
	out.CanPublish = r.PublishedAt == nil && r.BaseVersion == base && s.config.AI.WritesEnabled
	expires := s.now().Add(5 * time.Minute)
	out.claim = aiPreviewClaim{Actor: actor, Publication: aiconfig.Publication{Operation: r.Operation, Revision: r.ID, BaseVersion: base}, Policy: r.Policy, Endpoint: out.Endpoint, ExpiresAt: expires.Unix()}
	if out.CanPublish {
		if len(s.config.PreviewSigningKey) < 32 {
			return out, aiconfig.ErrInvalid
		}
		raw, _ := json.Marshal(out.claim)
		mac := hmac.New(sha256.New, s.config.PreviewSigningKey)
		mac.Write(raw)
		out.PreviewToken = base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
		out.ExpiresAt = &expires
	}
	return out, nil
}
func (s *Server) verifyAIPreview(expected aiPreviewClaim, token string) bool {
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
	var claim aiPreviewClaim
	if json.Unmarshal(raw, &claim) != nil || s.now().Unix() >= claim.ExpiresAt {
		return false
	}
	expected.ExpiresAt = claim.ExpiresAt
	a, _ := json.Marshal(expected)
	b, _ := json.Marshal(claim)
	return hmac.Equal(a, b)
}
