package adminportal

import (
	"context"
	"github.com/tellyouwhat/backend/internal/arkcontrol"
	"github.com/tellyouwhat/backend/internal/promptconfig"
	"os"
	"testing"
	"time"
)

func TestLiveConfiguredPromptModels(t *testing.T) {
	path := os.Getenv("ARK_MANAGEMENT_TEST_CREDENTIAL_FILE")
	lite := os.Getenv("PROMPT_TEST_LITE_ENDPOINT")
	pro := os.Getenv("PROMPT_TEST_PRO_ENDPOINT")
	if path == "" || lite == "" || pro == "" {
		t.Skip("explicit read-only service identity and endpoints required")
	}
	client, err := arkcontrol.NewFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	models, err := client.Models(ctx)
	if err != nil || len(models) == 0 {
		t.Fatal("model catalog unavailable", err)
	}
	p := promptconfig.Defaults(lite, pro, pro, 90)["journal"]
	if err = ResolvePromptModels(ctx, &p, client); err != nil {
		t.Fatal("configured model verification", err)
	}
	t.Logf("service identity verified: %d catalog models; Lite %s/%s; Pro %s/%s; all configured prices valid", len(models), p.Journal.Organize.Lite.FoundationModel, p.Journal.Organize.Lite.ModelVersion, p.Journal.Organize.Pro.FoundationModel, p.Journal.Organize.Pro.ModelVersion)
}
