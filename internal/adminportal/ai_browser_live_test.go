package adminportal

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/adminauth"
	"github.com/tellyouwhat/backend/internal/aiconfig"
	"github.com/tellyouwhat/backend/internal/airollout"
	"github.com/tellyouwhat/backend/internal/appstoreconnect"
	"github.com/tellyouwhat/backend/internal/arkcontrol"
	"github.com/tellyouwhat/backend/internal/contracts"
	arkprovider "github.com/tellyouwhat/backend/internal/provider/ark"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
	"github.com/tellyouwhat/backend/migrations"
)

type browserOffers struct{ OfferManager }

func (browserOffers) ListOffers(context.Context) ([]appstoreconnect.Offer, error) {
	return []appstoreconnect.Offer{}, nil
}

// Opt-in browser acceptance uses real WebAuthn, SQL, and Ark; only Apple offers
// and the session transport are local fixtures. No production account is used.
func TestLiveAIBrowserServer(t *testing.T) {
	ready := os.Getenv("ARK_BROWSER_TEST_READY_FILE")
	endpoint := os.Getenv("ARK_BROWSER_TEST_ENDPOINT")
	if ready == "" || endpoint == "" {
		t.Skip("explicit browser fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	db, err := mysqlstore.Open(ctx, os.Getenv("MYSQL_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var name string
	if err = db.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&name); err != nil || !strings.HasSuffix(name, "_test") {
		t.Fatal("isolated test database required", err)
	}
	if err = migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	cloud, err := arkcontrol.NewFromFile(os.Getenv("ARK_MANAGEMENT_TEST_CREDENTIAL_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	readOnly := os.Getenv("ARK_BROWSER_TEST_READ_ONLY") == "1"
	ep, err := cloud.Endpoint(ctx, endpoint)
	if err != nil || (!readOnly && !strings.HasPrefix(ep.Name, "health-ai-admin-validation-")) {
		t.Fatal("disposable endpoint required", err)
	}
	repo := adminauth.NewMySQLRepository(db)
	token, err := adminauth.RandomToken(32)
	if err != nil {
		t.Fatal(err)
	}
	if err = repo.CreateBootstrapToken(ctx, adminauth.TokenHash(token), time.Now().Add(15*time.Minute)); err != nil {
		t.Fatal(err)
	}
	auth, err := adminauth.NewService(repo, adminauth.NewMemoryStateStore(time.Now), adminauth.Config{RPID: "admin.e2e.test", Origin: "https://admin.e2e.test", AppIDs: []string{"health"}}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	endpoints := map[contracts.Operation]string{}
	for _, op := range contracts.OperationValues() {
		endpoints[op] = endpoint
		if readOnly {
			if id := os.Getenv("HEALTH_ARK_ENDPOINT_" + strings.ToUpper(string(op))); id != "" {
				endpoints[op] = id
			}
		}
	}
	var management airollout.Cloud = cloud
	if readOnly {
		management = browserReadOnlyCloud{cloud}
	}
	rollouts := &airollout.Service{Configurations: aiconfig.MySQLStore{DB: db}, Compatibility: &airollout.Compatibility{Probe: arkprovider.New(arkprovider.Config{BaseURL: "https://ark.cn-beijing.volces.com", APIKey: os.Getenv("HEALTH_ARK_API_KEY")}, nil, nil)}, Store: airollout.Store{DB: db}, Cloud: management, Endpoints: endpoints, Shared: map[string]bool{}, WritesEnabled: true}
	key := make([]byte, 32)
	if _, err = rand.Read(key); err != nil {
		t.Fatal(err)
	}
	portal, err := NewServer(auth, map[string]OfferManager{"health": browserOffers{}}, NewMySQLOperationStore(db), NewMySQLMetricsReader(db), Config{
		Apps: []AdminApp{{ID: "health", DisplayName: "告你健康"}}, PreviewSigningKey: key,
		AI: &AIConfig{Store: aiconfig.MySQLStore{DB: db}, Inventory: cloud, Endpoints: endpoints, SharedEndpoints: map[string]bool{}, WritesEnabled: true, TimeoutSeconds: 90, Rollouts: rollouts},
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(portal.Router())
	defer server.Close()
	data, _ := json.Marshal(map[string]string{"serverURL": server.URL, "setupURL": "https://admin.e2e.test/setup#token=" + token})
	if err = os.WriteFile(ready, data, 0600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(ready)
	if !readOnly {
		go rollouts.Run(ctx)
	}
	t.Log("isolated browser server ready")
	for {
		if result, readErr := os.ReadFile(ready + ".done"); readErr == nil {
			if string(result) != "passed\n" {
				t.Fatal("browser journey did not pass")
			}
			t.Log("browser journey finished")
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

// The read-only acceptance mode rejects cloud mutations even if a browser test
// accidentally submits a command. Its configuration store is an isolated DB.
type browserReadOnlyCloud struct{ *arkcontrol.Client }

func (browserReadOnlyCloud) CreateRolling(context.Context, string, arkcontrol.FoundationModel, string) (string, error) {
	return "", errors.New("read-only acceptance")
}
func (browserReadOnlyCloud) CancelRolling(context.Context, string) error {
	return errors.New("read-only acceptance")
}
func (browserReadOnlyCloud) RollbackRolling(context.Context, string) error {
	return errors.New("read-only acceptance")
}
