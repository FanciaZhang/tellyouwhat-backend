package airollout

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/aiconfig"
	"github.com/tellyouwhat/backend/internal/arkcontrol"
	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
	"github.com/tellyouwhat/backend/migrations"
)

var source = arkcontrol.FoundationModel{Name: "doubao-seed-2-0-mini", Version: "260215"}
var target = arkcontrol.FoundationModel{Name: "doubao-seed-2-0-lite", Version: "260428"}

type cloudFixture struct {
	ep                      arkcontrol.Endpoint
	rolling                 arkcontrol.Rolling
	creates, cancels, steps int
	loseReply               bool
}

func newCloud(id string) *cloudFixture {
	c := &cloudFixture{ep: arkcontrol.Endpoint{ID: id, Status: "Running"}}
	c.ep.Model.FoundationModel.Name = source.Name
	c.ep.Model.FoundationModel.Version = source.Version
	return c
}
func (c *cloudFixture) Endpoint(context.Context, string) (arkcontrol.Endpoint, error) {
	return c.ep, nil
}
func (c *cloudFixture) Rolling(context.Context, string) (arkcontrol.Rolling, error) {
	return c.rolling, nil
}
func (c *cloudFixture) Versions(_ context.Context, name string) ([]arkcontrol.Version, error) {
	return []arkcontrol.Version{{Name: name, Version: "260428"}}, nil
}
func (c *cloudFixture) ModelActivations(_ context.Context, names []string) ([]arkcontrol.Activation, error) {
	return []arkcontrol.Activation{{Name: names[0], State: "Available", Charges: []arkcontrol.ChargeItem{{Type: "InferencePrompt", Price: "0.0008", Unit: "千tokens"}, {Type: "InferenceCompletion", Price: "0.008", Unit: "千tokens"}, {Type: "AudioPrompt", Price: "0.012", Unit: "千tokens"}}}}, nil
}
func (c *cloudFixture) PreviewRolling(context.Context, string, arkcontrol.FoundationModel) error {
	return nil
}
func (c *cloudFixture) CreateRolling(_ context.Context, id string, m arkcontrol.FoundationModel, _ string) (string, error) {
	c.creates++
	c.ep.RollingID = "eprol-fixture"
	c.rolling = arkcontrol.Rolling{ID: c.ep.RollingID, EndpointID: id, In: m, Out: source, Status: "Running", Gray: 2}
	if c.loseReply {
		return "", arkcontrol.ErrUnavailable
	}
	return c.rolling.ID, nil
}
func (c *cloudFixture) CancelRolling(context.Context, string) error {
	c.cancels++
	c.rolling.Status = "Reverted"
	c.rolling.Gray = 0
	return nil
}
func (c *cloudFixture) RollbackRolling(context.Context, string) error {
	c.steps++
	c.rolling.Gray -= 2
	return nil
}
func fixture(t *testing.T) (*Service, string) {
	t.Helper()
	dsn := os.Getenv("MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("MYSQL_TEST_DSN required")
	}
	ctx := context.Background()
	db, err := mysqlstore.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var name string
	if err = db.QueryRow("SELECT DATABASE()").Scan(&name); err != nil || !strings.HasSuffix(name, "_test") {
		t.Fatal("isolated _test database required")
	}
	if err = migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	actor := uuid.NewString()
	if _, err = db.Exec("INSERT INTO admin_users(id,webauthn_id,display_name,role) VALUES(?,?,?,'admin')", actor, []byte(actor), "Rolling test"); err != nil {
		t.Fatal(err)
	}
	id := "ep-" + uuid.NewString()
	t.Cleanup(func() {
		db.Exec("DELETE FROM health_ai_rollout_commands WHERE actor=?", actor)
		db.Exec("DELETE FROM health_ai_endpoint_prices WHERE endpoint=?", id)
		db.Exec("DELETE FROM admin_audit_events WHERE admin_user_id=?", actor)
		db.Exec("DELETE FROM admin_users WHERE id=?", actor)
	})
	return &Service{Store: Store{DB: db}, Cloud: newCloud(id), Endpoints: map[contracts.Operation]string{contracts.OperationMealTextCapture: id}, Shared: map[string]bool{}, WritesEnabled: true}, actor
}
func mutation(actor string) aiconfig.Mutation {
	return aiconfig.Mutation{Actor: actor, Key: uuid.NewString(), RequestID: uuid.NewString()}
}
func start(t *testing.T, s *Service, actor string) (Input, Command, aiconfig.Mutation) {
	t.Helper()
	in := Input{Endpoint: s.Endpoints[contracts.OperationMealTextCapture], Action: "start", Target: target}
	p, err := s.Preview(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	m := mutation(actor)
	c, err := s.Submit(context.Background(), m, in, p)
	if err != nil {
		t.Fatal(err)
	}
	return in, c, m
}
func TestMySQLNativeLifecycleAndReplay(t *testing.T) {
	s, actor := fixture(t)
	ctx := context.Background()
	in, c, m := start(t, s, actor)
	cloud := s.Cloud.(*cloudFixture)
	// The ceiling exists before the cloud mutation and applies to legacy jobs.
	price, err := s.Store.AttemptPrice(s.Endpoints)(ctx, contracts.Request{Operation: contracts.OperationMealTextCapture})
	if err != nil || price == nil || price.InputNanosPerMillionTokens != 12_000_000_000 {
		t.Fatal("missing audio envelope", price, err)
	}
	if cloud.creates != 0 {
		t.Fatal("write during enqueue")
	}
	if err = s.Tick(ctx, in.Endpoint); err != nil {
		t.Fatal(err)
	}
	replay, err := s.Submit(ctx, m, in, Snapshot{})
	if err != nil || replay.ID != c.ID {
		t.Fatal("lost original result", err)
	}
	if cloud.creates != 1 {
		t.Fatal("duplicate creation")
	}
	cancelIn := Input{Endpoint: in.Endpoint, Action: "cancel"}
	p, err := s.Preview(ctx, cancelIn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Submit(ctx, mutation(actor), cancelIn, p); err != nil {
		t.Fatal(err)
	}
	if err = s.Tick(ctx, in.Endpoint); err != nil {
		t.Fatal(err)
	}
	if err = s.Tick(ctx, in.Endpoint); err != nil {
		t.Fatal(err)
	}
	if cloud.cancels != 1 {
		t.Fatal("duplicate cancellation")
	}
	pending, err := s.Store.Pending(ctx, in.Endpoint)
	if err != nil || len(pending) != 0 {
		t.Fatal("did not reconcile cancellation", pending, err)
	}
}
func TestMySQLUnknownResultNeverRetriesAfterRestart(t *testing.T) {
	s, actor := fixture(t)
	cloud := s.Cloud.(*cloudFixture)
	cloud.loseReply = true
	in, _, _ := start(t, s, actor)
	ctx := context.Background()
	if err := s.Tick(ctx, in.Endpoint); err != nil {
		t.Fatal(err)
	}
	restarted := &Service{Store: s.Store, Cloud: cloud, Endpoints: s.Endpoints, Shared: s.Shared, WritesEnabled: true}
	if err := restarted.Tick(ctx, in.Endpoint); err != nil {
		t.Fatal(err)
	}
	if cloud.creates != 1 {
		t.Fatal("unsafe retry")
	}
	commands, _ := s.Store.List(ctx, in.Endpoint)
	if len(commands) != 1 || commands[0].State != "uncertain" {
		t.Fatal("missing uncertain state", commands)
	}
	if _, err := restarted.Preview(ctx, in); !errors.Is(err, ErrConflict) {
		t.Fatal("allowed another create", err)
	}
}
func TestMySQLEndpointDriftAndOwnership(t *testing.T) {
	s, actor := fixture(t)
	ctx := context.Background()
	in, _, _ := start(t, s, actor)
	cloud := s.Cloud.(*cloudFixture)
	cloud.ep.Model.FoundationModel.Version = "999999"
	if err := s.Tick(ctx, in.Endpoint); err != nil {
		t.Fatal(err)
	}
	if cloud.creates != 0 {
		t.Fatal("dispatched after cloud drift")
	}
	if _, err := s.Store.AttemptPrice(s.Endpoints)(ctx, contracts.Request{Operation: contracts.OperationMealTextCapture}); err == nil {
		t.Fatal("price allowed unknown model")
	}
	s.Shared[in.Endpoint] = true
	if _, err := s.Preview(ctx, in); !errors.Is(err, ErrUnsupported) {
		t.Fatal("shared endpoint accepted", err)
	}
	if err := s.Tick(ctx, in.Endpoint); !errors.Is(err, ErrUnsupported) {
		t.Fatal("runner skipped ownership guard")
	}
}
func TestMySQLConcurrentCommandsAndAuditRollback(t *testing.T) {
	s, actor := fixture(t)
	ctx := context.Background()
	in := Input{Endpoint: s.Endpoints[contracts.OperationMealTextCapture], Action: "start", Target: target}
	p, err := s.Preview(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := s.Submit(ctx, mutation(actor), in, p); results <- err }()
	}
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		} else if !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
	}
	if wins != 1 {
		t.Fatalf("commands committed=%d", wins)
	}
	// A missing actor forces the audit FK failure; command/price must roll back.
	other := in
	other.Endpoint = "ep-other-" + uuid.NewString()
	s.Endpoints[contracts.OperationMealPhotoCapture] = other.Endpoint
	missing := mutation(uuid.NewString())
	_, err = s.Store.Enqueue(ctx, missing, other, p)
	if err == nil {
		t.Fatal("accepted unauditable command")
	}
	replay, err := s.Store.Replay(ctx, missing, other)
	if err != nil || replay != nil {
		t.Fatal("command escaped rollback", err)
	}
	price, err := s.Store.PriceState(ctx, other.Endpoint)
	if err != nil || price != nil {
		t.Fatal("price escaped rollback", err)
	}
}
func TestPriceUsesEveryTierAndUndiscountedAudio(t *testing.T) {
	var a arkcontrol.Activation
	if err := json.Unmarshal([]byte(`{"State":"Available","ChargeItems":[{"Type":"InferencePrompt","Price":"0.0004","UnitCode":"千tokens"},{"Type":"InferenceCompletion","Price":"0.004","UnitCode":"千tokens"}],"MultiChargeItems":[{"ChargeItems":[{"Type":"AudioPrompt","Price":"0.012","OriginalPrice":"0.027","UnitCode":"千tokens"},{"Type":"InferenceCompletion","Price":"0.0108","UnitCode":"千tokens"}]}]}`), &a); err != nil {
		t.Fatal(err)
	}
	p, err := Price(a)
	if err != nil || p.InputNanosPerMillionTokens != 27_000_000_000 || p.OutputNanosPerMillionTokens != 10_800_000_000 {
		t.Fatal(p, err)
	}
	a.Charges[0].Unit = "unknown"
	if _, err = Price(a); err == nil {
		t.Fatal("accepted unknown billing unit")
	}
}
func TestMySQLStalePriceFailsClosed(t *testing.T) {
	s, actor := fixture(t)
	in, _, _ := start(t, s, actor)
	ctx := context.Background()
	p, _ := s.Store.PriceState(ctx, in.Endpoint)
	p.SyncedAt = time.Now().Add(-6 * time.Minute)
	if err := s.Store.SavePrice(ctx, in.Endpoint, *p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.AttemptPrice(s.Endpoints)(ctx, contracts.Request{Operation: contracts.OperationMealTextCapture}); err == nil {
		t.Fatal("stale price allowed")
	}
}

func TestMySQLExplicitReconciliationAndCompletion(t *testing.T) {
	s, actor := fixture(t)
	cloud := s.Cloud.(*cloudFixture)
	cloud.loseReply = true
	in, _, _ := start(t, s, actor)
	ctx := context.Background()
	if err := s.Tick(ctx, in.Endpoint); err != nil {
		t.Fatal(err)
	}
	reconcile := Input{Endpoint: in.Endpoint, Action: "reconcile"}
	snap, err := s.Preview(ctx, reconcile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Submit(ctx, mutation(actor), reconcile, snap); err != nil {
		t.Fatal(err)
	}
	if err = s.Tick(ctx, in.Endpoint); err != nil {
		t.Fatal(err)
	}
	if cloud.creates != 1 {
		t.Fatal("reconciliation repeated cloud mutation")
	}
	cloud.rolling.Gray = 100
	cloud.ep.Model.FoundationModel.Name = target.Name
	cloud.ep.Model.FoundationModel.Version = target.Version
	if err = s.Tick(ctx, in.Endpoint); err != nil {
		t.Fatal(err)
	}
	commands, _ := s.Store.List(ctx, in.Endpoint)
	for _, c := range commands {
		if c.State != "succeeded" {
			t.Fatal("completion not observed", c.State)
		}
	}
}
func TestMySQLActualModelMetadataAndUnknownResponse(t *testing.T) {
	s, actor := fixture(t)
	in, _, _ := start(t, s, actor)
	ctx := context.Background()
	t.Cleanup(func() { s.Store.DB.Exec("DELETE FROM health_ai_model_attempts WHERE endpoint=?", in.Endpoint) })
	p, _ := s.Store.PriceState(ctx, in.Endpoint)
	r := contracts.Request{Operation: contracts.OperationMealTextCapture}
	record := s.Store.RecordModel(s.Endpoints)
	if !record(ctx, r, source.Name+"-"+source.Version, p.Price) {
		t.Fatal("known model not attributed")
	}
	if record(ctx, r, "unexpected-model", p.Price) {
		t.Fatal("unexpected model treated as known")
	}
	stored, _ := s.Store.PriceState(ctx, in.Endpoint)
	if !stored.Blocked || !stored.Drift {
		t.Fatal("unexpected model failed to block admission")
	}
	if record(ctx, r, "", p.Price) {
		t.Fatal("missing model inferred from endpoint")
	}
	attempts, err := s.Store.Attempts(ctx, in.Endpoint)
	if err != nil || len(attempts) != 3 {
		t.Fatal("metadata missing", err)
	}
}
