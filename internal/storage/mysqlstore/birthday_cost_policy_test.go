package mysqlstore

import (
	"testing"

	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/platformops"
)

func TestBirthdayCostPolicyAdmitsOnlyBirthdayInference(t *testing.T) {
	policy := platformops.Policy{Apps: map[string]platformops.AppPolicy{
		"health": {MonthlyBudgetNanos: 40 * costcontrol.NanosPerCNY, Paused: true},
	}}
	for _, operation := range []string{"birthday.propose", "birthday.revise"} {
		app, ok := costAppPolicy(policy, costcontrol.Attempt{AppID: "birthday", Operation: operation, Meter: "ark"})
		if !ok || app.MonthlyBudgetNanos != 10*costcontrol.NanosPerCNY {
			t.Fatalf("Birthday inference is missing its bounded budget: %#v %v", app, ok)
		}
	}
	for _, attempt := range []costcontrol.Attempt{
		{AppID: "unknown", Operation: "birthday.propose", Meter: "ark"},
		{AppID: "birthday", Operation: "journal.organize", Meter: "ark"},
		{AppID: "birthday", Operation: "birthday.propose", Meter: "other"},
	} {
		if _, ok := costAppPolicy(policy, attempt); ok {
			t.Fatalf("Unconfigured inference bypassed policy: %#v", attempt)
		}
	}
	app, ok := costAppPolicy(policy, costcontrol.Attempt{AppID: "health"})
	if !ok || app.MonthlyBudgetNanos != policy.Apps["health"].MonthlyBudgetNanos || !app.Paused || len(policy.Apps) != 1 {
		t.Fatal("Birthday admission changed an existing app policy")
	}
}
