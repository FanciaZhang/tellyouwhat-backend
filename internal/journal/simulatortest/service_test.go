package simulatortest

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/platform/appregistry"
)

type fixture struct {
	s       *Service
	r       Registry
	private ed25519.PrivateKey
	now     time.Time
	nonces  *attestation.MemoryNonceStore
	app     appregistry.App
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{now: time.Date(2026, 10, 21, 2, 0, 0, 0, time.UTC), nonces: attestation.NewMemoryNonceStore(),
		app: appregistry.App{ID: appregistry.Journal, BundleID: BundleID, Hosts: []string{Host}}}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f.private = private
	start, end := f.now.AddDate(0, 0, -4), f.now.AddDate(0, 1, 0)
	f.r = Registry{Version: 1, Credentials: []Credential{{KeyID: keyPrefix + uuid.NewString(), PublicKey: base64.StdEncoding.EncodeToString(public),
		SubjectID: uuid.NewString(), IssuedAt: f.now.Add(-time.Minute), ExpiresAt: f.now.Add(time.Hour),
		SubscriptionStartedAt: &start, SubscriptionExpiresAt: &end, JournalCharacters: 10000, PreviewCharacters: 1000}}}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "registry.json")
	f.s = &Service{path: path, nonces: f.nonces, now: func() time.Time { return f.now }}
	f.write(t)
	f.s, err = New(path, "development", f.app, f.nonces, func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) write(t *testing.T) {
	t.Helper()
	body, err := json.Marshal(f.r)
	if err != nil {
		t.Fatal(err)
	}
	temporary := f.s.path + ".next"
	if err := os.WriteFile(temporary, body, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temporary, f.s.path); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) proof(t *testing.T, method, path string) attestation.RequestProof {
	t.Helper()
	nonce, err := f.nonces.Issue(context.Background(), f.r.Credentials[0].KeyID, 5*time.Minute, f.now)
	if err != nil {
		t.Fatal(err)
	}
	p := attestation.RequestProof{Method: method, Path: path, KeyID: f.r.Credentials[0].KeyID, RequestID: uuid.NewString(), Nonce: nonce,
		Timestamp: f.now.Format(time.RFC3339), BodySHA256: contracts.BodySHA256([]byte(`{"synthetic":"text"}`))}
	f.sign(&p)
	return p
}

func (f *fixture) sign(p *attestation.RequestProof) {
	p.Assertion = base64.StdEncoding.EncodeToString(ed25519.Sign(f.private, Message(*p)))
}

func TestSignedSimulatorAuthenticationAndAtomicReplay(t *testing.T) {
	f := newFixture(t)
	p := f.proof(t, "GET", "/v1/journal/read-aloud/allowance?purpose=journal")
	var successes, replays atomic.Int32
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			principal, err := f.s.Authenticate(context.Background(), p)
			if err == nil {
				if principal.AppID != "journal" || principal.KeyID != p.KeyID || principal.DeviceID != p.KeyID || principal.TransactionID != "" {
					t.Error("test principal forged a purchase", principal)
				}
				successes.Add(1)
			} else if errors.Is(err, attestation.ErrReplay) {
				replays.Add(1)
			} else {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 || replays.Load() != 11 {
		t.Fatal("nonce was not consumed atomically", successes.Load(), replays.Load())
	}
}

func TestSimulatorProofBindsBodyQueryIdentityAndIssuedNonce(t *testing.T) {
	for _, name := range []string{"body", "query", "request", "key", "timestamp", "nonce", "domain", "unissued nonce", "wrong method"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			original := f.proof(t, "GET", "/v1/journal/read-aloud/allowance?purpose=journal")
			p := original
			switch name {
			case "body":
				p.BodySHA256 = contracts.BodySHA256([]byte("changed"))
			case "query":
				p.Path = "/v1/journal/read-aloud/allowance?purpose=preview"
			case "request":
				p.RequestID = uuid.NewString()
			case "key":
				p.KeyID = keyPrefix + uuid.NewString()
			case "timestamp":
				p.Timestamp = f.now.Add(-301 * time.Second).Format(time.RFC3339)
				f.sign(&p)
			case "nonce":
				p.Nonce = uuid.NewString()
				f.sign(&p)
			case "domain":
				p.Assertion = base64.StdEncoding.EncodeToString(ed25519.Sign(f.private, []byte(strings.ReplaceAll(string(Message(p)), Host, "api.journal.tellyouwhat.cn"))))
			case "unissued nonce":
				p.Nonce = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
				f.sign(&p)
			case "wrong method":
				p.Method = "DELETE"
				f.sign(&p)
			}
			if _, err := f.s.Authenticate(context.Background(), p); !errors.Is(err, attestation.ErrAuthentication) {
				t.Fatal("invalid proof accepted", err)
			}
			if _, err := f.s.Authenticate(context.Background(), original); err != nil {
				t.Fatal("invalid proof burned the valid nonce", err)
			}
		})
	}
}

func TestSimulatorRevocationAndExpiryReloadWithoutRestart(t *testing.T) {
	for _, state := range []string{"revoked", "expired", "not yet valid", "missing registry", "bad registry"} {
		t.Run(state, func(t *testing.T) {
			f := newFixture(t)
			p := f.proof(t, "POST", "/v1/journal/read-aloud/syntheses")
			switch state {
			case "revoked":
				f.r.Credentials[0].Revoked = true
				f.write(t)
			case "expired":
				f.now = f.r.Credentials[0].ExpiresAt
			case "not yet valid":
				f.r.Credentials[0].IssuedAt = f.now.Add(time.Minute)
				f.write(t)
			case "missing registry":
				if err := os.Remove(f.s.path); err != nil {
					t.Fatal(err)
				}
			case "bad registry":
				if err := os.WriteFile(f.s.path, []byte("private-invalid-sentinel"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			p.Timestamp = f.now.Format(time.RFC3339)
			f.sign(&p)
			if _, err := f.s.Authenticate(context.Background(), p); err == nil || strings.Contains(err.Error(), "sentinel") {
				t.Fatal("revoked/expired identity accepted or private input leaked", err)
			}
		})
	}
}

func TestSimulatorEmptyRegistryStartsWithoutGrantingAnyIdentity(t *testing.T) {
	f := newFixture(t)
	proof := f.proof(t, "POST", "/v1/journal/read-aloud/syntheses")
	credential := f.r.Credentials[0]
	f.r.Credentials = []Credential{}
	f.write(t)
	service, err := New(f.s.path, "development", f.app, f.nonces, func() time.Time { return f.now })
	if err != nil {
		t.Fatal("empty explicit registry must support a disabled integration state", err)
	}
	if _, err := service.Authenticate(context.Background(), proof); !errors.Is(err, attestation.ErrAuthentication) {
		t.Fatal("empty registry granted or misreported access", err)
	}
	f.r.Credentials = []Credential{credential}
	f.write(t)
	if _, err := service.Authenticate(context.Background(), proof); err != nil {
		t.Fatal("registered identity was not reloaded", err)
	}
	f.r.Credentials = []Credential{}
	f.write(t)
	if _, err := service.Lookup(credential.KeyID); !errors.Is(err, attestation.ErrAuthentication) {
		t.Fatal("removing the final identity did not revoke it", err)
	}
}

func TestSimulatorRegistryRequiresPrivateBoundedUnambiguousCredentials(t *testing.T) {
	for _, name := range []string{"world readable", "public parent", "symlink", "oversized", "unknown field", "trailing json", "missing credentials", "version", "null credentials", "too many", "duplicate", "inconsistent shared subject", "long lifetime", "zero lifetime", "invalid key", "invalid public key", "invalid subject", "half subscription", "invalid subscription", "unbounded journal", "unbounded preview"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			writeInvalid := func(body []byte) {
				t.Helper()
				if err := os.WriteFile(f.s.path, body, 0600); err != nil {
					t.Fatal(err)
				}
			}
			switch name {
			case "world readable":
				if err := os.Chmod(f.s.path, 0644); err != nil {
					t.Fatal(err)
				}
			case "public parent":
				dir := filepath.Dir(f.s.path)
				if err := os.Chmod(dir, 0755); err != nil {
					t.Fatal(err)
				}
				defer os.Chmod(dir, 0700)
			case "symlink":
				real := f.s.path + ".real"
				if err := os.Rename(f.s.path, real); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(real, f.s.path); err != nil {
					t.Fatal(err)
				}
			case "oversized":
				writeInvalid([]byte(strings.Repeat(" ", 32769)))
			case "unknown field":
				body, err := json.Marshal(f.r)
				if err != nil {
					t.Fatal(err)
				}
				writeInvalid(append(body[:len(body)-1], []byte(`,"unexpected":true}`)...))
			case "trailing json":
				body, err := json.Marshal(f.r)
				if err != nil {
					t.Fatal(err)
				}
				writeInvalid(append(body, []byte(`{}`)...))
			case "missing credentials":
				writeInvalid([]byte(`{"version":1}`))
			default:
				c := &f.r.Credentials[0]
				switch name {
				case "version":
					f.r.Version = 2
				case "null credentials":
					f.r.Credentials = nil
				case "too many":
					for len(f.r.Credentials) < 17 {
						other := *c
						other.KeyID = keyPrefix + uuid.NewString()
						f.r.Credentials = append(f.r.Credentials, other)
					}
				case "duplicate":
					f.r.Credentials = append(f.r.Credentials, *c)
				case "inconsistent shared subject":
					other := *c
					other.KeyID = keyPrefix + uuid.NewString()
					other.JournalCharacters++
					f.r.Credentials = append(f.r.Credentials, other)
				case "long lifetime":
					c.ExpiresAt = c.IssuedAt.Add(24*time.Hour + time.Second)
				case "zero lifetime":
					c.ExpiresAt = c.IssuedAt
				case "invalid key":
					c.KeyID = "simulator_AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA"
				case "invalid public key":
					c.PublicKey = base64.StdEncoding.EncodeToString([]byte("wrong"))
				case "invalid subject":
					c.SubjectID = "not-a-uuid"
				case "half subscription":
					c.SubscriptionExpiresAt = nil
				case "invalid subscription":
					c.SubscriptionExpiresAt = c.SubscriptionStartedAt
				case "unbounded journal":
					c.JournalCharacters = 100001
				case "unbounded preview":
					c.PreviewCharacters = 0
				}
				f.write(t)
			}
			if _, err := f.s.load(); err == nil {
				t.Fatal("invalid registry accepted")
			}
		})
	}
}

func TestSimulatorConstructorRejectsProductionWrongAppAndHost(t *testing.T) {
	f := newFixture(t)
	for _, name := range []string{"production", "health", "formal bundle", "formal host", "extra host", "relative path", "missing nonce store"} {
		t.Run(name, func(t *testing.T) {
			app := f.app
			app.Hosts = append([]string(nil), app.Hosts...)
			environment, path, nonces := "development", f.s.path, attestation.NonceStore(f.nonces)
			switch name {
			case "production":
				environment = "production"
			case "health":
				app.ID = appregistry.Health
			case "formal bundle":
				app.BundleID = "cn.tellyouwhat.journalapp"
			case "formal host":
				app.Hosts = []string{"api.journal.tellyouwhat.cn"}
			case "extra host":
				app.Hosts = append(app.Hosts, "localhost")
			case "relative path":
				path = "registry.json"
			case "missing nonce store":
				nonces = nil
			}
			if _, err := New(path, environment, app, nonces, nil); err == nil {
				t.Fatal("unsafe simulator runtime enabled")
			}
		})
	}
}

type failedNonces struct{ *attestation.MemoryNonceStore }

func (failedNonces) Consume(context.Context, string, string, time.Time) error {
	return errors.New("private-store-sentinel")
}

func TestSimulatorNonceInfrastructureFailureIsUnavailable(t *testing.T) {
	f := newFixture(t)
	p := f.proof(t, "GET", "/v1/journal/read-aloud/allowance")
	f.s.nonces = failedNonces{f.nonces}
	if _, err := f.s.Authenticate(context.Background(), p); err != attestation.ErrUnavailable {
		t.Fatal("infrastructure failure became invalid credentials", err)
	}
}

func TestSimulatorPathAllowlist(t *testing.T) {
	id := uuid.NewString()
	session := "/v1/journal/read-aloud/syntheses/" + id
	for _, tc := range []struct {
		method, path string
		allowed      bool
	}{
		{"POST", "/v1/privacy/consents", true}, {"POST", "/v1/journal/read-aloud/syntheses", true},
		{"GET", "/v1/journal/read-aloud/allowance?purpose=preview", true}, {"GET", session, true}, {"DELETE", session, true}, {"GET", session + "/events?after=11520000", true},
		{"GET", "/v1/ai/quota", false}, {"DELETE", "/v1/privacy/data", false}, {"POST", "/v1/development/entitlement", false},
		{"GET", session + "/events?after=1", false}, {"GET", session + "/events?after=11520002", false}, {"GET", session + "/events?after=02", false},
		{"GET", session + "/events?after=0&after=2", false}, {"GET", session + "/events?after=0&purpose=preview", false}, {"GET", session + "/events/extra", false},
		{"GET", "/v1/journal/read-aloud/allowance?purpose=journal&purpose=preview", false}, {"POST", "/v1/privacy/consents?", false},
		{"POST", "/v1/privacy/%63onsents", false}, {"POST", "/v1//privacy/consents", false}, {"GET", "https://" + Host + session, false},
		{"GET", "/v1/journal/read-aloud/syntheses/../allowance", false}, {"GET", session + "/events?after=-2", false},
	} {
		if got := Allowed(tc.method, tc.path); got != tc.allowed {
			t.Errorf("%s %s allowed=%v", tc.method, tc.path, got)
		}
	}
}
