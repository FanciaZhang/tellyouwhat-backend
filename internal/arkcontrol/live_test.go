package arkcontrol

import (
	"context"
	"os"
	"testing"
	"time"
)

// Live validation is read-only and requires an explicit service credential file.
func TestLiveServiceInventory(t *testing.T) {
	path := os.Getenv("ARK_MANAGEMENT_TEST_CREDENTIAL_FILE")
	if path == "" {
		t.Skip("explicit service credentials not provided")
	}
	c, err := NewFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	models, err := c.Models(ctx)
	if err != nil || len(models) == 0 {
		t.Fatalf("model directory: %v", err)
	}
	activations, err := c.Activations(ctx)
	if err != nil || len(activations) == 0 {
		t.Fatalf("activations: %v", err)
	}
	if id := os.Getenv("ARK_MANAGEMENT_TEST_ENDPOINT"); id != "" {
		ep, err := c.Endpoint(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if ep.Model.FoundationModel.Name == "" {
			t.Fatal("model binding absent")
		}
	}
	t.Logf("service identity verified: %d models, %d activation entries", len(models), len(activations))
}

func TestLiveRollingDryRun(t *testing.T) {
	path := os.Getenv("ARK_MANAGEMENT_TEST_CREDENTIAL_FILE")
	endpoint := os.Getenv("ARK_MANAGEMENT_ROLLING_TEST_ENDPOINT")
	if path == "" || endpoint == "" {
		t.Skip("explicit isolated rolling endpoint not provided")
	}
	c, err := NewFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err = c.PreviewRolling(ctx, endpoint, FoundationModel{Name: "doubao-seed-2-0-mini", Version: "260428"})
	if err != nil {
		t.Fatalf("isolated endpoint dry run: %v", err)
	}
	t.Log("service identity passed native rolling dry run; no cloud mutation")
}
