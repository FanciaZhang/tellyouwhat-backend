package albums

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"hash/crc64"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

type photoMultipartProtocol struct {
	key, id, fault                        string
	part, committed                       bool
	initiations, puts, completions, reads int
	beforePart                            func()
	beforeCompletion                      func()
}

func photoMultipartTransport(t *testing.T, spec PhotoWriteSpec, candidate PhotoCandidate, fault string) (*COSObjects, *photoMultipartProtocol) {
	t.Helper()
	key, err := spec.key()
	if err != nil {
		t.Fatal(err)
	}
	p := &photoMultipartProtocol{key: key, id: "photo-multipart-session", fault: fault}
	s := cosFixture(t, func(r *http.Request) (*http.Response, error) {
		q := r.URL.Query()
		if r.Method == "GET" && r.URL.Path == "/" {
			if q.Get("prefix") != key || q.Get("max-uploads") != "1000" {
				t.Fatal("unscoped initialization discovery")
			}
			return cosResponse(r, fmt.Sprintf(`<ListMultipartUploadsResult><Bucket>albums-test-1250000000</Bucket><MaxUploads>1000</MaxUploads><Prefix>%s</Prefix><IsTruncated>false</IsTruncated><Upload><Key>%s</Key><UploadId>%s</UploadId></Upload></ListMultipartUploadsResult>`, key, key, p.id), ""), nil
		}
		if r.URL.Path != "/"+key {
			t.Fatal("foreign photo key", r.URL.Path)
		}
		if r.Method == "POST" && q.Has("uploads") {
			p.initiations++
			if r.Header.Get("x-cos-acl") != "private" || r.Header.Get("Content-Type") != "image/"+candidate.Format {
				t.Fatal("photo initialization must preserve privacy and format")
			}
			if p.fault == "init-lost" {
				p.fault = ""
				return nil, io.ErrUnexpectedEOF
			}
			return cosResponse(r, fmt.Sprintf(`<InitiateMultipartUploadResult><Bucket>albums-test-1250000000</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>`, key, p.id), ""), nil
		}
		if r.Method == "HEAD" {
			if p.fault == "head-failed" {
				res := cosResponse(r, "", "")
				res.StatusCode = 500
				return res, nil
			}
			if !p.committed {
				res := cosResponse(r, "", "")
				res.StatusCode = 404
				return res, nil
			}
			res := cosResponse(r, "", "photo-version+fixed")
			res.ContentLength = spec.SizeBytes
			return res, nil
		}
		if r.Method == "GET" && q.Get("versionId") != "" {
			p.reads++
			if q.Get("versionId") != "photo-version+fixed" {
				t.Fatal("photo readback not pinned")
			}
			return cosResponse(r, string(candidate.Bytes), "photo-version+fixed"), nil
		}
		if q.Get("uploadId") != p.id {
			t.Fatal("photo session changed", q)
		}
		switch r.Method {
		case "PUT":
			if p.beforePart != nil {
				p.beforePart()
			}
			p.puts++
			if q.Get("partNumber") != "1" {
				t.Fatal("wrong photo part")
			}
			data, err := io.ReadAll(r.Body)
			if err != nil || string(data) != string(candidate.Bytes) || r.ContentLength != spec.SizeBytes {
				t.Fatal("wrong photo bytes")
			}
			if p.fault == "part-not-sent" {
				p.fault = ""
				return nil, io.ErrUnexpectedEOF
			}
			p.part = true
			if p.fault == "part-lost" {
				p.fault = ""
				return nil, io.ErrUnexpectedEOF
			}
			res := cosResponse(r, "", "")
			res.Header.Set("ETag", "photo-part-etag")
			res.Header.Set("x-cos-hash-crc64ecma", strconv.FormatUint(crc64.Checksum(data, crc64.MakeTable(crc64.ECMA)), 10))
			return res, nil
		case "GET":
			size := spec.SizeBytes
			if p.fault == "wrong-part-size" {
				size++
			}
			part := ""
			if p.part {
				part = fmt.Sprintf(`<Part><PartNumber>1</PartNumber><ETag>photo-part-etag</ETag><Size>%d</Size></Part>`, size)
			}
			return cosResponse(r, fmt.Sprintf(`<ListPartsResult><Key>%s</Key><UploadId>%s</UploadId><IsTruncated>false</IsTruncated>%s</ListPartsResult>`, key, p.id, part), ""), nil
		case "POST":
			if p.beforeCompletion != nil {
				p.beforeCompletion()
			}
			p.completions++
			var body struct {
				Parts []struct {
					Number int    `xml:"PartNumber"`
					ETag   string `xml:"ETag"`
				} `xml:"Part"`
			}
			if xml.NewDecoder(r.Body).Decode(&body) != nil || len(body.Parts) != 1 || body.Parts[0].Number != 1 || body.Parts[0].ETag != "photo-part-etag" {
				t.Fatal("completion trusted client parts")
			}
			p.committed = true
			if p.fault == "complete-lost" {
				p.fault = ""
				return nil, io.ErrUnexpectedEOF
			}
			resultKey := key
			if p.fault == "wrong-complete-key" {
				resultKey = "foreign"
			}
			return cosResponse(r, fmt.Sprintf(`<CompleteMultipartUploadResult><Bucket>albums-test-1250000000</Bucket><Key>%s</Key><ETag>photo-final-etag</ETag></CompleteMultipartUploadResult>`, resultKey), "photo-version+fixed"), nil
		default:
			t.Fatal("unexpected photo mutation", r.Method)
			return nil, nil
		}
	})
	return s, p
}

func TestPhotoMultipartCompletionRequiresDurableAdmission(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		t.Run(fmt.Sprint(recovery), func(t *testing.T) {
			spec, candidate := photoWriteFixture(t)
			spec.Multipart = true
			s, protocol := photoMultipartTransport(t, spec, candidate, "")
			protocol.part = true
			denied := 0
			before := func(context.Context) error { denied++; return ErrDerivedRetired }
			var err error
			if recovery {
				_, err = s.ReconcilePhotoMultipart(context.Background(), spec, protocol.id, before)
			} else {
				_, err = s.StorePhotoMultipartOnce(context.Background(), spec, protocol.id, candidate, before)
			}
			if !errors.Is(err, ErrDerivedRetired) || denied != 1 || protocol.completions != 0 {
				t.Fatal("unfenced completion reached provider", err, protocol)
			}
		})
	}
}

func TestPhotoMultipartPreservesAllPreviewFormats(t *testing.T) {
	for _, preset := range []PhotoPreset{PhotoGridJPEG, PhotoDailyJPEG, PhotoGridPNG, PhotoDailyPNG} {
		t.Run(string(preset), func(t *testing.T) {
			_, format, _, _ := photoPresetRule(preset)
			candidate, err := inspectPhotoCandidate(candidateImage(t, format, 17), preset)
			if err != nil {
				t.Fatal(err)
			}
			spec, _ := photoWriteFixture(t)
			spec.Preset, spec.Multipart = preset, true
			spec.SizeBytes, spec.SHA256, spec.Width, spec.Height = int64(len(candidate.Bytes)), candidate.SHA256, candidate.Width, candidate.Height
			s, protocol := photoMultipartTransport(t, spec, candidate, "")
			id, err := s.StartPhotoMultipart(context.Background(), spec)
			if err != nil {
				t.Fatal(err)
			}
			result, err := s.StorePhotoMultipartOnce(context.Background(), spec, id, candidate, allowPhotoCompletionFixture)
			if err != nil || result.SHA256 != spec.SHA256 || protocol.puts != 1 || protocol.reads != 1 {
				t.Fatal(result, err)
			}
		})
	}
}

func TestPhotoMultipartCancelledRequestsDoNotInitiateOrComplete(t *testing.T) {
	spec, candidate := photoWriteFixture(t)
	spec.Multipart = true
	s := cosFixture(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("cancelled operation reached provider")
		return nil, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.StartPhotoMultipart(ctx, spec); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := s.FindPhotoMultipart(ctx, spec); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := s.StorePhotoMultipartOnce(ctx, spec, "photo-session", candidate, allowPhotoCompletionFixture); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := s.ReconcilePhotoMultipart(ctx, spec, "photo-session", allowPhotoCompletionFixture); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestPhotoMultipartSingleSessionAndLostResponseRecovery(t *testing.T) {
	for _, fault := range []string{"", "init-lost", "part-lost", "complete-lost"} {
		t.Run(fault, func(t *testing.T) {
			spec, candidate := photoWriteFixture(t)
			spec.Multipart = true
			s, p := photoMultipartTransport(t, spec, candidate, fault)
			id, err := s.StartPhotoMultipart(context.Background(), spec)
			if fault == "init-lost" {
				if !errors.Is(err, ErrPhotoWriteUncertain) {
					t.Fatal("lost initiation was trusted", err)
				}
				id, err = s.FindPhotoMultipart(context.Background(), spec)
			}
			if err != nil || id != p.id {
				t.Fatal(id, err)
			}
			receipt, err := s.StorePhotoMultipartOnce(context.Background(), spec, id, candidate, allowPhotoCompletionFixture)
			if fault == "part-lost" || fault == "complete-lost" {
				if !errors.Is(err, ErrPhotoWriteUncertain) {
					t.Fatal("lost mutation response was trusted", err)
				}
				receipt, err = s.ReconcilePhotoMultipart(context.Background(), spec, id, allowPhotoCompletionFixture)
			}
			if err != nil || receipt.SHA256 != spec.SHA256 || receipt.VersionID != "photo-version+fixed" || p.initiations != 1 || p.puts != 1 || p.completions != 1 {
				t.Fatal("duplicate or unverified photo write", receipt, err, p)
			}
		})
	}
}

func TestPhotoMultipartRejectsUnprovenCompletionAndCancelledBytes(t *testing.T) {
	for _, fault := range []string{"wrong-part-size", "wrong-complete-key", "head-failed", "empty", "cancelled", "wrong-candidate"} {
		t.Run(fault, func(t *testing.T) {
			spec, candidate := photoWriteFixture(t)
			spec.Multipart = true
			s, p := photoMultipartTransport(t, spec, candidate, fault)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var err error
			if fault == "cancelled" || fault == "wrong-candidate" {
				if fault == "cancelled" {
					cancel()
				} else {
					candidate.SHA256 = strings.Repeat("a", 64)
				}
				_, err = s.StorePhotoMultipartOnce(ctx, spec, p.id, candidate, allowPhotoCompletionFixture)
				if p.puts != 0 || p.completions != 0 {
					t.Fatal("unapproved bytes sent")
				}
			} else if fault == "head-failed" || fault == "empty" {
				_, err = s.ReconcilePhotoMultipart(ctx, spec, p.id, allowPhotoCompletionFixture)
				if p.puts != 0 || p.completions != 0 {
					t.Fatal("failed observation mutated session")
				}
			} else {
				_, err = s.StorePhotoMultipartOnce(ctx, spec, p.id, candidate, allowPhotoCompletionFixture)
				if fault == "wrong-part-size" && p.completions != 0 {
					t.Fatal("invalid parts completed")
				}
			}
			if err == nil {
				t.Fatal("unproven write accepted", fault)
			}
		})
	}
}
