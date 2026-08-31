package adminportal

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/tellyouwhat/backend/internal/adminauth"
	"github.com/tellyouwhat/backend/internal/adminui"
	"github.com/tellyouwhat/backend/internal/appstoreconnect"
)

const maximumBodyBytes = 64 << 10

var (
	validDurations     = []string{"THREE_DAYS", "ONE_WEEK", "TWO_WEEKS", "ONE_MONTH", "TWO_MONTHS", "THREE_MONTHS", "SIX_MONTHS", "ONE_YEAR"}
	validCustomers     = []string{"NEW", "EXISTING", "EXPIRED"}
	idempotencyPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,128}$`)
	customCodePattern  = regexp.MustCompile(`^[A-Z0-9]{4,64}$`)
	resourceIDPattern  = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
)

type OfferManager interface {
	ListOffers(context.Context) ([]appstoreconnect.Offer, error)
	CreateFreeOffer(context.Context, appstoreconnect.OfferDraft) (appstoreconnect.Offer, error)
	DeactivateOffer(context.Context, string) (appstoreconnect.Offer, error)
	CreateCustomCode(context.Context, string, string, int, string) (appstoreconnect.CodePool, error)
	CreateOneTimeCodeBatch(context.Context, string, int, string, string) (appstoreconnect.CodePool, error)
	ListCodePools(context.Context, string) ([]appstoreconnect.CodePool, error)
	DownloadOneTimeCodes(context.Context, string) ([]byte, error)
}

type Config struct {
	PreviewSigningKey []byte
	WritesEnabled     bool
}

type Server struct {
	mux        *http.ServeMux
	auth       *adminauth.Service
	offers     OfferManager
	operations OperationStore
	metrics    MetricsReader
	now        func() time.Time
	config     Config
	limiter    *rateLimiter
}

func NewServer(auth *adminauth.Service, offers OfferManager, operations OperationStore, metrics MetricsReader, config Config, now func() time.Time) (*Server, error) {
	if auth == nil || offers == nil || operations == nil || metrics == nil || len(config.PreviewSigningKey) < 32 {
		return nil, errors.New("admin portal dependencies and a 32-byte preview key are required")
	}
	if now == nil {
		now = time.Now
	}
	server := &Server{mux: http.NewServeMux(), auth: auth, offers: offers, operations: operations, metrics: metrics, now: now, config: config, limiter: newRateLimiter(now)}
	server.mux.HandleFunc("GET /healthz", server.health)
	server.mux.HandleFunc("GET /readyz", server.health)
	server.mux.HandleFunc("GET /api/v1/offers", server.listOffers)
	server.mux.HandleFunc("GET /api/v1/metrics/offers", server.offerMetrics)
	server.mux.HandleFunc("POST /api/v1/offers/preview", server.previewOffer)
	server.mux.HandleFunc("POST /api/v1/offers", server.createOffer)
	server.mux.HandleFunc("POST /api/v1/offers/{offerID}/deactivate", server.deactivateOffer)
	server.mux.HandleFunc("POST /api/v1/offers/{offerID}/custom-codes", server.createCustomCode)
	server.mux.HandleFunc("POST /api/v1/offers/{offerID}/one-time-code-batches", server.createOneTimeBatch)
	server.mux.HandleFunc("GET /api/v1/offers/{offerID}/code-pools", server.listCodePools)
	server.mux.HandleFunc("POST /api/v1/one-time-code-batches/{batchID}/download", server.downloadOneTimeCodes)
	auth.RegisterRoutes(server.mux)
	server.mux.Handle("/", adminui.Handler())
	return server, nil
}

func (server *Server) listCodePools(writer http.ResponseWriter, request *http.Request) {
	if _, ok := server.auth.RequireAuthenticated(writer, request, false, false); !ok {
		return
	}
	offerID := cleanID(request.PathValue("offerID"))
	if offerID == "" {
		writeFailure(writer, http.StatusBadRequest, "invalid_offer", "Offer 标识无效")
		return
	}
	pools, err := server.offers.ListCodePools(request.Context(), offerID)
	if err != nil {
		writeAppleFailure(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"codePools": pools})
}

func (server *Server) downloadOneTimeCodes(writer http.ResponseWriter, request *http.Request) {
	if _, ok := server.auth.RequireAuthenticated(writer, request, true, true); !ok {
		return
	}
	batchID := cleanID(request.PathValue("batchID"))
	if batchID == "" {
		writeFailure(writer, http.StatusBadRequest, "invalid_code_pool", "一次性码池标识无效")
		return
	}
	data, err := server.offers.DownloadOneTimeCodes(request.Context(), batchID)
	if err != nil {
		writeAppleFailure(writer, err)
		return
	}
	writer.Header().Set("Content-Type", "text/csv; charset=utf-8")
	writer.Header().Set("Content-Disposition", `attachment; filename="offer-codes-`+batchID+`.csv"`)
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(data)
}

func (server *Server) offerMetrics(writer http.ResponseWriter, request *http.Request) {
	if _, ok := server.auth.RequireAuthenticated(writer, request, false, false); !ok {
		return
	}
	metrics, err := server.metrics.OfferMetrics(request.Context())
	if err != nil {
		writeFailure(writer, http.StatusServiceUnavailable, "metrics_unavailable", "兑换统计暂时不可用")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"metrics": metrics, "anonymous": true})
}

func (server *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.Header().Set("Referrer-Policy", "no-referrer")
	writer.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
	if !server.limiter.allow(request) {
		writer.Header().Set("Retry-After", "60")
		writeFailure(writer, http.StatusTooManyRequests, "rate_limited", "请求过于频繁，请稍后重试")
		return
	}
	server.mux.ServeHTTP(writer, request)
}

func (server *Server) health(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
}

func (server *Server) listOffers(writer http.ResponseWriter, request *http.Request) {
	if _, ok := server.auth.RequireAuthenticated(writer, request, false, false); !ok {
		return
	}
	offers, err := server.offers.ListOffers(request.Context())
	if err != nil {
		writeAppleFailure(writer, err)
		return
	}
	active := 0
	for _, offer := range offers {
		if offer.Active {
			active++
		}
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"offers": offers, "activeCount": active, "activeLimit": 10, "syncedAt": server.now().UTC(),
		"writesEnabled": server.config.WritesEnabled,
	})
}

func (server *Server) previewOffer(writer http.ResponseWriter, request *http.Request) {
	if _, ok := server.auth.RequireAuthenticated(writer, request, true, false); !ok {
		return
	}
	draft, ok := decodeOfferDraft(writer, request)
	if !ok {
		return
	}
	token, expiresAt, err := server.signPreview(draft)
	if err != nil {
		writeFailure(writer, http.StatusServiceUnavailable, "preview_unavailable", "暂时无法生成确认预览")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"draft": draft, "previewToken": token, "expiresAt": expiresAt,
		"summary":  previewSummary(draft),
		"warnings": []string{"Offer 创建后只能停用，名称、适用人群、时长和续订设置无法修改。"},
	})
}

func (server *Server) createOffer(writer http.ResponseWriter, request *http.Request) {
	session, ok := server.requireWrite(writer, request)
	if !ok {
		return
	}
	var input struct {
		Draft        appstoreconnect.OfferDraft `json:"draft"`
		PreviewToken string                     `json:"previewToken"`
	}
	if !decodeJSON(writer, request, &input) || !validateOfferDraft(writer, input.Draft) {
		return
	}
	input.Draft = normalizeOfferDraft(input.Draft)
	if !server.verifyPreview(input.Draft, input.PreviewToken) {
		writeFailure(writer, http.StatusConflict, "preview_expired", "确认预览已过期或内容已改变，请重新预览")
		return
	}
	existing, err := server.offers.ListOffers(request.Context())
	if err != nil {
		writeAppleFailure(writer, err)
		return
	}
	active := 0
	for _, offer := range existing {
		if offer.Active {
			active++
		}
	}
	if active >= 10 {
		writeFailure(writer, http.StatusConflict, "active_offer_limit", "启用中的 Offer 已达到 Apple 上限")
		return
	}
	if !server.beginOperation(writer, request, session.UserID, "offer.create", input) {
		return
	}
	offer, err := server.offers.CreateFreeOffer(request.Context(), input.Draft)
	if err != nil {
		writeAppleFailure(writer, err)
		return
	}
	server.completeOperation(writer, request, session.UserID, http.StatusCreated, map[string]any{"offer": offer})
}

func (server *Server) deactivateOffer(writer http.ResponseWriter, request *http.Request) {
	session, ok := server.requireWrite(writer, request)
	if !ok {
		return
	}
	offerID := cleanID(request.PathValue("offerID"))
	if offerID == "" {
		writeFailure(writer, http.StatusBadRequest, "invalid_offer", "Offer 标识无效")
		return
	}
	if !server.beginOperation(writer, request, session.UserID, "offer.deactivate", map[string]string{"offerID": offerID}) {
		return
	}
	offer, err := server.offers.DeactivateOffer(request.Context(), offerID)
	if err != nil {
		writeAppleFailure(writer, err)
		return
	}
	server.completeOperation(writer, request, session.UserID, http.StatusOK, map[string]any{"offer": offer})
}

func (server *Server) createCustomCode(writer http.ResponseWriter, request *http.Request) {
	session, ok := server.requireWrite(writer, request)
	if !ok {
		return
	}
	var input struct {
		Code           string `json:"code"`
		NumberOfCodes  int    `json:"numberOfCodes"`
		ExpirationDate string `json:"expirationDate"`
	}
	if !decodeJSON(writer, request, &input) {
		return
	}
	input.Code = strings.ToUpper(strings.TrimSpace(input.Code))
	offerID := cleanID(request.PathValue("offerID"))
	if offerID == "" || !customCodePattern.MatchString(input.Code) || input.NumberOfCodes < 1 || input.NumberOfCodes > 25000 || !validFutureDate(input.ExpirationDate, true, server.now()) {
		writeFailure(writer, http.StatusBadRequest, "invalid_code_pool", "自定义码池参数无效")
		return
	}
	operationInput := struct {
		OfferID string
		Input   any
	}{offerID, input}
	if !server.beginOperation(writer, request, session.UserID, "custom-code.create", operationInput) {
		return
	}
	pool, err := server.offers.CreateCustomCode(request.Context(), offerID, input.Code, input.NumberOfCodes, input.ExpirationDate)
	if err != nil {
		writeAppleFailure(writer, err)
		return
	}
	server.completeOperation(writer, request, session.UserID, http.StatusCreated, map[string]any{"codePool": pool})
}

func (server *Server) createOneTimeBatch(writer http.ResponseWriter, request *http.Request) {
	session, ok := server.requireWrite(writer, request)
	if !ok {
		return
	}
	var input struct {
		NumberOfCodes  int    `json:"numberOfCodes"`
		ExpirationDate string `json:"expirationDate"`
		Environment    string `json:"environment"`
	}
	if !decodeJSON(writer, request, &input) {
		return
	}
	input.Environment = strings.ToUpper(strings.TrimSpace(input.Environment))
	offerID := cleanID(request.PathValue("offerID"))
	if offerID == "" || input.NumberOfCodes < 1 || input.NumberOfCodes > 25000 ||
		!validFutureDate(input.ExpirationDate, false, server.now()) || input.Environment != "PRODUCTION" && input.Environment != "SANDBOX" {
		writeFailure(writer, http.StatusBadRequest, "invalid_code_pool", "一次性码池参数无效")
		return
	}
	operationInput := struct {
		OfferID string
		Input   any
	}{offerID, input}
	if !server.beginOperation(writer, request, session.UserID, "one-time-code-batch.create", operationInput) {
		return
	}
	pool, err := server.offers.CreateOneTimeCodeBatch(request.Context(), offerID, input.NumberOfCodes, input.ExpirationDate, input.Environment)
	if err != nil {
		writeAppleFailure(writer, err)
		return
	}
	server.completeOperation(writer, request, session.UserID, http.StatusCreated, map[string]any{"codePool": pool})
}

func (server *Server) beginOperation(writer http.ResponseWriter, request *http.Request, userID, action string, value any) bool {
	result, err := server.operations.Begin(request.Context(), userID, request.Header.Get("Idempotency-Key"), action, operationHash(value))
	if errors.Is(err, ErrOperationConflict) {
		writeFailure(writer, http.StatusConflict, "idempotency_conflict", "该防重复标识已经用于其他操作")
		return false
	}
	if errors.Is(err, ErrOperationPending) {
		writeFailure(writer, http.StatusConflict, "operation_pending", "操作正在处理或结果待核对，请刷新 Offer 列表")
		return false
	}
	if err != nil {
		writeFailure(writer, http.StatusServiceUnavailable, "operation_unavailable", "暂时无法安全提交操作")
		return false
	}
	if result != nil {
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("Idempotent-Replayed", "true")
		writer.WriteHeader(result.Status)
		_, _ = writer.Write(result.Body)
		return false
	}
	return true
}

func (server *Server) completeOperation(writer http.ResponseWriter, request *http.Request, userID string, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil || server.operations.Complete(request.Context(), userID, request.Header.Get("Idempotency-Key"), status, body, server.now()) != nil {
		writeFailure(writer, http.StatusServiceUnavailable, "operation_result_uncertain", "操作结果未能安全记录，请刷新 Offer 列表核对")
		return
	}
	writeJSON(writer, status, value)
}

func (server *Server) requireWrite(writer http.ResponseWriter, request *http.Request) (adminauth.Session, bool) {
	if !server.config.WritesEnabled {
		writeFailure(writer, http.StatusServiceUnavailable, "writes_disabled", "生产写操作尚未启用")
		return adminauth.Session{}, false
	}
	if !idempotencyPattern.MatchString(request.Header.Get("Idempotency-Key")) {
		writeFailure(writer, http.StatusBadRequest, "idempotency_key_required", "缺少有效的防重复提交标识")
		return adminauth.Session{}, false
	}
	return server.auth.RequireAuthenticated(writer, request, true, true)
}

type previewPayload struct {
	Draft     appstoreconnect.OfferDraft `json:"draft"`
	ExpiresAt int64                      `json:"expiresAt"`
}

func (server *Server) signPreview(draft appstoreconnect.OfferDraft) (string, time.Time, error) {
	expiresAt := server.now().UTC().Add(10 * time.Minute)
	payload, err := json.Marshal(previewPayload{Draft: draft, ExpiresAt: expiresAt.Unix()})
	if err != nil {
		return "", time.Time{}, err
	}
	signature := hmac.New(sha256.New, server.config.PreviewSigningKey)
	_, _ = signature.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(signature.Sum(nil)), expiresAt, nil
}

func (server *Server) verifyPreview(draft appstoreconnect.OfferDraft, token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return false
	}
	payload, payloadErr := base64.RawURLEncoding.DecodeString(parts[0])
	supplied, signatureErr := base64.RawURLEncoding.DecodeString(parts[1])
	if payloadErr != nil || signatureErr != nil {
		return false
	}
	mac := hmac.New(sha256.New, server.config.PreviewSigningKey)
	_, _ = mac.Write(payload)
	if !hmac.Equal(supplied, mac.Sum(nil)) {
		return false
	}
	var value previewPayload
	return json.Unmarshal(payload, &value) == nil && server.now().Unix() < value.ExpiresAt && equalDraft(value.Draft, draft)
}

func decodeOfferDraft(writer http.ResponseWriter, request *http.Request) (appstoreconnect.OfferDraft, bool) {
	var draft appstoreconnect.OfferDraft
	if !decodeJSON(writer, request, &draft) || !validateOfferDraft(writer, draft) {
		return appstoreconnect.OfferDraft{}, false
	}
	return normalizeOfferDraft(draft), true
}

func normalizeOfferDraft(draft appstoreconnect.OfferDraft) appstoreconnect.OfferDraft {
	draft.Name = strings.TrimSpace(draft.Name)
	draft.Duration = strings.TrimSpace(draft.Duration)
	slices.Sort(draft.CustomerEligibilities)
	return draft
}

func validateOfferDraft(writer http.ResponseWriter, draft appstoreconnect.OfferDraft) bool {
	if !draftIsValid(draft) {
		writeFailure(writer, http.StatusBadRequest, "invalid_offer", "Offer 名称、时长或适用人群无效")
		return false
	}
	return true
}

func draftIsValid(draft appstoreconnect.OfferDraft) bool {
	if len([]rune(strings.TrimSpace(draft.Name))) < 1 || len([]rune(strings.TrimSpace(draft.Name))) > 64 ||
		!slices.Contains(validDurations, draft.Duration) || len(draft.CustomerEligibilities) == 0 || len(draft.CustomerEligibilities) > 3 {
		return false
	}
	seen := map[string]bool{}
	for _, eligibility := range draft.CustomerEligibilities {
		if !slices.Contains(validCustomers, eligibility) || seen[eligibility] {
			return false
		}
		seen[eligibility] = true
	}
	return true
}

func equalDraft(left, right appstoreconnect.OfferDraft) bool {
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	return hmac.Equal(leftJSON, rightJSON)
}

func previewSummary(draft appstoreconnect.OfferDraft) map[string]any {
	return map[string]any{"price": "免费", "duration": draft.Duration, "periods": 1,
		"autoRenewEnabled": draft.AutoRenewEnabled, "customerEligibilities": draft.CustomerEligibilities,
		"offerEligibility": "REPLACE_INTRO_OFFERS", "targetSubscriptionPlanType": "MONTHLY"}
}

func validFutureDate(value string, optional bool, now time.Time) bool {
	if value == "" {
		return optional
	}
	date, err := time.Parse("2006-01-02", value)
	return err == nil && date.After(time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC))
}

func cleanID(value string) string {
	value = strings.TrimSpace(value)
	if !resourceIDPattern.MatchString(value) {
		return ""
	}
	return value
}

func decodeJSON(writer http.ResponseWriter, request *http.Request, destination any) bool {
	request.Body = http.MaxBytesReader(writer, request.Body, maximumBodyBytes)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		writeFailure(writer, http.StatusBadRequest, "invalid_request", "请求格式无效")
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeFailure(writer, http.StatusBadRequest, "invalid_request", "请求只能包含一个 JSON 对象")
		return false
	}
	return true
}

func writeAppleFailure(writer http.ResponseWriter, err error) {
	status, code, message := http.StatusBadGateway, "apple_unavailable", "App Store Connect 暂时不可用，请稍后核对状态"
	if errors.Is(err, appstoreconnect.ErrForbidden) {
		status, code, message = http.StatusFailedDependency, "apple_credentials_forbidden", "App Store Connect 专用密钥没有所需权限"
	}
	writeJSON(writer, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func writeFailure(writer http.ResponseWriter, status int, code, message string) {
	writeJSON(writer, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
