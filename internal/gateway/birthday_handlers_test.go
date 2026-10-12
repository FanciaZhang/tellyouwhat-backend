package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/tellyouwhat/backend/internal/birthday"
	"github.com/tellyouwhat/backend/internal/platform/appregistry"
	"github.com/tellyouwhat/backend/internal/usage"
	"net/http/httptest"
	"testing"
)

type birthdayFixture struct{ calls int }

func (f *birthdayFixture) Assist(_ context.Context, _ birthday.Request) (birthday.Result, error) {
	f.calls++
	return birthday.Result{InputTokens: 1, OutputTokens: 1, Value: birthday.ModelResult{Kind: "proposals", Questions: []birthday.Question{}, Changes: []string{}, Proposals: []birthday.Proposal{{ID: "p1", Title: "打电话", Reason: "先表达祝福", Actions: []birthday.Action{{ID: "a1", Kind: "call", Title: "打电话", Detail: "确认方便时间"}}, Preparations: []birthday.Preparation{}, EvidenceIDs: []string{}, BudgetNote: "未确定", Tradeoff: "可能需要改时间", Alternative: "文字祝福"}}}}, nil
}
func TestBirthdayRouteRejectsOtherAppIdentityBeforeModelCall(t *testing.T) {
	model := &birthdayFixture{}
	app := appregistry.App{ID: appregistry.Birthday, AllowedOperations: []string{"birthday.propose"}}
	input := birthday.Request{ContractVersion: 2, PromptVersion: 1, RequestID: "19be2f9e-bd92-4699-b561-e3816092114c", SessionID: "b7a3d5fb-c14d-4b01-854e-232c8e89d066", Operation: "propose", CelebrationDate: "2026-12-20", TimeZone: "Asia/Shanghai", LeadTimeDays: 10, Currency: "CNY", Answers: map[string]string{}}
	body, _ := json.Marshal(input)
	for _, test := range []struct {
		appID  string
		status int
		suffix string
	}{{"journal", 401, ""}, {"birthday", 200, ""}, {"birthday", 422, "{}"}, {"birthday", 422, ","}} {
		s := New(Dependencies{App: app, Authenticator: fakeAuthenticator{appID: test.appID}, Quota: fakeQuota{}, Usage: usage.NewMemoryRecorder(), BirthdayAssistant: model})
		request := httptest.NewRequest("POST", "/v2/birthday/assist", bytes.NewReader(append(append([]byte{}, body...), test.suffix...)))
		for key, value := range map[string]string{"X-Tellyouwhat-Request-ID": input.RequestID, "X-Tellyouwhat-Key-ID": "synthetic-key", "X-Tellyouwhat-Assertion": "proof", "X-Tellyouwhat-Nonce": "nonce"} {
			request.Header.Set(key, value)
		}
		response := httptest.NewRecorder()
		s.Router().ServeHTTP(response, request)
		if response.Code != test.status {
			t.Fatalf("%s: %d %s", test.appID, response.Code, response.Body.String())
		}
		if test.status == 200 {
			var output birthday.Response
			_ = json.Unmarshal(response.Body.Bytes(), &output)
			if output.Source != "model" || output.RequestID != input.RequestID {
				t.Fatal("binding/source missing")
			}
		}
	}
	if model.calls != 1 {
		t.Fatal("wrong app reached provider")
	}
}
