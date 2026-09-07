package arkcontrol

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSignedReadAndResponseBoundary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Query().Get("Action") != "GetEndpoint" || r.URL.Query().Get("Version") != "2024-01-01" {
			t.Error("incorrect management request")
		}
		if !strings.Contains(r.Header.Get("Authorization"), "Credential=test-ak/") || strings.Contains(r.Header.Get("Authorization"), "test-secret") {
			t.Error("missing signature or leaked secret")
		}
		fmt.Fprint(w, `{"ResponseMetadata":{},"Result":{"Id":"ep-test","Status":"Running","SupportRolling":true,"ModelReference":{"FoundationModel":{"Name":"mini","ModelVersion":"260428"}},"Secret":"must not escape"}}`)
	}))
	defer server.Close()
	c, err := newClient("test-ak", "test-secret", server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	ep, err := c.Endpoint(context.Background(), "ep-test")
	if err != nil {
		t.Fatal(err)
	}
	if ep.Model.FoundationModel.Name != "mini" || ep.SupportRolling == nil || !*ep.SupportRolling {
		t.Fatalf("unexpected endpoint: %+v", ep)
	}
}

func TestPaginationAndRedactedFailure(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			fmt.Fprintf(w, `{"Result":{"TotalCount":2,"Items":[{"Name":"model-%d"}]}}`, calls)
			return
		}
		w.WriteHeader(403)
		fmt.Fprint(w, `{"ResponseMetadata":{"Error":{"Code":"Denied","Message":"SECRET_ACCOUNT_DETAIL"}}}`)
	}))
	defer server.Close()
	c, _ := newClient("test-ak", "test-secret", server.URL, server.Client())
	models, err := c.Models(context.Background())
	if err != nil || len(models) != 2 {
		t.Fatalf("pagination: %v %v", models, err)
	}
	_, err = c.Models(context.Background())
	if err != ErrUnavailable {
		t.Fatalf("upstream error leaked: %v", err)
	}
}

func TestCredentialFileDoesNotFallBack(t *testing.T) {
	t.Setenv("VOLCENGINE_ACCESS_KEY", "ambient-ak")
	t.Setenv("VOLCENGINE_SECRET_KEY", "ambient-secret")
	if _, err := NewFromFile(filepath.Join(t.TempDir(), "missing")); err != ErrCredentials {
		t.Fatal("fell back to ambient credentials")
	}
	p := filepath.Join(t.TempDir(), "credentials.json")
	for _, content := range []string{`{}`, `{"accessKey":"a","secretKey":"b","extra":"c"}`, `{"accessKey":"a","secretKey":"b"} {}`} {
		if err := os.WriteFile(p, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := NewFromFile(p); err != ErrCredentials {
			t.Fatal("accepted malformed credentials")
		}
	}
	os.WriteFile(p, []byte(`{"accessKey":"a","secretKey":"b"}`), 0600)
	if _, err := NewFromFile(p); err != nil {
		t.Fatal(err)
	}
	os.Chmod(p, 0644)
	if _, err := NewFromFile(p); err != ErrCredentials {
		t.Fatal("accepted world-readable credentials")
	}
}
