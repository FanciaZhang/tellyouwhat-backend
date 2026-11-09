//go:build albums_live

package albums

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func TestLiveCOSSharingOriginalDownloadAndRevocation(t *testing.T) {
	retiredSyntheticCloudProbe(t)
}

func liveSharingCleaner(t *testing.T) *COSObjects {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal("home unavailable")
	}
	var keys [2]struct{ SecretId, SecretKey string }
	for i, name := range []string{"cleanup-credentials.json", "upload-cos.json"} {
		data, err := os.ReadFile(filepath.Join(home, ".config/tellyouwhat/albums-test", name))
		if err != nil || json.Unmarshal(data, &keys[i]) != nil {
			t.Fatal("dedicated cleanup credentials required before upload")
		}
	}
	cleaner, err := NewCOSObjects(COSConfig{Bucket: "tellyouwhat-albums-1251995834", Region: "ap-guangzhou", SecretID: keys[0].SecretId, SecretKey: keys[0].SecretKey, UploadSecretID: keys[1].SecretId, UploadSecretKey: keys[1].SecretKey})
	if err != nil {
		t.Fatal("cleanup configuration invalid")
	}
	return cleaner
}

// Retry only the recorded synthetic version; never delete by prefix or latest.
func TestLiveCOSSharingCleanupRecovery(t *testing.T) {
	objects, root, ctx := liveRoundtripFixture(t)
	var intent, objectsToDelete []SealedObject
	roundtripRead(t, root, "sharing-intent.json", &intent)
	roundtripRead(t, root, "sharing-object.json", &objectsToDelete)
	if len(intent) < 1 || len(intent) > 2 || len(intent) != len(objectsToDelete) {
		t.Fatal("invalid synthetic cleanup record count")
	}
	for i, object := range objectsToDelete {
		if intent[i].VersionID != "" || !concreteCOSVersion(object.VersionID) || !albumObjectKey(object.Key, "originals") || (object.ResourceID != "photo" && object.ResourceID != "video") || object.SizeBytes <= 0 || object.SizeBytes > 32<<20 {
			t.Fatal("invalid synthetic cleanup record")
		}
		intent[i].VersionID = object.VersionID
		if intent[i] != object {
			t.Fatal("cleanup record does not match intent")
		}
	}
	for _, object := range objectsToDelete {
		if liveSharingCleaner(t).deleteObjectVersion(ctx, object.Key, object.VersionID) != nil {
			t.Fatal("exact synthetic cleanup failed")
		}
		response, err := objects.client.Object.Get(ctx, object.Key, nil, url.QueryEscape(object.VersionID))
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		if err == nil || response == nil || response.StatusCode != http.StatusNotFound {
			t.Fatal("synthetic version absence unconfirmed")
		}
	}
	roundtripJSON(t, root, "sharing-cleanup-recovery.json", map[string]any{"exactVersionDeleted": true, "resources": len(objectsToDelete), "fixedVersionReadStatus": 404})
}
