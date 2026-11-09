//go:build albums_live

package albums

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

// These paid probes use a native-generated synthetic pair and fixed test bucket.
// No user media, database receipts, permission changes or deletion are involved.
func liveRoundtripFixture(t *testing.T) (*COSObjects, string, context.Context) {
	t.Helper()
	if os.Getenv("ALBUM_CI_LIVE_PROBE") != "1" {
		t.Skip("explicit live probe required")
	}
	root := os.Getenv("ALBUM_CI_LIVE_DIRECTORY")
	if root == "" {
		t.Fatal("fixture directory required")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal("home unavailable")
	}
	var keys [2]struct{ SecretId, SecretKey string }
	for i, name := range []string{"backend-cos.json", "upload-cos.json"} {
		data, e := os.ReadFile(filepath.Join(home, ".config/tellyouwhat/albums-test", name))
		if e != nil || json.Unmarshal(data, &keys[i]) != nil {
			t.Fatal("credentials unavailable")
		}
	}
	s, err := NewCOSObjects(COSConfig{Bucket: "tellyouwhat-albums-1251995834", Region: "ap-guangzhou", SecretID: keys[0].SecretId, SecretKey: keys[0].SecretKey, UploadSecretID: keys[1].SecretId, UploadSecretKey: keys[1].SecretKey})
	if err != nil {
		t.Fatal("COS configuration invalid")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	if s.Check(ctx) != nil {
		t.Fatal("bucket preflight failed")
	}
	return s, root, ctx
}
func roundtripJSON(t *testing.T, root, name string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil || os.WriteFile(filepath.Join(root, name), data, 0600) != nil {
		t.Fatal("evidence write failed")
	}
}
func roundtripRead(t *testing.T, root, name string, value any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, name))
	if err != nil || json.Unmarshal(data, value) != nil {
		t.Fatal("fixture JSON unavailable: " + name)
	}
}
func roundtripBytes(t *testing.T, root, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, name))
	if err != nil || len(data) == 0 || len(data) > 32<<20 {
		t.Fatal("bounded synthetic file required: " + name)
	}
	return data
}
func roundtripHash(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func TestLiveCOSLiveComponents(t *testing.T) {
	s, root, ctx := liveRoundtripFixture(t)
	j := videoJobFixture()
	j.ProviderJobID = ""
	j.Spec.QueueID = "p543daa336f7a4d4b896654b933f8881b"
	var timing LivePhotoTiming
	roundtripRead(t, root, "timing.json", &timing)
	j.Spec.LivePhotoTiming = &timing
	evidence := map[string]any{"ownerID": j.OwnerID, "versionID": j.VersionID, "timing": timing, "scope": "synthetic native pair through production media adapters; no database or published backup"}
	defer func() { evidence["passed"] = !t.Failed(); roundtripJSON(t, root, "components-evidence.json", evidence) }()
	sources := map[string]SealedObject{}
	for _, part := range []struct{ id, file string }{{"photo", "source.jpg"}, {"movie", "source.mov"}} {
		data := roundtripBytes(t, root, part.file)
		o := SealedObject{ResourceID: part.id, Key: "albums/originals/" + j.OwnerID + "/" + j.VersionID + "/" + part.id, SizeBytes: int64(len(data)), SHA256: roundtripHash(data)}
		response, err := s.client.Object.Put(ctx, o.Key, bytes.NewReader(data), nil)
		if err != nil || response == nil || response.StatusCode != 200 {
			t.Fatal("synthetic source upload failed; no retry")
		}
		o.VersionID = response.Header.Get("x-cos-version-id")
		if response.Body != nil {
			response.Body.Close()
		}
		if !concreteCOSVersion(o.VersionID) {
			t.Fatal("source version missing")
		}
		sources[part.id] = o
	}
	evidence["sources"] = sources
	candidate, err := s.RenderPhotoCandidate(ctx, sources["photo"], PhotoDailyJPEG)
	if err != nil {
		t.Fatal("photo processing failed")
	}
	spec := PhotoWriteSpec{OwnerID: j.OwnerID, AttemptID: uuid.NewString(), Preset: PhotoDailyJPEG, SizeBytes: int64(len(candidate.Bytes)), SHA256: candidate.SHA256, Width: candidate.Width, Height: candidate.Height}
	photo, err := s.StorePhotoCandidateOnce(ctx, spec, candidate)
	if err != nil {
		t.Fatal("photo write failed/unknown; no retry")
	}
	evidence["photo"] = photo
	roundtripJSON(t, root, "photo-descriptor.json", LivePairPhotoSource{string(PhotoDailyJPEG), photo.SizeBytes, photo.SHA256, candidate.Width, candidate.Height, "image/jpeg"})
	if os.WriteFile(filepath.Join(root, "component.jpg"), candidate.Bytes, 0600) != nil {
		t.Fatal("photo save failed")
	}
	j.Spec.Source = sources["movie"]
	profile, err := s.InspectVideoSource(ctx, j.Spec.Source)
	if err != nil {
		t.Fatal("video source inspection failed")
	}
	j.Spec.Profile = profile
	j.Spec.MaxBytes = 32 << 20
	j.SubmittedAt = time.Now().UTC()
	evidence["job"] = j
	id, err := s.SubmitVideoOnce(ctx, j)
	if err != nil {
		t.Fatal("video submission failed/unknown; no retry")
	}
	j.ProviderJobID = id
	evidence["job"] = j
	for {
		done, err := s.PollVideoSubmission(ctx, j)
		if err != nil {
			t.Fatal("video poll failed")
		}
		if done {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("video deadline")
		case <-time.After(3 * time.Second):
		}
	}
	result, err := s.VerifyVideoResult(ctx, j)
	if err != nil {
		t.Fatal("video verification failed")
	}
	evidence["video"] = result
	response, err := s.client.Object.Get(ctx, result.Object.Key, nil, url.QueryEscape(result.Object.VersionID))
	if err != nil || response == nil {
		t.Fatal("video download failed")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, result.Object.SizeBytes+1))
	response.Body.Close()
	if err != nil || int64(len(data)) != result.Object.SizeBytes || roundtripHash(data) != result.Object.SHA256 {
		t.Fatal("video bytes mismatch")
	}
	if os.WriteFile(filepath.Join(root, "component.mp4"), data, 0600) != nil {
		t.Fatal("video save failed")
	}
	roundtripJSON(t, root, "video-descriptor.json", LivePairVideoSource{string(VideoDaily1080), LivePairResource{result.Object.SizeBytes, result.Object.SHA256}, result.Profile})
	for _, part := range []struct{ id, file string }{{"photo", "source.jpg"}, {"movie", "source.mov"}} {
		source := sources[part.id]
		if !s.photoSourceUnchanged(ctx, source) {
			t.Fatal("source latest changed")
		}
		r, e := s.client.Object.Get(ctx, source.Key, nil, url.QueryEscape(source.VersionID))
		if e != nil || r == nil {
			t.Fatal("source download failed")
		}
		actual, e := io.ReadAll(io.LimitReader(r.Body, source.SizeBytes+1))
		r.Body.Close()
		if e != nil || !bytes.Equal(actual, roundtripBytes(t, root, part.file)) {
			t.Fatal("source bytes changed")
		}
	}
	evidence["sourceLatestAndBytesUnchanged"] = true
}
func TestLiveCOSLivePair(t *testing.T) {
	s, root, ctx := liveRoundtripFixture(t)
	var previous struct {
		OwnerID, VersionID string
		Passed             bool
		Photo              SealedObject
		Video              VideoResult
		Timing             LivePhotoTiming
	}
	roundtripRead(t, root, "components-evidence.json", &previous)
	if !previous.Passed {
		t.Fatal("verified components required")
	}
	var descriptor LivePairDescriptor
	roundtripRead(t, root, "pair-descriptor.json", &descriptor)
	if descriptor.validate() != nil {
		t.Fatal("native pair descriptor invalid")
	}
	if descriptor.SourcePhoto.SizeBytes != previous.Photo.SizeBytes || descriptor.SourcePhoto.SHA256 != previous.Photo.SHA256 ||
		descriptor.SourceVideo.LivePairResource != (LivePairResource{previous.Video.Object.SizeBytes, previous.Video.Object.SHA256}) ||
		descriptor.SourceVideo.VideoProfile != previous.Video.Profile || descriptor.Timing != previous.Timing {
		t.Fatal("native pair does not belong to these verified components")
	}
	u := LivePairUpload{ID: uuid.NewString(), OwnerID: previous.OwnerID, VersionID: previous.VersionID, Preset: DailyLivePair, Descriptor: descriptor, State: "uploading", ExpiresAt: time.Now().UTC().Add(10 * time.Minute)}
	evidence := map[string]any{"pairUploadID": u.ID, "ownerID": u.OwnerID, "versionID": u.VersionID, "descriptor": descriptor, "scope": "production signed PUT, seal, reconcile and fixed-version read; no repository receipt"}
	defer func() { evidence["passed"] = !t.Failed(); roundtripJSON(t, root, "pair-evidence.json", evidence) }()
	client := &http.Client{Timeout: time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, part := range []struct{ id, file string }{{"photo", "paired.jpg"}, {"motion", "paired.mov"}} {
		data := roundtripBytes(t, root, part.file)
		expected, _ := u.resource(part.id)
		if int64(len(data)) != expected.SizeBytes || roundtripHash(data) != expected.SHA256 {
			t.Fatal("native pair bytes mismatch")
		}
		grant, err := s.AuthorizeLivePairPut(ctx, u, part.id, time.Now().UTC().Add(4*time.Minute))
		if err != nil {
			t.Fatal("pair upload grant failed")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, grant.URL, bytes.NewReader(data))
		if err != nil {
			t.Fatal("pair upload request failed")
		}
		for k, v := range grant.Headers {
			req.Header.Set(k, v)
		}
		res, err := client.Do(req)
		if res != nil {
			res.Body.Close()
		}
		if err != nil || res == nil || res.StatusCode != 200 {
			t.Fatal("pair PUT failed/unknown; no retry")
		}
	}
	u.State = "verifying"
	u.PhotoConsumed = true
	u.MovieConsumed = true
	for _, part := range []string{"photo", "motion"} {
		source, err := s.PrepareLivePairSeal(ctx, u, part)
		if err != nil {
			t.Fatal("pair preflight failed")
		}
		v, err := s.SealLivePairOnce(ctx, u, part, source)
		if err != nil {
			t.Fatal("pair seal failed/unknown; no retry")
		}
		recovered, err := s.ReconcileLivePairSeal(ctx, u, part)
		if err != nil || recovered != v {
			t.Fatal("pair seal reconciliation mismatch")
		}
		reader, err := s.OpenLivePairVersion(ctx, u, part, v)
		if err != nil {
			t.Fatal("pair fixed-version read failed")
		}
		expected, _ := u.resource(part)
		actual, err := io.ReadAll(io.LimitReader(reader, expected.SizeBytes+1))
		reader.Close()
		if err != nil || int64(len(actual)) != expected.SizeBytes || roundtripHash(actual) != expected.SHA256 {
			t.Fatal("pair archived bytes mismatch")
		}
		key, _ := u.key(part, false)
		u.Objects = append(u.Objects, SealedObject{ResourceID: part, Key: key, VersionID: v, SizeBytes: expected.SizeBytes, SHA256: expected.SHA256})
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+s.host+"/"+key+"?versionId="+url.QueryEscape(v), nil)
		res, err := client.Do(req)
		if res != nil {
			res.Body.Close()
		}
		if err != nil || res == nil || res.StatusCode != 403 {
			t.Fatal("anonymous read not denied")
		}
	}
	evidence["objects"] = u.Objects
	evidence["anonymousStatuses"] = []int{403, 403}
	writeRoundtripReadGrants(t, s, root, ctx, u)
}

// Rechecks the recorded job without uploading or submitting another task.
func TestLiveCOSLiveResume(t *testing.T) {
	s, root, ctx := liveRoundtripFixture(t)
	var state struct {
		Job     VideoJob
		Sources map[string]SealedObject
	}
	roundtripRead(t, root, "components-evidence.json", &state)
	j := state.Job
	if j.ProviderJobID == "" || j.Spec == nil {
		t.Fatal("recorded job required")
	}
	// Refresh only this controlled fixture's profile after an adapter correction.
	// No persisted production job or submitted request is changed.
	profile, err := s.InspectVideoSource(ctx, j.Spec.Source)
	if err != nil {
		t.Fatal("recorded source inspection failed")
	}
	j.Spec.Profile = profile
	done, err := s.PollVideoSubmission(ctx, j)
	if err != nil || !done {
		t.Fatal("recorded job not complete")
	}
	key, err := j.outputKey()
	if err != nil {
		t.Fatal("invalid recorded job")
	}
	head, err := s.client.Object.Head(ctx, key, nil)
	if err != nil || head == nil {
		t.Fatal("output head failed")
	}
	version := head.Header.Get("x-cos-version-id")
	if head.Body != nil {
		head.Body.Close()
	}
	info, _, err := s.client.CI.GetMediaInfo(ctx, key, nil, url.QueryEscape(version))
	if err != nil || info == nil {
		t.Fatal("output metadata failed")
	}
	roundtripJSON(t, root, "output-media-info.json", info.MediaInfo)
	result, err := s.VerifyVideoResult(ctx, j)
	if err != nil {
		t.Fatal("recorded output verification failed")
	}
	response, err := s.client.Object.Get(ctx, result.Object.Key, nil, url.QueryEscape(result.Object.VersionID))
	if err != nil || response == nil {
		t.Fatal("output read failed")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, result.Object.SizeBytes+1))
	response.Body.Close()
	if err != nil || int64(len(data)) != result.Object.SizeBytes || roundtripHash(data) != result.Object.SHA256 {
		t.Fatal("output hash mismatch")
	}
	if os.WriteFile(filepath.Join(root, "component.mp4"), data, 0600) != nil {
		t.Fatal("output save failed")
	}
	roundtripJSON(t, root, "video-descriptor.json", LivePairVideoSource{string(VideoDaily1080), LivePairResource{result.Object.SizeBytes, result.Object.SHA256}, result.Profile})
	for _, source := range state.Sources {
		if !s.photoSourceUnchanged(ctx, source) {
			t.Fatal("source latest changed")
		}
		r, e := s.client.Object.Get(ctx, source.Key, nil, url.QueryEscape(source.VersionID))
		if e != nil || r == nil {
			t.Fatal("source read failed")
		}
		b, e := io.ReadAll(io.LimitReader(r.Body, source.SizeBytes+1))
		r.Body.Close()
		if e != nil || int64(len(b)) != source.SizeBytes || roundtripHash(b) != source.SHA256 {
			t.Fatal("source bytes changed")
		}
	}
	var evidence map[string]any
	roundtripRead(t, root, "components-evidence.json", &evidence)
	evidence["video"] = result
	evidence["sourceLatestAndBytesUnchanged"] = true
	evidence["passed"] = true
	evidence["readOnlyResume"] = true
	evidence["job"] = j
	roundtripJSON(t, root, "components-evidence.json", evidence)
}

func writeRoundtripReadGrants(t *testing.T, s *COSObjects, root string, ctx context.Context, u LivePairUpload) {
	t.Helper()
	u.State = "verified" // Controlled adapter state, never a database/public backup receipt.
	expiry := time.Now().UTC().Add(4 * time.Minute).Truncate(time.Second)
	grants := map[string]any{"ownerID": u.OwnerID, "versionID": u.VersionID, "pairUploadID": u.ID, "expiresAt": expiry, "descriptor": u.Descriptor}
	for _, o := range u.Objects {
		signed, err := s.AuthorizeLivePairRead(ctx, u, o, expiry)
		if err != nil {
			t.Fatal("pair read grant failed")
		}
		name := o.ResourceID
		if name == "motion" {
			name = "movie"
		}
		grants[name] = map[string]any{"url": signed, "objectVersionID": o.VersionID}
	}
	roundtripJSON(t, root, "read-grants.json", grants) // Secret URLs; never preserve in repository evidence.
}

// Refreshes short-lived download grants from saved verified probe receipts only.
func TestLiveCOSLiveRefreshReadGrants(t *testing.T) {
	s, root, ctx := liveRoundtripFixture(t)
	var e struct {
		PairUploadID, OwnerID, VersionID string
		Descriptor                       LivePairDescriptor
		Objects                          []SealedObject
		Passed                           bool
	}
	roundtripRead(t, root, "pair-evidence.json", &e)
	if !e.Passed || len(e.Objects) != 2 {
		t.Fatal("successful pair probe required")
	}
	u := LivePairUpload{ID: e.PairUploadID, OwnerID: e.OwnerID, VersionID: e.VersionID, Descriptor: e.Descriptor, Objects: e.Objects}
	for _, o := range e.Objects {
		if u.validateReceipt(o) != nil {
			t.Fatal("invalid probe receipt")
		}
		reader, err := s.OpenLivePairVersion(ctx, u, o.ResourceID, o.VersionID)
		if err != nil {
			t.Fatal("pair recheck failed")
		}
		data, err := io.ReadAll(io.LimitReader(reader, o.SizeBytes+1))
		reader.Close()
		if err != nil || int64(len(data)) != o.SizeBytes || roundtripHash(data) != o.SHA256 {
			t.Fatal("pair bytes changed")
		}
	}
	writeRoundtripReadGrants(t, s, root, ctx, u)
}
