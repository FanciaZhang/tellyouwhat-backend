package airollout

import (
	"context"
	"errors"
	"github.com/tellyouwhat/backend/internal/aiconfig"
	"github.com/tellyouwhat/backend/internal/arkcontrol"
	"github.com/tellyouwhat/backend/internal/contracts"
	"sync"
	"testing"
	"time"
)

type configurationFixture map[contracts.Operation]contracts.ExecutionPolicy

func (f configurationFixture) Current(_ context.Context, op contracts.Operation) (*aiconfig.Revision, error) {
	p, ok := f[op]
	if !ok {
		return nil, nil
	}
	return &aiconfig.Revision{Policy: p}, nil
}

type probeFixture struct {
	mu    sync.Mutex
	calls []Requirement
	err   error
}

func (p *probeFixture) Probe(_ context.Context, _ string, op contracts.Operation, policy contracts.ExecutionPolicy) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, Requirement{op, policy.ReasoningEffort, policy.WebSearchEnabled})
	return p.err
}

type dynamicCloud struct {
	*cloudFixture
	version arkcontrol.Version
}

func (c dynamicCloud) Versions(context.Context, string) ([]arkcontrol.Version, error) {
	return []arkcontrol.Version{c.version}, nil
}
func TestCompatibilityUsesBoundFeaturesAndSupportsUnlistedModels(t *testing.T) {
	ctx := context.Background()
	model := arkcontrol.FoundationModel{Name: "future-text-model", Version: "future-version"}
	probe := &probeFixture{}
	configs := configurationFixture{contracts.OperationMealTextCapture: {Endpoint: "ep-text", ReasoningEffort: "high"}, contracts.OperationVoiceTranscription: {Endpoint: "ep-audio", ReasoningEffort: "minimal"}}
	service := &Service{Configurations: configs, Compatibility: &Compatibility{Probe: probe}, Endpoints: map[contracts.Operation]string{contracts.OperationMealTextCapture: "ep-old", contracts.OperationVoiceTranscription: "ep-audio"}, Cloud: dynamicCloud{newCloud("ep-text"), arkcontrol.Version{Name: model.Name, Version: model.Version, ModelID: "future-text-model-future-version", Status: "Published", Domains: []string{"LLM"}}}}
	required, err := service.Requirements(ctx, "ep-text")
	if err != nil || len(required) != 1 || required[0].Operation != contracts.OperationMealTextCapture {
		t.Fatal(required, err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		err = service.CheckModel(ctx, model, required)
		if !errors.Is(err, ErrChecking) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("check timed out")
		}
		time.Sleep(time.Millisecond)
	}
	if err != nil {
		t.Fatal("model absent from built-in certificates was rejected", err)
	}
	if err = service.CheckModel(ctx, model, required); err != nil {
		t.Fatal(err)
	}
	probe.mu.Lock()
	defer probe.mu.Unlock()
	if len(probe.calls) != 1 {
		t.Fatal("successful evidence not reused", probe.calls)
	}
}
func TestLegacyRequirementsRemainEndpointScoped(t *testing.T) {
	service := &Service{Endpoints: map[contracts.Operation]string{contracts.OperationMealTextCapture: "ep-text", contracts.OperationVoiceTranscription: "ep-audio", contracts.OperationMealDecision: "ep-search"}}
	req, err := service.Requirements(context.Background(), "ep-text")
	if err != nil || len(req) != 5 {
		t.Fatal(req, err)
	}
	for _, r := range req {
		if r.Operation != contracts.OperationMealTextCapture || r.Search {
			t.Fatal("unrelated capabilities required", r)
		}
	}
}
func TestTextPricingDoesNotRequireAudioCharge(t *testing.T) {
	p, err := Price(arkcontrol.Activation{State: "Available", Charges: []arkcontrol.ChargeItem{{Type: "InferencePrompt", Unit: "百万tokens", Price: "1.5"}, {Type: "InferenceCompletion", Unit: "千tokens", Price: "0.006"}}})
	if err != nil || p.InputNanosPerMillionTokens != 1500000000 || p.OutputNanosPerMillionTokens != 6000000000 {
		t.Fatal(p, err)
	}
}
func TestGenerationModelsRemainVisibleButCannotPassLanguageCheck(t *testing.T) {
	c := dynamicCloud{newCloud("ep-text"), arkcontrol.Version{Name: "media-generator", Version: "v1", ModelID: "media-v1", Domains: []string{"T2I"}}}
	s := Service{Cloud: c}
	err := s.CheckModel(context.Background(), arkcontrol.FoundationModel{Name: "media-generator", Version: "v1"}, []Requirement{{Operation: contracts.OperationMealTextCapture, Effort: "minimal"}})
	var failure *CheckError
	if !errors.As(err, &failure) || failure.Stage != "compatibility" {
		t.Fatal(err)
	}
}
func TestMySQLConfigurationChangesInvalidateQueuedModelSwitch(t *testing.T) {
	s, actor := fixture(t)
	id := s.Endpoints[contracts.OperationMealTextCapture]
	configs := configurationFixture{contracts.OperationMealTextCapture: {Endpoint: id, ReasoningEffort: "minimal"}}
	s.Configurations = configs
	in, command, _ := start(t, s, actor)
	configs[contracts.OperationVoiceTranscription] = contracts.ExecutionPolicy{Endpoint: id, ReasoningEffort: "minimal"}
	if err := s.Tick(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	latest, err := s.Store.List(context.Background(), in.Endpoint)
	if err != nil || len(latest) != 1 || latest[0].State != "conflict" {
		t.Fatal(command, latest, err)
	}
	if s.Cloud.(*cloudFixture).creates != 0 {
		t.Fatal("dispatched after requirements changed")
	}
}

func TestMySQLQueuedJobsRetainHistoricalEndpointRequirements(t *testing.T) {
	s, actor := fixture(t)
	ctx := context.Background()
	id := s.Endpoints[contracts.OperationMealTextCapture]
	s.Configurations = configurationFixture{contracts.OperationMealTextCapture: {Endpoint: "ep-new", ReasoningEffort: "minimal"}}
	key := actor
	if _, err := s.Store.DB.Exec(`INSERT INTO app_attest_keys(app_id,key_id,device_id,public_key_der,environment,receipt) VALUES('health',?,?,X'01','development',X'01')`, key, key); err != nil {
		t.Fatal(err)
	}
	defer s.Store.DB.Exec("DELETE FROM app_attest_keys WHERE app_id='health' AND key_id=?", key)
	if _, err := s.Store.DB.Exec(`INSERT INTO ai_jobs(app_id,id,request_id,body_digest,owner_key_id,owner_device_id,request_ciphertext,request_nonce,status,created_at,updated_at,expires_at) VALUES('health',?,?,?, ?,?,X'01',X'01','queued',?,?,?)`, actor, actor, "digest", key, key, time.Now(), time.Now(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	required, err := s.Requirements(ctx, id)
	if err != nil || len(required) != 5 {
		t.Fatal("old queued endpoint lost legacy requirements", required, err)
	}
	if _, err = s.Store.DB.Exec("UPDATE ai_jobs SET status='succeeded' WHERE app_id='health' AND id=?", actor); err != nil {
		t.Fatal(err)
	}
	required, err = s.Requirements(ctx, id)
	if err != nil || len(required) != 0 {
		t.Fatal("drained queue retained unrelated legacy requirements", required, err)
	}
}
