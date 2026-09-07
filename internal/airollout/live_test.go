package airollout

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/arkcontrol"
	"github.com/tellyouwhat/backend/internal/contracts"
)

// Only explicitly supplied disposable endpoints can be changed by this test.
func TestLiveNativeCommandLifecycle(t *testing.T) {
	endpoint := os.Getenv("ARK_NATIVE_LIFECYCLE_TEST_ENDPOINT")
	credentials := os.Getenv("ARK_MANAGEMENT_TEST_CREDENTIAL_FILE")
	if endpoint == "" || credentials == "" {
		t.Skip("explicit disposable endpoint and service credential required")
	}
	cloud, err := arkcontrol.NewFromFile(credentials)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	ep, err := cloud.Endpoint(ctx, endpoint)
	if err != nil || !strings.HasPrefix(ep.Name, "health-ai-admin-validation-") {
		t.Fatal("refusing endpoint outside disposable fixture", err)
	}
	s, actor := fixture(t)
	s.Cloud = cloud
	s.Endpoints = map[contracts.Operation]string{contracts.OperationMealTextCapture: endpoint}
	t.Cleanup(func() { s.Store.DB.Exec("DELETE FROM health_ai_endpoint_prices WHERE endpoint=?", endpoint) })
	in := Input{Endpoint: endpoint, Action: "start", Target: arkcontrol.FoundationModel{Name: "doubao-seed-2-0-lite", Version: "260428"}}
	preview, err := s.Preview(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	command, err := s.Submit(ctx, mutation(actor), in, preview)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("durable command committed", command.ID)
	if err = s.Tick(ctx, endpoint); err != nil {
		t.Fatal(err)
	}
	commands, err := s.Store.List(ctx, endpoint)
	if err != nil || len(commands) == 0 {
		t.Fatal(err)
	}
	rollingID := commands[0].RollingID
	if rollingID == "" {
		t.Fatal("no rolling ID", commands[0].State, commands[0].Detail)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = cloud.CancelRolling(cleanup, rollingID)
	})
	t.Log("native rolling created by service", rollingID)
	for {
		r, err := cloud.Rolling(ctx, rollingID)
		if err != nil {
			t.Fatal(err)
		}
		if r.Gray > 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(5 * time.Second):
		}
	}
	if os.Getenv("ARK_NATIVE_LIFECYCLE_TEST_ACTION") == "cancel" {
		stop := Input{Endpoint: endpoint, Action: "cancel"}
		snap, err := s.Preview(ctx, stop)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.Submit(ctx, mutation(actor), stop, snap); err != nil {
			t.Fatal(err)
		}
		if err = s.Tick(ctx, endpoint); err != nil {
			t.Fatal(err)
		}
		r, err := cloud.Rolling(ctx, rollingID)
		if err != nil || r.Gray != 0 || r.Status != "Reverted" {
			t.Fatal("cancel did not converge", r.Status, r.Gray, err)
		}
		t.Log("durable cancellation confirmed Reverted/0%")
		return
	}
	back := Input{Endpoint: endpoint, Action: "step_back"}
	snap, err := s.Preview(ctx, back)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Submit(ctx, mutation(actor), back, snap); err != nil {
		t.Fatal(err)
	}
	if err = s.Tick(ctx, endpoint); err != nil {
		t.Fatal(err)
	}
	t.Log("step back submitted")
	// Cancel is supported during Running. Reverting must first converge.
	for {
		r, err := cloud.Rolling(ctx, rollingID)
		if err != nil {
			t.Fatal(err)
		}
		if r.Status == "Running" {
			break
		}
		if r.Status == "Reverted" && r.Gray == 0 {
			t.Log("native step back returned to zero")
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(5 * time.Second):
		}
	}
	stop := Input{Endpoint: endpoint, Action: "cancel"}
	snap, err = s.Preview(ctx, stop)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Submit(ctx, mutation(actor), stop, snap); err != nil {
		t.Fatal(err)
	}
	if err = s.Tick(ctx, endpoint); err != nil {
		t.Fatal(err)
	}
	t.Log("cancel submitted; cloud cleanup also registered")
}
