//go:build albums_live

package albums

import (
	"bytes"
	"context"
	"github.com/google/uuid"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Operator-authorized provider probe, not an App account or subscription grant.
// Only a generated MP4 and its cover are written; cleanup pins exact versions.
func TestLiveCOSVideoCover(t *testing.T) {
	objects, root, ctx := liveRoundtripFixture(t)
	cleaner := liveSharingCleaner(t)
	payload := roundtripBytes(t, root, "source.mp4")
	if len(payload) > 1<<20 || len(payload) < 16 || string(payload[4:8]) != "ftyp" {
		t.Fatal("small synthetic MP4 required")
	}
	j := videoJobFixture()
	source := j.Spec.Source
	source.ResourceID = "video"
	source.Key = "albums/originals/" + j.OwnerID + "/" + j.VersionID + "/video"
	source.VersionID = ""
	source.SizeBytes = int64(len(payload))
	source.SHA256 = roundtripHash(payload)
	intent := []SealedObject{source}
	roundtripJSON(t, root, "video-cover-intent.json", intent)
	var owned []SealedObject
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		complete := len(owned) == len(intent)
		for _, object := range owned {
			if cleaner.deleteObjectVersion(cleanupCtx, object.Key, object.VersionID) != nil {
				complete = false
				continue
			}
			response, err := objects.client.Object.Get(cleanupCtx, object.Key, nil, url.QueryEscape(object.VersionID))
			if response != nil && response.Body != nil {
				response.Body.Close()
			}
			if err == nil || response == nil || response.StatusCode != http.StatusNotFound {
				complete = false
			}
		}
		roundtripJSON(t, root, "video-cover-cleanup.json", map[string]any{"complete": complete, "exactVersions": len(owned)})
		if !complete {
			t.Error("exact-version probe cleanup needs recovery")
		}
	})
	response, err := objects.client.Object.Put(ctx, source.Key, bytes.NewReader(payload), liveOriginalPutOptions(t))
	if response != nil {
		source.VersionID = response.Header.Get("x-cos-version-id")
		if response.Body != nil {
			response.Body.Close()
		}
	}
	if concreteCOSVersion(source.VersionID) {
		owned = append(owned, source)
		roundtripJSON(t, root, "video-cover-owned.json", owned)
	}
	if err != nil || response == nil || response.StatusCode != 200 || !concreteCOSVersion(source.VersionID) {
		t.Fatal("synthetic upload outcome needs recorded-key reconciliation")
	}
	candidate, err := objects.RenderVideoCover(ctx, source)
	if err != nil {
		t.Fatal("real video snapshot failed", err)
	}
	if candidate.Format != "jpeg" || candidate.Width > 512 || candidate.Height > 512 {
		t.Fatal("unbounded cover")
	}
	spec := PhotoWriteSpec{OwnerID: j.OwnerID, AttemptID: uuid.NewString(), Preset: PhotoGridJPEG, SizeBytes: int64(len(candidate.Bytes)), SHA256: candidate.SHA256, Width: candidate.Width, Height: candidate.Height}
	key, _ := spec.key()
	intent = append(intent, SealedObject{ResourceID: string(spec.Preset), Key: key, SizeBytes: spec.SizeBytes, SHA256: spec.SHA256})
	roundtripJSON(t, root, "video-cover-intent.json", intent)
	stored, err := objects.StorePhotoCandidateOnce(ctx, spec, candidate)
	if concreteCOSVersion(stored.VersionID) {
		owned = append(owned, stored)
		roundtripJSON(t, root, "video-cover-owned.json", owned)
	}
	if err != nil {
		t.Fatal("cover publication needs recorded-attempt reconciliation", err)
	}
	record := PhotoWriteRecord{VersionID: j.VersionID, ManifestDigest: j.ManifestDigest, Spec: spec, ObjectVersion: stored.VersionID}
	signed, err := objects.AuthorizePhotoPreviewRead(ctx, record, time.Now().UTC().Add(time.Minute))
	if err != nil {
		t.Fatal("cover signing failed")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, signed, nil)
	if err != nil {
		t.Fatal("cover request invalid")
	}
	fetched, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal("signed cover read failed")
	}
	defer fetched.Body.Close()
	data, err := io.ReadAll(io.LimitReader(fetched.Body, spec.SizeBytes+1))
	if err != nil || fetched.StatusCode != 200 || !bytes.Equal(data, candidate.Bytes) {
		t.Fatal("cover readback mismatch")
	}
	if os.WriteFile(filepath.Join(root, "cover.jpg"), data, 0600) != nil {
		t.Fatal("cover evidence write failed")
	}
	roundtripJSON(t, root, "video-cover-result.json", map[string]any{"sourceBytes": len(payload), "coverBytes": len(data), "coverSHA256": candidate.SHA256, "width": candidate.Width, "height": candidate.Height, "contentType": fetched.Header.Get("Content-Type"), "signedReadStatus": fetched.StatusCode, "replacementEligible": false})
}
