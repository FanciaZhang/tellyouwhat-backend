//go:build albums_live

package albums

import (
	"os"
	"testing"
)

// Keep the old names so explicit historical commands fail rather than pass with no tests.
func TestLiveCOSNativePhotoPipeline(t *testing.T)     { retiredSyntheticCloudProbe(t) }
func TestLiveCOSNativeVideoPipeline(t *testing.T)     { retiredSyntheticCloudProbe(t) }
func TestLiveCOSNativeLivePhotoPipeline(t *testing.T) { retiredSyntheticCloudProbe(t) }
func TestLiveCOSNativeMixedPipeline(t *testing.T)     { retiredSyntheticCloudProbe(t) }

func retiredSyntheticCloudProbe(t *testing.T) {
	t.Helper()
	if os.Getenv("ALBUM_CI_LIVE_PROBE") != "1" {
		t.Skip("retired synthetic-subscription cloud probe")
	}
	t.Fatal("retired: synthetic subscriptions must not authorize real COS/CI; use the .dev App with real Apple Sandbox on the shared test backend")
}
