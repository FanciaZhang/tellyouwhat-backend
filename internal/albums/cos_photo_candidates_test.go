package albums

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func candidateSource() SealedObject {
	return SealedObject{ResourceID: "photo", Key: "albums/originals/" + uuid.NewString() + "/" + uuid.NewString() + "/photo", VersionID: "version+with&reserved=#? characters", SizeBytes: 300000}
}
func candidateImage(t *testing.T, format string, width int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, 7))
	img.Set(0, 0, color.NRGBA{R: 220, A: 128})
	var out bytes.Buffer
	var err error
	if format == "jpeg" {
		err = jpeg.Encode(&out, img, nil)
	} else {
		err = png.Encode(&out, img)
	}
	if err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestCOSPhotoCandidatePinsVersionAndFixedPreset(t *testing.T) {
	source := candidateSource()
	for _, preset := range []PhotoPreset{PhotoGridJPEG, PhotoDailyJPEG, PhotoGridPNG, PhotoDailyPNG} {
		t.Run(string(preset), func(t *testing.T) {
			rule, format, _, _ := photoPresetRule(preset)
			payload := candidateImage(t, format, 11)
			requests := 0
			s := cosFixture(t, func(r *http.Request) (*http.Response, error) {
				requests++
				if r.Method != "GET" || r.URL.Path != "/"+source.Key || r.URL.Query().Get("versionId") != source.VersionID || len(r.URL.Query()) != 2 || !r.URL.Query().Has(rule) || r.URL.Fragment != "" || r.Header.Get("Authorization") == "" {
					t.Fatalf("incorrect processing request: %s", r.URL.Redacted())
				}
				return cosResponse(r, string(payload), source.VersionID), nil
			})
			result, err := s.RenderPhotoCandidate(context.Background(), source, preset)
			expected := sha256.Sum256(payload)
			if err != nil || result.Width != 11 || result.Height != 7 || result.Format != format || !bytes.Equal(result.Bytes, payload) || result.SHA256 != hex.EncodeToString(expected[:]) || requests != 1 {
				t.Fatalf("unexpected candidate: format=%s size=%dx%d error=%v calls=%d", result.Format, result.Width, result.Height, err, requests)
			}
			if format == "png" {
				img, _ := png.Decode(bytes.NewReader(result.Bytes))
				_, _, _, alpha := img.At(0, 0).RGBA()
				if alpha == 65535 {
					t.Fatal("transparent candidate flattened")
				}
			}
		})
	}
}

type candidateTrackedBody struct {
	io.Reader
	closed bool
}

func (b *candidateTrackedBody) Close() error { b.closed = true; return nil }

func TestCOSPhotoCandidateRejectsInvalidResponseAndCloses(t *testing.T) {
	source := candidateSource()
	good := candidateImage(t, "jpeg", 11)
	for _, scenario := range []string{"wrong-version", "partial", "encoding", "range", "too-large", "length", "wrong-format", "oversize-dimensions", "truncated-image", "html", "truncated-stream", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			payload := good
			switch scenario {
			case "wrong-format":
				payload = candidateImage(t, "png", 11)
			case "oversize-dimensions":
				payload = candidateImage(t, "jpeg", 513)
			case "truncated-image":
				payload = good[:len(good)/2]
			case "html":
				payload = []byte("<html>error</html>")
			}
			body := &candidateTrackedBody{Reader: bytes.NewReader(payload)}
			s := cosFixture(t, func(r *http.Request) (*http.Response, error) {
				res := cosResponse(r, string(payload), source.VersionID)
				res.Body = body
				switch scenario {
				case "wrong-version":
					res.Header.Set("x-cos-version-id", "latest-version")
				case "partial":
					res.StatusCode = 206
				case "encoding":
					res.Header.Set("Content-Encoding", "gzip")
				case "range":
					res.Header.Set("Content-Range", "bytes 0-1/999")
				case "too-large":
					res.ContentLength = 1 << 30
				case "length":
					res.ContentLength++
				case "truncated-stream":
					body.Reader = io.MultiReader(bytes.NewReader(payload), errorCandidateReader{})
				case "cancelled":
					cancel()
				}
				return res, nil
			})
			result, err := s.RenderPhotoCandidate(ctx, source, PhotoGridJPEG)
			if err == nil || result.Bytes != nil || !body.closed {
				t.Fatalf("accepted invalid candidate or leaked body: %v closed=%v", err, body.closed)
			}
		})
	}
}

type errorCandidateReader struct{}

func (errorCandidateReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestCOSPhotoCandidateRejectsUnboundedChunkedResponse(t *testing.T) {
	source := candidateSource()
	reader := &repeatedCandidateReader{}
	body := &candidateTrackedBody{Reader: io.LimitReader(reader, 16<<20)}
	s := cosFixture(t, func(r *http.Request) (*http.Response, error) {
		res := cosResponse(r, "", source.VersionID)
		res.ContentLength = -1
		res.Body = body
		return res, nil
	})
	if _, err := s.RenderPhotoCandidate(context.Background(), source, PhotoGridJPEG); !errors.Is(err, ErrPhotoCandidate) || !body.closed || reader.read > (2<<20)+1 {
		t.Fatalf("unbounded candidate: %v", err)
	}
}

type repeatedCandidateReader struct{ read int }

func (r *repeatedCandidateReader) Read(p []byte) (int, error) {
	r.read += len(p)
	clear(p)
	return len(p), nil
}

func TestCOSPhotoCandidateValidatesBeforeNetwork(t *testing.T) {
	source := candidateSource()
	s := cosFixture(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid candidate reached provider")
		return nil, nil
	})
	for _, change := range []func(*SealedObject){
		func(v *SealedObject) { v.Key = strings.Replace(v.Key, "/originals/", "/staging/", 1) },
		func(v *SealedObject) { v.VersionID = "null" },
		func(v *SealedObject) { v.ResourceID = "other" },
		func(v *SealedObject) { v.SizeBytes = 0 },
	} {
		invalid := source
		change(&invalid)
		if _, err := s.RenderPhotoCandidate(context.Background(), invalid, PhotoGridJPEG); !errors.Is(err, ErrInvalidManifest) {
			t.Fatalf("invalid source: %v", err)
		}
	}
	if _, err := s.RenderPhotoCandidate(context.Background(), source, PhotoPreset("arbitrary-client-rule")); !errors.Is(err, ErrInvalidManifest) {
		t.Fatal(err)
	}
}
