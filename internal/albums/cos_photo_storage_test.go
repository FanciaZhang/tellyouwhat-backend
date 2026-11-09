package albums

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"errors"
	"hash/crc64"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func photoWriteFixture(t *testing.T) (PhotoWriteSpec, PhotoCandidate) {
	t.Helper()
	candidate, err := inspectPhotoCandidate(candidateImage(t, "png", 11), PhotoGridPNG)
	if err != nil {
		t.Fatal(err)
	}
	return PhotoWriteSpec{OwnerID: uuid.NewString(), AttemptID: uuid.NewString(), Preset: PhotoGridPNG, SizeBytes: int64(len(candidate.Bytes)), SHA256: candidate.SHA256, Width: candidate.Width, Height: candidate.Height}, candidate
}

func TestPhotoWritePersistsPrivateBytesAndVerifiesExactVersion(t *testing.T) {
	spec, candidate := photoWriteFixture(t)
	key, _ := spec.key()
	var methods []string
	s := cosFixture(t, func(r *http.Request) (*http.Response, error) {
		methods = append(methods, r.Method)
		if r.URL.Path != "/"+key || r.URL.Scheme != "https" || !strings.Contains(r.Header.Get("Authorization"), "q-ak=test-id") {
			t.Fatal("wrong target or credentials")
		}
		if r.Method == "PUT" {
			data, _ := io.ReadAll(r.Body)
			hash := md5.Sum(data)
			if string(data) != string(candidate.Bytes) || r.Header.Get("x-cos-acl") != "private" || r.Header.Get("Content-Type") != "image/png" || r.Header.Get("Content-MD5") != base64.StdEncoding.EncodeToString(hash[:]) || r.ContentLength != spec.SizeBytes {
				t.Fatal("wrong upload bytes/headers")
			}
			res := cosResponse(r, "", "persisted+version/&?")
			res.Header.Set("x-cos-hash-crc64ecma", strconv.FormatUint(crc64.Checksum(data, crc64.MakeTable(crc64.ECMA)), 10))
			return res, nil
		}
		if r.Method != "GET" || r.URL.Query().Get("versionId") != "persisted+version/&?" {
			t.Fatal("readback not pinned to PUT result")
		}
		return cosResponse(r, string(candidate.Bytes), "persisted+version/&?"), nil
	})
	receipt, err := s.StorePhotoCandidateOnce(context.Background(), spec, candidate)
	if err != nil || receipt.Key != key || receipt.VersionID != "persisted+version/&?" || receipt.SizeBytes != spec.SizeBytes || receipt.SHA256 != spec.SHA256 || strings.Join(methods, ",") != "PUT,GET" {
		t.Fatalf("write receipt: %+v %v methods=%v", receipt, err, methods)
	}
}

func TestPhotoWriteLostResponseRecoversWithoutSecondPut(t *testing.T) {
	spec, candidate := photoWriteFixture(t)
	puts, heads, gets := 0, 0, 0
	s := cosFixture(t, func(r *http.Request) (*http.Response, error) {
		switch r.Method {
		case "PUT":
			puts++
			return nil, io.ErrUnexpectedEOF // provider committed but its response was lost
		case "HEAD":
			heads++
			res := cosResponse(r, "", "recovered-version")
			res.ContentLength = spec.SizeBytes
			return res, nil
		case "GET":
			gets++
			if r.URL.Query().Get("versionId") != "recovered-version" {
				t.Fatal("recovery used latest rather than HEAD version")
			}
			return cosResponse(r, string(candidate.Bytes), "recovered-version"), nil
		default:
			t.Fatal("unexpected operation")
			return nil, nil
		}
	})
	if _, err := s.StorePhotoCandidateOnce(context.Background(), spec, candidate); !errors.Is(err, ErrPhotoWriteUncertain) {
		t.Fatalf("lost response did not preserve uncertainty: %v", err)
	}
	for range 2 {
		receipt, err := s.ReconcilePhotoCandidate(context.Background(), spec)
		if err != nil || receipt.SHA256 != spec.SHA256 {
			t.Fatal("recovery failed", err)
		}
	}
	if puts != 1 || heads != 2 || gets != 2 {
		t.Fatalf("recovery duplicated write: %d/%d/%d", puts, heads, gets)
	}
}

func TestPhotoWriteRecoveryRejectsUnprovenResults(t *testing.T) {
	for _, scenario := range []string{"missing", "head-size", "head-null", "get-version", "get-size", "get-hash", "get-dimensions", "get-truncated", "get-range", "get-encoding", "get-uncompressed", "get-overflow"} {
		t.Run(scenario, func(t *testing.T) {
			spec, candidate := photoWriteFixture(t)
			if scenario == "get-hash" {
				spec.SHA256 = strings.Repeat("a", 64)
			}
			if scenario == "get-dimensions" {
				spec.Width++
			}
			var body *candidateTrackedBody
			s := cosFixture(t, func(r *http.Request) (*http.Response, error) {
				if r.Method == "HEAD" {
					res := cosResponse(r, "", "saved-version")
					res.ContentLength = spec.SizeBytes
					switch scenario {
					case "missing":
						res.StatusCode = 404
					case "head-size":
						res.ContentLength++
					case "head-null":
						res.Header.Set("x-cos-version-id", "null")
					}
					return res, nil
				}
				if r.Method != "GET" {
					t.Fatal("recovery mutated objects")
				}
				res := cosResponse(r, string(candidate.Bytes), "saved-version")
				body = &candidateTrackedBody{Reader: strings.NewReader(string(candidate.Bytes))}
				res.Body = body
				switch scenario {
				case "get-version":
					res.Header.Set("x-cos-version-id", "later-version")
				case "get-size":
					res.ContentLength++
				case "get-truncated":
					body.Reader = strings.NewReader(string(candidate.Bytes[:5]))
				case "get-range":
					res.Header.Set("Content-Range", "bytes 0-10/99")
				case "get-encoding":
					res.Header.Set("Content-Encoding", "gzip")
				case "get-uncompressed":
					res.Uncompressed = true
				case "get-overflow":
					body.Reader = strings.NewReader(string(candidate.Bytes) + "extra")
				}
				return res, nil
			})
			receipt, err := s.ReconcilePhotoCandidate(context.Background(), spec)
			if !errors.Is(err, ErrPhotoWriteUncertain) || receipt.Key != "" || (body != nil && !body.closed) {
				t.Fatalf("accepted invalid receipt or leaked body: %+v %v", receipt, err)
			}
		})
	}
}

func TestPhotoWriteRejectsInvalidSpecOrCandidateBeforeMutation(t *testing.T) {
	s := cosFixture(t, func(*http.Request) (*http.Response, error) { t.Fatal("invalid write reached network"); return nil, nil })
	for _, mutate := range []func(*PhotoWriteSpec){
		func(v *PhotoWriteSpec) { v.OwnerID = "../another" },
		func(v *PhotoWriteSpec) { v.AttemptID = "bad" },
		func(v *PhotoWriteSpec) { v.Preset = "../originals" },
		func(v *PhotoWriteSpec) { v.SHA256 = strings.Repeat("A", 64) },
		func(v *PhotoWriteSpec) { v.SizeBytes = 1 << 40 },
		func(v *PhotoWriteSpec) { v.Width = 0 },
	} {
		spec, candidate := photoWriteFixture(t)
		mutate(&spec)
		if _, err := s.StorePhotoCandidateOnce(context.Background(), spec, candidate); err == nil {
			t.Fatal("accepted invalid spec")
		}
		if _, err := s.ReconcilePhotoCandidate(context.Background(), spec); err == nil {
			t.Fatal("accepted invalid recovery spec")
		}
	}
	spec, candidate := photoWriteFixture(t)
	candidate.Width++
	if _, err := s.StorePhotoCandidateOnce(context.Background(), spec, candidate); err == nil {
		t.Fatal("trusted candidate metadata without decoding")
	}
}
