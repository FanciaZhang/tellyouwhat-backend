package adminportal

import (
	"context"
	"testing"

	"github.com/tellyouwhat/backend/internal/arkcontrol"
	"github.com/tellyouwhat/backend/internal/contracts"
)

type inventoryFixture struct{ ep arkcontrol.Endpoint }

func (i inventoryFixture) Endpoint(context.Context, string) (arkcontrol.Endpoint, error) {
	return i.ep, nil
}
func (i inventoryFixture) Models(context.Context) ([]arkcontrol.Model, error) { return nil, nil }
func TestAIPublicationRejectsUnverifiedModelAndSearch(t *testing.T) {
	ep := arkcontrol.Endpoint{ID: "ep-health", Status: "Running"}
	ep.Model.FoundationModel.Name = "doubao-seed-2-0-mini"
	ep.Model.FoundationModel.Version = "260428"
	s := &Server{config: Config{AI: &AIConfig{Inventory: inventoryFixture{ep}, Endpoints: map[contracts.Operation]string{contracts.OperationMealDecision: "ep-health"}}}}
	p := contracts.ExecutionPolicy{Version: "revision", Endpoint: "ep-health", ReasoningEffort: "low", WebSearchEnabled: true, TimeoutSeconds: 90}
	if err := s.validateAIPolicy(context.Background(), contracts.OperationMealDecision, p); err != nil {
		t.Fatal(err)
	}
	if s.validateAIPolicy(context.Background(), contracts.OperationMealTextCapture, p) == nil {
		t.Fatal("expanded search boundary")
	}
	p.Endpoint = "ep-journal"
	if s.validateAIPolicy(context.Background(), contracts.OperationMealDecision, p) == nil {
		t.Fatal("accepted unknown/shared application endpoint")
	}
	p.Endpoint = "ep-health"
	ep.Model.FoundationModel.Name = "doubao-seed-2-1-pro"
	s.config.AI.Inventory = inventoryFixture{ep}
	if s.validateAIPolicy(context.Background(), contracts.OperationMealDecision, p) == nil {
		t.Fatal("accepted uncertified cost/capability")
	}
}
