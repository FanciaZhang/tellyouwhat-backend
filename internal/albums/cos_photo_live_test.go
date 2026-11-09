//go:build albums_live

package albums

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Explicit paid test. All media is generated below; no user photos are read.
// Only unique keys in the fixed development bucket are written, never deleted.
func TestLiveCOSPhotoAdapter(t *testing.T) {
	if os.Getenv("ALBUM_CI_PHOTO_PROBE") != "1" {
		t.Skip("explicit live probe required")
	}
	root := os.Getenv("ALBUM_CI_PHOTO_DIRECTORY")
	if root == "" {
		t.Fatal("evidence directory required")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal("home unavailable")
	}
	var credentials [2]struct{ SecretId, SecretKey string }
	for i, name := range []string{"backend-cos.json", "upload-cos.json"} {
		data, err := os.ReadFile(filepath.Join(home, ".config/tellyouwhat/albums-test", name))
		if err != nil || json.Unmarshal(data, &credentials[i]) != nil {
			t.Fatal("credentials unavailable")
		}
	}
	for _, name := range []string{"jpeg", "oriented-jpeg", "transparent-png"} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(root, name)
			if os.MkdirAll(dir, 0700) != nil {
				t.Fatal("evidence directory unavailable")
			}
			trace := []map[string]any{}
			transport := cosTestTransport(func(request *http.Request) (*http.Response, error) {
				response, err := http.DefaultTransport.RoundTrip(request)
				if response != nil {
					trace = append(trace, map[string]any{"method": request.Method, "status": response.StatusCode, "hasQuery": request.URL.RawQuery != "", "version": response.Header.Get("x-cos-version-id"), "aclHeader": request.Header.Get("x-cos-acl")})
				}
				return response, err
			})
			s, err := newCOSObjects(COSConfig{Bucket: "tellyouwhat-albums-1251995834", Region: "ap-guangzhou", SecretID: credentials[0].SecretId, SecretKey: credentials[0].SecretKey, UploadSecretID: credentials[1].SecretId, UploadSecretKey: credentials[1].SecretKey}, transport)
			if err != nil {
				t.Fatal("COS configuration invalid")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			if s.Check(ctx) != nil {
				t.Fatal("bucket preflight failed")
			}
			img := image.NewNRGBA(image.Rect(0, 0, 3000, 2000))
			for y := 0; y < 2000; y++ {
				for x := 0; x < 3000; x++ {
					c := color.NRGBA{R: 230, G: 30, B: 20, A: 255}
					if x >= 1500 {
						c = color.NRGBA{R: 20, G: 30, B: 230, A: 255}
					}
					if name == "transparent-png" && y < 1000 {
						c.A = 0
					}
					img.SetNRGBA(x, y, c)
				}
			}
			var encoded bytes.Buffer
			presets := []PhotoPreset{PhotoGridJPEG, PhotoDailyJPEG}
			if name == "transparent-png" {
				err = png.Encode(&encoded, img)
				presets = []PhotoPreset{PhotoGridPNG, PhotoDailyPNG}
			} else {
				err = jpeg.Encode(&encoded, img, &jpeg.Options{Quality: 95})
			}
			if err != nil {
				t.Fatal("fixture encoding failed")
			}
			payload := encoded.Bytes()
			if name == "oriented-jpeg" {
				// EXIF little-endian IFD0 Orientation=6 (90 degrees clockwise).
				exif := []byte{0xff, 0xe1, 0, 34, 'E', 'x', 'i', 'f', 0, 0, 'I', 'I', 42, 0, 8, 0, 0, 0, 1, 0, 0x12, 1, 3, 0, 1, 0, 0, 0, 6, 0, 0, 0, 0, 0, 0, 0}
				payload = append(append(append([]byte{}, payload[:2]...), exif...), payload[2:]...)
			}
			owner, version := uuid.NewString(), uuid.NewString()
			sum := sha256.Sum256(payload)
			source := SealedObject{ResourceID: "photo", Key: "albums/originals/" + owner + "/" + version + "/photo", SizeBytes: int64(len(payload)), SHA256: hex.EncodeToString(sum[:])}
			evidence := map[string]any{"source": source, "scope": "synthetic images; production render, store and reconcile adapters; no account, DB or deletion"}
			defer func() {
				evidence["trace"] = trace
				evidence["passed"] = !t.Failed()
				data, e := json.MarshalIndent(evidence, "", "  ")
				if e != nil || os.WriteFile(filepath.Join(dir, "evidence.json"), data, 0600) != nil {
					t.Error("evidence write failed")
				}
			}()
			put, err := s.client.Object.Put(ctx, source.Key, bytes.NewReader(payload), liveOriginalPutOptions(t))
			if err != nil || put == nil || put.StatusCode != 200 {
				t.Fatal("fixture upload failed")
			}
			source.VersionID = put.Header.Get("x-cos-version-id")
			if put.Body != nil {
				put.Body.Close()
			}
			evidence["source"] = source
			if !concreteCOSVersion(source.VersionID) {
				t.Fatal("source version missing")
			}
			verifyLiveLowFrequencyOriginal(t, ctx, s, source, payload)
			evidence["sourceStorageClass"] = os.Getenv("ALBUM_COS_PROBE_STORAGE_CLASS")
			results := []map[string]any{}
			for _, preset := range presets {
				candidate, err := s.RenderPhotoCandidate(ctx, source, preset)
				if err != nil {
					evidence["stage"] = "render:" + string(preset)
					t.Fatal("production image render failed")
				}
				decoded, _, err := image.Decode(bytes.NewReader(candidate.Bytes))
				if err != nil {
					t.Fatal("decode failed")
				}
				wantWidth, wantHeight := 512, 341
				if preset == PhotoDailyJPEG || preset == PhotoDailyPNG {
					wantWidth, wantHeight = 2560, 1707
				}
				if name == "oriented-jpeg" {
					wantWidth, wantHeight = wantHeight, wantWidth
				}
				if math.Abs(float64(candidate.Width-wantWidth)) > 1 || math.Abs(float64(candidate.Height-wantHeight)) > 1 {
					t.Fatal("incorrect resize/orientation")
				}
				r, g, b, a := decoded.At(candidate.Width/4, candidate.Height/4).RGBA()
				if name == "transparent-png" {
					if a != 0 {
						t.Fatal("transparent area flattened")
					}
				} else if r <= b*3 || r <= g*3 {
					t.Fatal("orientation/color sentinel lost")
				}
				bx, by := 3*candidate.Width/4, 3*candidate.Height/4
				if name == "oriented-jpeg" {
					bx = candidate.Width / 4
				}
				r, g, b, a = decoded.At(bx, by).RGBA()
				if b <= r*3 || b <= g*3 {
					t.Fatal("blue orientation/color sentinel lost")
				}
				if a != 65535 {
					t.Fatal("opaque area lost")
				}
				spec := PhotoWriteSpec{OwnerID: owner, AttemptID: uuid.NewString(), Preset: preset, SizeBytes: int64(len(candidate.Bytes)), SHA256: candidate.SHA256, Width: candidate.Width, Height: candidate.Height}
				evidence["pendingSpec"] = spec
				receipt, err := s.StorePhotoCandidateOnce(ctx, spec, candidate)
				if err != nil {
					evidence["stage"] = "store:" + string(preset)
					t.Fatal("production image write unknown/failed; no PUT retry")
				}
				recovered, err := s.ReconcilePhotoCandidate(ctx, spec)
				if err != nil || recovered != receipt {
					t.Fatal("read-only reconciliation mismatch")
				}
				if os.WriteFile(filepath.Join(dir, string(preset)+"."+candidate.Format), candidate.Bytes, 0600) != nil {
					t.Fatal("fixture result write failed")
				}
				if os.Getenv("ALBUM_COS_PROBE_STORAGE_CLASS") != "" {
					assertLiveStorageClass(t, ctx, s, receipt, "MAZ_STANDARD")
				}
				results = append(results, map[string]any{"spec": spec, "receipt": receipt})
				evidence["results"] = results
				// Private objects must remain unreadable without authorization.
				u := "https://" + s.host + "/" + receipt.Key + "?versionId=" + url.QueryEscape(receipt.VersionID)
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
				anonymous := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
				res, err := anonymous.Do(req)
				if res != nil {
					res.Body.Close()
				}
				if err != nil || res == nil || res.StatusCode != 403 {
					t.Fatal("anonymous access was not denied")
				}
				results[len(results)-1]["anonymousStatus"] = res.StatusCode
			}
			head, err := s.client.Object.Head(ctx, source.Key, nil)
			if err != nil || head == nil || head.Header.Get("x-cos-version-id") != source.VersionID {
				t.Fatal("source latest changed")
			}
			get, err := s.client.Object.Get(ctx, source.Key, nil, url.QueryEscape(source.VersionID))
			if err != nil || get == nil {
				t.Fatal("source recheck failed")
			}
			actual, err := io.ReadAll(io.LimitReader(get.Body, int64(len(payload)+1)))
			get.Body.Close()
			if err != nil || !bytes.Equal(payload, actual) {
				t.Fatal("source bytes changed")
			}
			evidence["sourceLatestAndBytesUnchanged"] = true
		})
	}
}
