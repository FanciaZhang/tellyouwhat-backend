package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/tellyouwhat/backend/internal/birthday"
	"github.com/tellyouwhat/backend/internal/birthdayhttpapi"
	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/quota"
	"github.com/tellyouwhat/backend/internal/usage"
)

// Birthday is metered without a StoreKit purchase in this first product slice.
// App Attest, App host isolation, per-device/IP quotas, and the project cost gate remain mandatory.
func (s *Server) AssistBirthday(ctx context.Context, request birthdayhttpapi.AssistBirthdayRequestObject) (birthdayhttpapi.AssistBirthdayResponseObject, error) {
	id := request.Params.XTellyouwhatRequestID
	fail := func(status int, code string) (birthdayhttpapi.AssistBirthdayResponseObject, error) {
		return birthdayhttpapi.AssistBirthdaydefaultJSONResponse{StatusCode: status, Body: birthdayhttpapi.ErrorResponse{Error: birthdayhttpapi.ErrorDetail{Code: code, Message: code, RequestID: id.String()}}}, nil
	}
	principal, failure := s.apiAuthenticate(ctx, id)
	if failure != nil {
		return fail(failure.status, failure.code)
	}
	if s.birthdayAssistant == nil || s.quota == nil || s.usage == nil {
		return fail(503, "not_ready")
	}
	if request.Body == nil || !json.Valid(rawRequestBody(strictGinContext(ctx))) || request.Body.Validate() != nil {
		return fail(422, "contract_violation")
	}
	input := *request.Body
	if !strings.EqualFold(input.RequestID, id.String()) || !s.app.AllowsOperation("birthday."+input.Operation) {
		return fail(422, "contract_violation")
	}
	estimated := birthday.ReservationTokens(input)
	lease, err := s.quota.Acquire(ctx, quota.Identity{DeviceID: principal.DeviceID, TransactionID: principal.KeyID, IP: s.ipResolver(strictGinContext(ctx).Request)}, contracts.Operation("birthday."+input.Operation), estimated, "", s.now())
	if err != nil {
		if errors.Is(err, quota.ErrExceeded) {
			return fail(429, "quota_exceeded")
		} else {
			return fail(503, "quota_unavailable")
		}
	}
	actual := estimated
	defer func() { lease.Release(actual) }()
	result, err := s.birthdayAssistant.Assist(ctx, input)
	if result.InputTokens >= 0 && result.OutputTokens >= 0 && result.InputTokens+result.OutputTokens > 0 {
		actual = result.InputTokens + result.OutputTokens
	}
	if err != nil {
		switch {
		case errors.Is(err, birthday.ErrInvalid):
			return fail(502, "invalid_model_result")
		case errors.Is(err, costcontrol.ErrBudgetExceeded), errors.Is(err, costcontrol.ErrConcurrencyExceeded):
			return fail(429, "cost_limit")
		default:
			return fail(http.StatusBadGateway, "upstream_error")
		}
	}
	if result.Value.Validate(input) != nil {
		return fail(502, "invalid_model_result")
	}
	if s.usage.Record(ctx, usage.Record{RequestID: id.String(), KeyID: principal.KeyID, DeviceID: principal.DeviceID, TransactionID: principal.KeyID, Operation: contracts.Operation("birthday." + input.Operation), InputTokens: result.InputTokens, OutputTokens: result.OutputTokens, OccurredAt: s.now()}) != nil {
		actual = estimated
	}
	return birthdayhttpapi.AssistBirthday200JSONResponse(birthday.Response{ContractVersion: 2, RequestID: input.RequestID, DraftRevision: input.DraftRevision, Source: "model", ModelResult: result.Value}), nil
}
