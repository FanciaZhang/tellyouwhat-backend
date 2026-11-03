package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/journal/readaloud"
	"github.com/tellyouwhat/backend/internal/journal/simulatortest"
	"github.com/tellyouwhat/backend/internal/platform/appregistry"
	"github.com/tellyouwhat/backend/internal/privacy"
)

type simulatorProofRecorder struct {
	calls int
	proof RequestProof
	app   string
}

func (a *simulatorProofRecorder) Authenticate(_ context.Context, p RequestProof) (Principal, error) {
	a.calls++
	a.proof = p
	return Principal{AppID: a.app, KeyID: p.KeyID, DeviceID: p.KeyID}, nil
}

func TestSimulatorAuthenticationDispatchNeverFallsBackAndBindsQuery(t *testing.T) {
	for _, name := range []string{"valid", "normal", "disabled", "unknown scheme", "empty scheme", "duplicate scheme", "wrong host", "wrong app", "formal bundle", "wrong principal", "forbidden path", "unmarked simulator key"} {
		t.Run(name, func(t *testing.T) {
			normal, testAuth := &simulatorProofRecorder{app: "journal"}, &simulatorProofRecorder{app: "journal"}
			server := New(Dependencies{App: appregistry.App{ID: appregistry.Journal, BundleID: simulatortest.BundleID}, Authenticator: normal, ReadAloudSimulatorAuthenticator: testAuth})
			w := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(w)
			ctx.Request = httptest.NewRequest("GET", "https://"+simulatortest.Host+"/v1/journal/read-aloud/allowance?purpose=preview", nil)
			ctx.Request.Header.Set("X-Tellyouwhat-Authentication", simulatortest.Scheme)
			ctx.Request.Header.Set("X-Tellyouwhat-Key-ID", "simulator_"+uuid.NewString())
			switch name {
			case "normal":
				ctx.Request.Header.Del("X-Tellyouwhat-Authentication")
				ctx.Request.Header.Set("X-Tellyouwhat-Key-ID", "apple-key")
			case "disabled":
				server.readAloudSimulatorAuthenticator = nil
			case "unknown scheme":
				ctx.Request.Header.Set("X-Tellyouwhat-Authentication", "unknown")
			case "empty scheme":
				ctx.Request.Header.Set("X-Tellyouwhat-Authentication", "")
			case "duplicate scheme":
				ctx.Request.Header.Add("X-Tellyouwhat-Authentication", simulatortest.Scheme)
			case "wrong host":
				ctx.Request.Host = "api.journal.tellyouwhat.cn"
			case "wrong app":
				server.app.ID = appregistry.Health
			case "formal bundle":
				server.app.BundleID = "cn.tellyouwhat.journalapp"
			case "wrong principal":
				testAuth.app = "health"
			case "forbidden path":
				ctx.Request.URL.Path = "/v1/privacy/data"
				ctx.Request.URL.RawQuery = ""
			case "unmarked simulator key":
				ctx.Request.Header.Del("X-Tellyouwhat-Authentication")
			}
			_, failure := server.apiAuthenticate(ctx, uuid.New())
			switch name {
			case "valid":
				if failure != nil || testAuth.calls != 1 || normal.calls != 0 || testAuth.proof.Path != "/v1/journal/read-aloud/allowance?purpose=preview" {
					t.Fatal("test proof did not bind full URI", failure)
				}
			case "normal":
				if failure != nil || normal.calls != 1 || testAuth.calls != 0 || normal.proof.Path != "/v1/journal/read-aloud/allowance" {
					t.Fatal("normal App Attest behavior changed", failure)
				}
			default:
				if failure == nil || failure.status != 401 || normal.calls != 0 {
					t.Fatal("invalid test identity fell back to normal proof", failure)
				}
			}
		})
	}
}

func TestSignedSimulatorIdentityUsesRealHTTPConsentAdmission(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 21, 2, 0, 0, 0, time.UTC)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key := "simulator_" + uuid.NewString()
	app := appregistry.App{ID: appregistry.Journal, BundleID: simulatortest.BundleID, Hosts: []string{simulatortest.Host}}
	registry := simulatortest.Registry{Version: 1, Credentials: []simulatortest.Credential{{KeyID: key, PublicKey: base64.StdEncoding.EncodeToString(public), SubjectID: uuid.NewString(), IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour), JournalCharacters: 10000, PreviewCharacters: 1000}}}
	body, err := json.Marshal(registry)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "registry.json")
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	nonces := attestation.NewMemoryNonceStore()
	auth, err := simulatortest.New(path, "development", app, nonces, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	normal := &simulatorProofRecorder{app: "journal"}
	consents := privacy.NewService(privacy.NewMemoryRepository(), nil, nil, func() time.Time { return now })
	server := New(Dependencies{App: app, Authenticator: normal, ReadAloudSimulatorAuthenticator: auth, Privacy: consents, ReadAloudSessions: &readaloud.SessionService{}, ReadAloudAccess: deniedSpeechAccess{}})
	for _, tc := range []struct {
		scope, version string
		status         int
	}{
		{privacy.JournalReadAloudScope, privacy.JournalReadAloudDocumentVersion, 200},
		{privacy.ManagedAIScope, privacy.AIDocumentVersion, 422},
		{privacy.JournalIllustrationScope, privacy.JournalIllustrationDocumentVersion, 422},
		{privacy.JournalReadAloudScope, "old-version", 422},
	} {
		id := uuid.NewString()
		content := `{"consents":[{"scope":"` + tc.scope + `","documentVersion":"` + tc.version + `","granted":true}]}`
		request := httptest.NewRequest("POST", "https://"+simulatortest.Host+"/v1/privacy/consents", strings.NewReader(content))
		nonce, err := nonces.Issue(ctx, key, 5*time.Minute, now)
		if err != nil {
			t.Fatal(err)
		}
		proof := RequestProof{Method: "POST", Path: "/v1/privacy/consents", RequestID: id, KeyID: key, Nonce: nonce, Timestamp: now.Format(time.RFC3339), BodySHA256: contracts.BodySHA256([]byte(content))}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Tellyouwhat-Authentication", simulatortest.Scheme)
		request.Header.Set("X-Tellyouwhat-Request-ID", id)
		request.Header.Set("X-Tellyouwhat-Key-ID", key)
		request.Header.Set("X-Tellyouwhat-Nonce", nonce)
		request.Header.Set("X-Tellyouwhat-Timestamp", proof.Timestamp)
		request.Header.Set("X-Tellyouwhat-Assertion", base64.StdEncoding.EncodeToString(ed25519.Sign(private, simulatortest.Message(proof))))
		w := httptest.NewRecorder()
		server.Router().ServeHTTP(w, request)
		if w.Code != tc.status {
			t.Fatal("wrong signed consent admission", tc.scope, w.Code, w.Body.String())
		}
		if tc.status == 200 {
			repeated := httptest.NewRequest("POST", "https://"+simulatortest.Host+"/v1/privacy/consents", strings.NewReader(content))
			repeated.Header = request.Header.Clone()
			replay := httptest.NewRecorder()
			server.Router().ServeHTTP(replay, repeated)
			if replay.Code != 409 || !strings.Contains(replay.Body.String(), "replay_detected") {
				t.Fatal("signed HTTP replay accepted", replay.Code, replay.Body.String())
			}
		}
	}
	if normal.calls != 0 {
		t.Fatal("signed simulator requests used normal App Attest")
	}
	if ok, err := consents.HasRequiredConsents(ctx, Principal{AppID: "journal", KeyID: key}, []string{privacy.JournalReadAloudScope}); err != nil || !ok {
		t.Fatal("consent did not pass the existing privacy service", ok, err)
	}
}
