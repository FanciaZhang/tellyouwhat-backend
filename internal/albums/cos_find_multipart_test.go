package albums

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

func TestCOSFindMultipartRequiresUniqueExactKeyAndCompleteListing(t *testing.T) {
	key := "albums/staging/" + uuid.NewString() + "/" + uuid.NewString() + "/photo"
	for _, scenario := range []string{"one", "none", "neighbor", "multiple", "paged", "bad-marker", "duplicate-page", "wrong-prefix", "outside-prefix", "bad-id", "page-budget", "grouped", "delimiter", "missing-truncation", "bad-bucket", "wrong-request-marker"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			s := cosFixture(t, func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != "GET" || r.URL.Path != "/" || r.URL.Query().Get("prefix") != key || r.URL.Query().Get("max-uploads") != "1000" {
					t.Fatal("unscoped or mutating discovery")
				}
				uploadKey, id, prefix := key, "provider-one", key
				if scenario == "neighbor" {
					uploadKey += "_neighbor"
				}
				if scenario == "outside-prefix" {
					uploadKey = "other"
				}
				if scenario == "bad-id" {
					id = "bad/id"
				}
				if scenario == "wrong-prefix" {
					prefix = "other"
				}
				tail := "<IsTruncated>false</IsTruncated>"
				if scenario == "paged" || scenario == "bad-marker" || scenario == "duplicate-page" {
					if calls == 1 {
						tail = fmt.Sprintf(`<IsTruncated>true</IsTruncated><NextKeyMarker>%s</NextKeyMarker><NextUploadIdMarker>provider-one</NextUploadIdMarker>`, key)
						if scenario == "bad-marker" {
							tail = `<IsTruncated>true</IsTruncated><NextKeyMarker>wrong</NextKeyMarker><NextUploadIdMarker>provider-one</NextUploadIdMarker>`
						}
					} else {
						if r.URL.Query().Get("key-marker") != key || r.URL.Query().Get("upload-id-marker") != "provider-one" {
							t.Fatal("lost pagination binding")
						}
						if scenario != "duplicate-page" {
							uploadKey += "_neighbor"
							id = "provider-two"
						}
					}
				}
				if scenario == "page-budget" {
					uploadKey = fmt.Sprintf("%s_neighbor%d", key, calls)
					id = fmt.Sprintf("provider-%d", calls)
					tail = fmt.Sprintf(`<IsTruncated>true</IsTruncated><NextKeyMarker>%s</NextKeyMarker><NextUploadIdMarker>%s</NextUploadIdMarker>`, uploadKey, id)
				}
				uploads := fmt.Sprintf(`<Upload><Key>%s</Key><UploadId>%s</UploadId></Upload>`, uploadKey, id)
				if scenario == "none" {
					uploads = ""
				}
				if scenario == "multiple" {
					uploads += fmt.Sprintf(`<Upload><Key>%s</Key><UploadId>provider-two</UploadId></Upload>`, key)
				}
				if scenario == "grouped" {
					tail += "<CommonPrefixes><Prefix>hidden/</Prefix></CommonPrefixes>"
				}
				if scenario == "delimiter" {
					tail += "<Delimiter>/</Delimiter>"
				}
				if scenario == "missing-truncation" {
					tail = ""
				}
				bucket, marker := "albums-test-1250000000", r.URL.Query().Get("key-marker")
				if scenario == "bad-bucket" {
					bucket = "foreign"
				}
				if scenario == "wrong-request-marker" {
					marker = "unrequested"
				}
				return cosResponse(r, fmt.Sprintf(`<ListMultipartUploadsResult><Bucket>%s</Bucket><MaxUploads>1000</MaxUploads><KeyMarker>%s</KeyMarker><UploadIdMarker>%s</UploadIdMarker><Prefix>%s</Prefix>%s%s</ListMultipartUploadsResult>`, bucket, marker, r.URL.Query().Get("upload-id-marker"), prefix, uploads, tail), ""), nil
			})
			id, err := s.FindStagingMultipart(context.Background(), key)
			if scenario == "one" || scenario == "paged" {
				if err != nil || id != "provider-one" {
					t.Fatal("unique exact session not found", err)
				}
			} else if !errors.Is(err, ErrMultipartPending) || id != "" {
				t.Fatal("ambiguous or incomplete listing bound", id, err)
			}
			if scenario == "page-budget" && calls != 10 {
				t.Fatal("unbounded discovery", calls)
			}
		})
	}
}
