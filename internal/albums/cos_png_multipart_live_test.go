//go:build albums_live

package albums

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	cos "github.com/tencentyun/cos-go-sdk-v5"
)

const pngMultipartScope = "albums-owned-png-multipart-v1"

type pngMultipartIntent struct {
	Scope, Owner, Upload, Grid, Daily, SHA256 string
	Bytes                                     int64
}

func pngMultipartPayload(t *testing.T) []byte {
	t.Helper()
	im := image.NewNRGBA(image.Rect(0, 0, 3072, 2048))
	random := uint64(19)
	for y := range 2048 {
		alpha := byte(255)
		if y < 682 {
			continue
		} else if y < 1365 {
			alpha = 128
		}
		for x := range 3072 {
			i := im.PixOffset(x, y)
			random = random*6364136223846793005 + 1442695040888963407
			im.Pix[i], im.Pix[i+1], im.Pix[i+2], im.Pix[i+3] = byte(random>>16), byte(random>>24), byte(random>>32), alpha
		}
	}
	var b bytes.Buffer
	if png.Encode(&b, im) != nil || b.Len() < 1<<20 || b.Len() > 32<<20 {
		t.Fatal("bounded synthetic PNG encoding failed")
	}
	return b.Bytes()
}

func pngMultipartAlpha(data []byte) error {
	im, err := png.Decode(bytes.NewReader(data))
	if err != nil || im.Bounds().Dx() < 1 || im.Bounds().Dy() < 6 {
		return fmt.Errorf("invalid PNG dimensions or encoding")
	}
	for i, want := range []uint32{0, 128, 255} {
		_, _, _, a := im.At(im.Bounds().Dx()/2, im.Bounds().Dy()*(2*i+1)/6).RGBA()
		actual := a >> 8
		if want != 128 && actual != want || want == 128 && (actual+1 < want || actual > want+1) {
			return fmt.Errorf("PNG alpha band %d differs: %d", i, actual)
		}
	}
	return nil
}

func TestPNGMultipartSyntheticAlpha(t *testing.T) {
	data := pngMultipartPayload(t)
	if err := pngMultipartAlpha(data); err != nil {
		t.Fatal(err)
	}
	var flattened bytes.Buffer
	if png.Encode(&flattened, image.NewGray(image.Rect(0, 0, 12, 12))) != nil || pngMultipartAlpha(flattened.Bytes()) == nil || pngMultipartAlpha([]byte("invalid")) == nil {
		t.Fatal("invalid or flattened PNG accepted")
	}
}

func pngMultipartSource(t *testing.T, in pngMultipartIntent) string {
	t.Helper()
	if in.Scope != pngMultipartScope || !validOwner(in.Owner) || !validOwner(in.Upload) || !validOwner(in.Grid) || !validOwner(in.Daily) || in.Grid == in.Daily || !validSHA256(in.SHA256) || in.Bytes < 1<<20 || in.Bytes > 32<<20 {
		t.Fatal("owned PNG intent invalid")
	}
	return "albums/originals/" + in.Owner + "/" + in.Upload + "/photo"
}

func cleanPNGMultipartProbe(t *testing.T, root string, reader, cleaner *COSObjects, in pngMultipartIntent) bool {
	t.Helper()
	key := pngMultipartSource(t, in)
	for _, preset := range []PhotoPreset{PhotoGridPNG, PhotoDailyPNG} {
		folder := filepath.Join(root, string(preset))
		_, err := os.Lstat(folder)
		if os.IsNotExist(err) {
			continue
		}
		var intent photoMultipartProbeIntent
		roundtripRead(t, folder, "intent.json", &intent)
		attempt := in.Grid
		if preset == PhotoDailyPNG {
			attempt = in.Daily
		}
		candidate, err := inspectPhotoCandidate(roundtripBytes(t, folder, "candidate.png"), preset)
		if err != nil || pngMultipartAlpha(candidate.Bytes) != nil || intent.Spec.OwnerID != in.Owner || intent.Spec.AttemptID != attempt || !cleanPhotoMultipartCandidateProbe(t, cleaner, reader, folder, intent, candidate, preset) {
			t.Error("PNG derivative cleanup unresolved; retain ownership intent")
			return false
		}
	}
	var source SealedObject
	closed, err := os.Lstat(filepath.Join(root, "source-closed.json"))
	if err != nil || !closed.Mode().IsRegular() {
		t.Error("source PUT closure unproven; retain intent without retrying")
		return false
	}
	roundtripRead(t, root, "source-closed.json", &source)
	if source.Key != key || !concreteCOSVersion(source.VersionID) || source.SHA256 != in.SHA256 || source.SizeBytes != in.Bytes {
		t.Error("source closure differs from intent")
		return false
	}
	versions, err := cleaner.listPurgeVersions(context.Background(), in.Owner, purgeTarget{key})
	uploads, uploadErr := cleaner.listPurgeUploads(context.Background(), in.Owner, purgeTarget{key})
	if err != nil || uploadErr != nil || len(versions) > 1 || len(uploads) != 0 {
		t.Error("unexpected source inventory; preserved")
		return false
	}
	for _, v := range versions {
		if v.DeleteMarker || v.VersionID != source.VersionID || v.SizeBytes != in.Bytes {
			t.Error("unexpected source version; preserved")
			return false
		}
		body, err := reader.OpenVersion(context.Background(), key, v.VersionID)
		if err != nil {
			t.Error("source readback unavailable")
			return false
		}
		data, readErr := io.ReadAll(io.LimitReader(body, in.Bytes+1))
		body.Close()
		if readErr != nil || int64(len(data)) != in.Bytes || roundtripHash(data) != in.SHA256 || cleaner.deletePurgeVersion(context.Background(), in.Owner, purgeTarget{key}, v) != nil {
			t.Error("source verification or cleanup failed")
			return false
		}
	}
	for _, key := range []string{key, "albums/derivatives/" + in.Owner + "/" + in.Grid + "/" + string(PhotoGridPNG), "albums/derivatives/" + in.Owner + "/" + in.Daily + "/" + string(PhotoDailyPNG)} {
		versions, err := cleaner.listPurgeVersions(context.Background(), in.Owner, purgeTarget{key})
		uploads, uploadErr := cleaner.listPurgeUploads(context.Background(), in.Owner, purgeTarget{key})
		if err != nil || uploadErr != nil || len(versions)+len(uploads) != 0 {
			t.Error("PNG exact-key cleanup incomplete")
			return false
		}
	}
	return true
}

// Operator-owned format probe, without a user account, synthetic entitlement,
// App API or deletion permission for any existing media. All CI calls are explicit.
func TestLiveCOSPNGMultipart(t *testing.T) {
	if os.Getenv("ALBUM_PNG_COS_PROBE") != "1" {
		t.Skip("explicit owned PNG processing and cleanup probe required")
	}
	phase := os.Getenv("ALBUM_PNG_COS_PHASE")
	if phase != "run" && phase != "recover" {
		t.Fatal("explicit run or recover phase required")
	}
	reader, root, ctx := liveRoundtripFixture(t)
	completionProbeLease(t, root)
	cleaner := completionProbeCleaner(t)
	trace := photoMultipartProbeTrace(t, reader)
	defer func() { roundtripJSON(t, root, phase+"-http.json", trace()) }()
	if phase == "recover" {
		var in pngMultipartIntent
		roundtripRead(t, root, "png-intent.json", &in)
		if !cleanPNGMultipartProbe(t, root, reader, cleaner, in) {
			t.Fatal("retained PNG intent still unresolved")
		}
		photoMultipartProbeCheckpoint(t, root, "recovery-result.json", map[string]any{"passed": true, "newUploads": 0, "newCITasks": 0, "exactKeysEmpty": 3})
		return
	}
	payload := pngMultipartPayload(t)
	in := pngMultipartIntent{pngMultipartScope, uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), roundtripHash(payload), int64(len(payload))}
	key := pngMultipartSource(t, in)
	for _, key := range []string{key, "albums/derivatives/" + in.Owner + "/" + in.Grid + "/" + string(PhotoGridPNG), "albums/derivatives/" + in.Owner + "/" + in.Daily + "/" + string(PhotoDailyPNG)} {
		vs, err := cleaner.listPurgeVersions(ctx, in.Owner, purgeTarget{key})
		us, uploadErr := cleaner.listPurgeUploads(ctx, in.Owner, purgeTarget{key})
		if err != nil || uploadErr != nil || len(vs)+len(us) != 0 {
			t.Fatal("fresh exact PNG keys required")
		}
	}
	photoMultipartProbeCheckpoint(t, root, "png-intent.json", in)
	defer func() {
		roundtripJSON(t, root, "cleanup-result.json", map[string]any{"passed": cleanPNGMultipartProbe(t, root, reader, cleaner, in), "exactOwnedKeysOnly": true})
	}()
	if os.WriteFile(filepath.Join(root, "source.png"), payload, 0600) != nil {
		t.Fatal("source evidence write failed")
	}
	response, err := reader.client.Object.Put(ctx, key, bytes.NewReader(payload), &cos.ObjectPutOptions{ObjectPutHeaderOptions: &cos.ObjectPutHeaderOptions{ContentType: "image/png", XCosStorageClass: "MAZ_STANDARD", XCosServerSideEncryption: "AES256"}})
	closePhotoResponse(response)
	if err != nil || response == nil || response.StatusCode != 200 || !concreteCOSVersion(response.Header.Get("x-cos-version-id")) {
		t.Fatal("source PUT not closed; preserve intent")
	}
	source := SealedObject{ResourceID: "photo", Key: key, VersionID: response.Header.Get("x-cos-version-id"), SizeBytes: in.Bytes, SHA256: in.SHA256}
	photoMultipartProbeCheckpoint(t, root, "source-closed.json", source)
	results := []map[string]any{}
	for _, preset := range []PhotoPreset{PhotoGridPNG, PhotoDailyPNG} {
		candidate, err := reader.RenderPhotoCandidate(ctx, source, preset)
		if err != nil || pngMultipartAlpha(candidate.Bytes) != nil {
			t.Fatal("real CI PNG alpha or processing failed")
		}
		width, height := 512, 341
		if preset == PhotoDailyPNG {
			width, height = 2560, 1707
		}
		if candidate.Width != width || candidate.Height < height-1 || candidate.Height > height+1 {
			t.Fatal("real CI PNG dimensions differ from the fixed preset")
		}
		folder := filepath.Join(root, string(preset))
		if os.Mkdir(folder, 0700) != nil || os.WriteFile(filepath.Join(folder, "candidate.png"), candidate.Bytes, 0600) != nil {
			t.Fatal("candidate evidence write failed")
		}
		attempt := in.Grid
		if preset == PhotoDailyPNG {
			attempt = in.Daily
		}
		intent := photoMultipartProbeIntent{photoMultipartProbeScope, PhotoWriteSpec{OwnerID: in.Owner, AttemptID: attempt, Preset: preset, SizeBytes: int64(len(candidate.Bytes)), SHA256: candidate.SHA256, Width: candidate.Width, Height: candidate.Height, Multipart: true}}
		photoMultipartProbeCheckpoint(t, folder, "intent.json", intent)
		id, err := reader.StartPhotoMultipart(ctx, intent.Spec)
		if err != nil {
			t.Fatal("PNG initiation unresolved; do not reinitialize")
		}
		photoMultipartProbeCheckpoint(t, folder, "session.json", map[string]string{"uploadID": id})
		receipt, err := reader.StorePhotoMultipartOnce(ctx, intent.Spec, id, candidate, func(context.Context) error {
			photoMultipartProbeCheckpoint(t, folder, "completion-request.json", map[string]bool{"mayComplete": true})
			return nil
		})
		if err != nil {
			t.Fatal("PNG completion unresolved; retain immutable intent")
		}
		photoMultipartProbeCheckpoint(t, folder, "receipt.json", receipt)
		recovered, err := reader.ReconcilePhotoCandidate(ctx, intent.Spec)
		if err != nil || recovered != receipt {
			t.Fatal("PNG fixed-version readback differs")
		}
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+reader.host+"/"+receipt.Key+"?versionId="+url.QueryEscape(receipt.VersionID), nil)
		client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		anonymous, err := client.Do(request)
		if anonymous != nil {
			anonymous.Body.Close()
		}
		if err != nil || anonymous == nil || anonymous.StatusCode != http.StatusForbidden {
			t.Fatal("PNG derivative was anonymously readable")
		}
		if preset == PhotoDailyPNG && intent.Spec.SizeBytes >= in.Bytes*9/10 {
			t.Fatal("synthetic PNG resizing did not save ten percent")
		}
		results = append(results, map[string]any{"preset": preset, "width": candidate.Width, "height": candidate.Height, "bytes": intent.Spec.SizeBytes, "sha256": candidate.SHA256, "alphaBands": []int{0, 128, 255}, "anonymousStatus": anonymous.StatusCode})
	}
	photoMultipartProbeCheckpoint(t, root, "png-result.json", map[string]any{"passed": true, "sourceBytes": in.Bytes, "sourceSHA256": in.SHA256, "results": results, "realCOSAndCI": true, "appAccountOrEntitlementCreated": false, "systemPhotoLibraryAcceptance": false})
}
