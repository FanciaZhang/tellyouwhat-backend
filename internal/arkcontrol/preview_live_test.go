package arkcontrol

import (
	"context"
	"os"
	"testing"
)

// Explicit dry-run only: this test never changes endpoint bindings or traffic.
func TestLiveSelectedRollingPreview(t *testing.T) {
	endpoint := os.Getenv("ARK_PREVIEW_TEST_ENDPOINT")
	name := os.Getenv("ARK_PREVIEW_TEST_MODEL")
	version := os.Getenv("ARK_PREVIEW_TEST_VERSION")
	if endpoint == "" || name == "" || version == "" {
		t.Skip("explicit dry-run target required")
	}
	client, err := NewFromFile(os.Getenv("ARK_MANAGEMENT_TEST_CREDENTIAL_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	if err = client.PreviewRolling(context.Background(), endpoint, FoundationModel{Name: name, Version: version}); err != nil {
		t.Fatal(err)
	}
}
