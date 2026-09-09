package offerdelivery

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
	"github.com/tellyouwhat/backend/internal/testutil"
)

func fixture(t *testing.T) (Store, Pool, time.Time) {
	t.Helper()
	db := testutil.MySQL(t)
	cipher, err := mysqlstore.NewPayloadCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	s := Store{DB: db, Cipher: cipher}
	now := time.Now().UTC().Truncate(time.Microsecond)
	p := Pool{ID: "pool-1", OfferID: "offer-id", OfferName: "FRIENDS", Kind: "oneTime", Environment: "production", Capacity: 2, Active: true, ExpiresAt: now.Add(time.Hour), SyncedAt: now}
	if err = s.SyncPool(context.Background(), "health", p); err != nil {
		t.Fatal(err)
	}
	return s, p, now
}
func TestCSVValidatesCompleteBatch(t *testing.T) {
	for _, raw := range []string{"Code,URL\r\nABC123,https://example.test\r\nXYZ456,https://example.test\r\n", "\ufeffCode\nABC123\nXYZ456", "ABC123\nXYZ456"} {
		codes, err := ParseCSV([]byte(raw), 2)
		if err != nil || len(codes) != 2 {
			t.Fatalf("parse failed: %v", err)
		}
	}
	for _, raw := range []string{"Code\nABC123", "Code\nABC123\nABC123", "Code\nABC123\n=cmd()", "Code\nABC123\nXYZ456\nEXTRA7"} {
		if _, err := ParseCSV([]byte(raw), 2); !errors.Is(err, ErrInvalid) {
			t.Fatal("accepted an incomplete or invalid batch")
		}
	}
}
func TestNamedRequestLifecycleKeepsCodeSeparateFromVerifiedRedemption(t *testing.T) {
	s, p, now := fixture(t)
	ctx := context.Background()
	recipient := Recipient{Name: "Fixture recipient", Contact: "fixture@example.test", Reference: "tester-12", Channel: "manual", Note: "Synthetic record"}
	if err := s.ImportCodes(ctx, "health", p.ID, "admin", []string{"ABC123", "XYZ456"}, now); err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateRequest(ctx, "health", p.ID, "request-1", "admin", recipient, now)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := s.CreateRequest(ctx, "health", p.ID, "request-1", "admin", recipient, now)
	if err != nil || retry.ID != r.ID {
		t.Fatal("request retry created another application")
	}
	changed := recipient
	changed.Name = "Different person"
	if _, err = s.CreateRequest(ctx, "health", p.ID, "request-1", "admin", changed, now); !errors.Is(err, ErrConflict) {
		t.Fatal("idempotency accepted different recipient")
	}
	if _, err = s.Assign(ctx, "health", r.ID, "admin", r.Version, now); !errors.Is(err, ErrUnavailable) {
		t.Fatal("imported external codes were silently treated as unused")
	}
	if err = s.ConfirmAvailable(ctx, "health", p.ID, "admin", []string{"ABC123"}, now); err != nil {
		t.Fatal(err)
	}
	r, err = s.Assign(ctx, "health", r.ID, "admin", r.Version, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Assign(ctx, "health", r.ID, "admin", 1, now); !errors.Is(err, ErrConflict) {
		t.Fatal("duplicate assignment accepted")
	}
	code, err := s.Reveal(ctx, "health", r.ID, "admin", now)
	if err != nil || code != "ABC123" {
		t.Fatal("assigned code could not be revealed")
	}
	r, err = s.Transition(ctx, "health", r.ID, "admin", "deliver", r.Version, now)
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.Transition(ctx, "health", r.ID, "admin", "report_redeemed", r.Version, now)
	if err != nil {
		t.Fatal(err)
	}
	if r.Recipient != recipient || r.VerifiedAt != nil || r.ReportedRedeemedAt == nil {
		t.Fatal("lost identity or treated recipient report as Apple evidence")
	}
	counts, err := s.Summary(ctx, "health", p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Imported != 2 || counts.Available != 0 || counts.External != 1 || counts.Requests != 1 || counts.Delivered != 1 || counts.ReportedRedeemed != 1 || counts.LinkedVerified != 0 {
		t.Fatalf("unexpected lifecycle counts: %+v", counts)
	}
	var raw []byte
	if err = s.DB.QueryRow(`SELECT ciphertext FROM offer_delivery_requests WHERE id=?`, r.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(recipient.Name)) || bytes.Contains(raw, []byte(recipient.Contact)) {
		t.Fatal("recipient details stored in plaintext")
	}
	if _, err = s.Get(ctx, "journal", r.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("request crossed App boundary")
	}
	if _, err = s.Reveal(ctx, "journal", r.ID, "admin", now); !errors.Is(err, ErrNotFound) {
		t.Fatal("code crossed App boundary")
	}
	if err = s.ImportCodes(ctx, "health", p.ID, "admin", []string{"ABC123", "XYZ456"}, now); err != nil {
		t.Fatal(err)
	}
	counts, _ = s.Summary(ctx, "health", p.ID)
	if counts.Imported != 2 || counts.AssignedCodes != 1 {
		t.Fatal("reimport reset existing assignments")
	}
}
func TestConcurrentAllocationAndCancellationNeverReuseExposedCode(t *testing.T) {
	s, p, now := fixture(t)
	ctx := context.Background()
	if err := s.ImportCodes(ctx, "health", p.ID, "admin", []string{"ABC123", "XYZ456"}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmAvailable(ctx, "health", p.ID, "admin", []string{"ABC123"}, now); err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateRequest(ctx, "health", p.ID, "a", "admin", Recipient{Name: "A"}, now)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateRequest(ctx, "health", p.ID, "b", "admin", Recipient{Name: "B"}, now)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan Request, 2)
	failures := make(chan error, 2)
	for _, r := range []Request{a, b} {
		wg.Add(1)
		go func(r Request) {
			defer wg.Done()
			assigned, err := s.Assign(ctx, "health", r.ID, "admin", r.Version, now)
			if err != nil {
				failures <- err
			} else {
				results <- assigned
			}
		}(r)
	}
	wg.Wait()
	close(results)
	close(failures)
	if len(results) != 1 || len(failures) != 1 {
		t.Fatal("one code was allocated more than once")
	}
	if err := <-failures; !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	assigned := <-results
	if _, err = s.Reveal(ctx, "health", assigned.ID, "admin", now); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Transition(ctx, "health", assigned.ID, "admin", "cancel", assigned.Version, now); err != nil {
		t.Fatal(err)
	}
	if err = s.ConfirmAvailable(ctx, "health", p.ID, "admin", []string{"ABC123"}, now); !errors.Is(err, ErrConflict) {
		t.Fatal("cancelled assignment was recycled")
	}
	counts, _ := s.Summary(ctx, "health", p.ID)
	if counts.Available != 0 || counts.AssignedCodes != 1 {
		t.Fatal("cancelled code became available")
	}
}

func TestCustomPoolTracksRecipientsAndBoundsAssignments(t *testing.T) {
	s, p, now := fixture(t)
	ctx := context.Background()
	p.ID = "custom-pool"
	p.Kind = "custom"
	p.Code = "FRIENDS2026"
	if err := s.SyncPool(ctx, "health", p); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"a", "b", "c"} {
		r, err := s.CreateRequest(ctx, "health", p.ID, key, "admin", Recipient{Name: key}, now)
		if err != nil {
			t.Fatal(err)
		}
		r, err = s.Assign(ctx, "health", r.ID, "admin", 1, now)
		if key == "c" {
			if !errors.Is(err, ErrUnavailable) {
				t.Fatal("custom allocation exceeded configured capacity")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		code, err := s.Reveal(ctx, "health", r.ID, "admin", now)
		if err != nil || code != p.Code {
			t.Fatal("custom recipient could not read assigned code")
		}
	}
}

func TestVerifiedAssociationRequiresMatchingAppleEvidenceAndIsSeparateFromCodeProof(t *testing.T) {
	s, p, now := fixture(t)
	ctx := context.Background()
	p.ID = "verified-custom"
	p.Kind = "custom"
	p.Code = "FRIENDS2026"
	if err := s.SyncPool(ctx, "health", p); err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateRequest(ctx, "health", p.ID, "person", "admin", Recipient{Name: "Fixture recipient"}, now)
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.Assign(ctx, "health", r.ID, "admin", 1, now)
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.Transition(ctx, "health", r.ID, "admin", "deliver", r.Version, now)
	if err != nil {
		t.Fatal(err)
	}
	original := strings.Repeat("a", 64)
	if _, err = s.LinkVerified(ctx, "health", r.ID, "admin", original, r.Version, now); !errors.Is(err, ErrNotFound) {
		t.Fatal("unverified subscription accepted")
	}
	for _, v := range []struct {
		environment string
		kind        int
		tx          string
	}{{"sandbox", 3, "sandbox"}, {"production", 2, "promotion"}} {
		_, err = s.DB.Exec(`INSERT INTO app_store_offer_redemptions(app_id,environment,transaction_hash,original_transaction_hash,offer_identifier,offer_type,redeemed_at,expires_at) VALUES('health',?,UNHEX(SHA2(?,256)),UNHEX(?),'FRIENDS',?,?,?)`, v.environment, v.tx, original, v.kind, now, now.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.LinkVerified(ctx, "health", r.ID, "admin", original, r.Version, now); !errors.Is(err, ErrNotFound) {
		t.Fatal("sandbox or promotional evidence crossed into production code redemption")
	}
	_, err = s.DB.Exec(`INSERT INTO app_store_offer_redemptions(app_id,environment,transaction_hash,original_transaction_hash,offer_identifier,offer_type,redeemed_at,expires_at) VALUES('health','production',UNHEX(SHA2('verified',256)),UNHEX(?),'FRIENDS',3,?,?)`, original, now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.LinkVerified(ctx, "health", r.ID, "admin", original, r.Version, now)
	if err != nil || r.VerifiedAt == nil {
		t.Fatal("matching verified subscription not linked")
	}
	counts, err := s.Summary(ctx, "health", p.ID)
	if err != nil || counts.LinkedVerified != 1 || counts.ReportedRedeemed != 0 {
		t.Fatal("verified link lost source distinction")
	}
}
func TestExportQuarantinesAvailableCodesAndRetainsAssignedCodes(t *testing.T) {
	s, p, now := fixture(t)
	ctx := context.Background()
	if err := s.ImportCodes(ctx, "health", p.ID, "admin", []string{"ABC123", "XYZ456"}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmAvailable(ctx, "health", p.ID, "admin", []string{"ABC123", "XYZ456"}, now); err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateRequest(ctx, "health", p.ID, "person", "admin", Recipient{Name: "Fixture recipient"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Assign(ctx, "health", r.ID, "admin", 1, now); err != nil {
		t.Fatal(err)
	}
	if err = s.RecordExport(ctx, "health", p.ID, "admin", now); err != nil {
		t.Fatal(err)
	}
	counts, err := s.Summary(ctx, "health", p.ID)
	if err != nil || counts.Available != 0 || counts.External != 1 || counts.AssignedCodes != 1 {
		t.Fatal("export left externally exposed codes available")
	}
}

func TestLedgerSearchInventoryConfirmationAndEvidence(t *testing.T) {
	s, p, now := fixture(t)
	ctx := context.Background()
	if err := s.ImportCodes(ctx, "health", p.ID, "admin", []string{"ABC123", "XYZ456"}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmExternalInventory(ctx, "health", p.ID, "admin", 1, now); !errors.Is(err, ErrConflict) {
		t.Fatal("accepted stale inventory count", err)
	}
	if err := s.ConfirmExternalInventory(ctx, "health", p.ID, "admin", 2, now); err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateRequest(ctx, "health", p.ID, "ledger-test", "admin", Recipient{Name: "小林", Contact: "Lin@example.test", Channel: "朋友"}, now)
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.Search(ctx, "health", p.ID, "", "LIN@", "requested")
	if err != nil || len(page.Requests) != 1 || page.Requests[0].ID != r.ID {
		t.Fatal("named search failed", err)
	}
	page, err = s.Search(ctx, "health", p.ID, "", "小林", "delivered")
	if err != nil || len(page.Requests) != 0 {
		t.Fatal("status filter ignored", err)
	}
	page, err = s.Search(ctx, "journal", p.ID, "", "小林", "")
	if err != nil || len(page.Requests) != 0 {
		t.Fatal("cross-app search leak", err)
	}
	if _, err = s.Assign(ctx, "health", r.ID, "admin", 1, now); err != nil {
		t.Fatal(err)
	}
	sum, err := s.Summary(ctx, "health", p.ID)
	if err != nil || sum.AssignedRequests != 1 || sum.Available != 1 {
		t.Fatal("wrong allocation summary", sum, err)
	}
	events, err := s.Events(ctx, "health", p.ID)
	if err != nil || len(events) != 4 {
		t.Fatal("missing ledger audit", len(events), err)
	}
	verified, err := s.VerifiedSubscriptions(ctx, "health", p.ID)
	if err != nil || len(verified) != 0 {
		t.Fatal("fabricated evidence", err)
	}
	if _, err = s.DB.ExecContext(ctx, `INSERT INTO app_store_offer_redemptions(app_id,environment,transaction_hash,original_transaction_hash,offer_identifier,offer_type,redeemed_at,expires_at) VALUES('health','production',?,?,?,?,?,?)`, bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32), p.OfferName, 3, now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	verified, err = s.VerifiedSubscriptions(ctx, "health", p.ID)
	if err != nil || len(verified) != 1 || len(verified[0].Reference) != 64 {
		t.Fatal("missing real evidence", err)
	}
}

func TestHistoricalDeliveryUsesKnownCodeAndDoesNotCreateApplication(t *testing.T) {
	s, p, now := fixture(t)
	ctx := context.Background()
	codes := []string{"ABC123", "XYZ456"}
	if err := s.ImportCodes(ctx, "health", p.ID, "admin", codes, now); err != nil {
		t.Fatal(err)
	}
	// Past delivery remains recordable after the pool expires and is deactivated.
	p.Active = false
	p.ExpiresAt = now.Add(-time.Hour)
	if err := s.SyncPool(ctx, "health", p); err != nil {
		t.Fatal(err)
	}
	recipient := Recipient{Name: "历史领取人", Channel: "朋友邀请"}
	at := now.Add(-24 * time.Hour)
	r, err := s.RecordExternal(ctx, "health", p.ID, "past-delivery-1", "admin", recipient, "ABC123", at, now)
	if err != nil {
		t.Fatal(err)
	}
	if r.Source != "external" || r.Status != "delivered" || r.DeliveredAt == nil || !r.DeliveredAt.Equal(at) {
		t.Fatal("lost historical delivery", r)
	}
	retry, err := s.RecordExternal(ctx, "health", p.ID, "past-delivery-1", "admin", recipient, "ABC123", at, now)
	if err != nil || retry.ID != r.ID {
		t.Fatal("not idempotent", err)
	}
	if _, err = s.RecordExternal(ctx, "health", p.ID, "past-delivery-2", "admin", recipient, "ABC123", at, now); !errors.Is(err, ErrConflict) {
		t.Fatal("assigned a code twice", err)
	}
	if _, err = s.RecordExternal(ctx, "health", p.ID, "past-delivery-3", "admin", recipient, "NOTKNOWN", at, now); !errors.Is(err, ErrNotFound) {
		t.Fatal("unknown code accepted", err)
	}
	if _, err = s.RecordExternal(ctx, "health", p.ID, "past-delivery-4", "admin", recipient, "XYZ456", now.Add(time.Hour), now); !errors.Is(err, ErrInvalid) {
		t.Fatal("future delivery accepted", err)
	}
	summary, err := s.Summary(ctx, "health", p.ID)
	if err != nil || summary.Requests != 1 || summary.Applications != 0 || summary.ExternalDeliveries != 1 || summary.Delivered != 1 || summary.External != 1 || summary.LinkedVerified != 0 {
		t.Fatal("confused applications, delivery or redemption", summary, err)
	}
}

func TestDirectClaimNeedsNoRecipientSubmissionAndCanBeRevoked(t *testing.T) {
	s, p, now := fixture(t)
	ctx := context.Background()
	if err := s.ImportCodes(ctx, "health", p.ID, "admin", []string{"ABC123", "XYZ456"}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmExternalInventory(ctx, "health", p.ID, "admin", 2, now); err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateRequest(ctx, "health", p.ID, "direct-claim-fixture", "admin", Recipient{Name: "后台备注的小王"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetClaimLink(ctx, "health", r.ID, "admin", r.Version, true, now); !errors.Is(err, ErrConflict) {
		t.Fatal("unallocated record made claimable", err)
	}
	r, err = s.Assign(ctx, "health", r.ID, "admin", r.Version, now)
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.SetClaimLink(ctx, "health", r.ID, "admin", r.Version, true, now)
	if err != nil {
		t.Fatal(err)
	}
	generation := r.ClaimGeneration
	status, code, err := s.AccessClaim(ctx, "health", r.ID, generation, "status", now)
	if err != nil || code != "" || status.DeliveredAt != nil {
		t.Fatal("status revealed code or recorded delivery", err)
	}
	r, code, err = s.AccessClaim(ctx, "health", r.ID, generation, "claim", now)
	if err != nil || code == "" || r.DeliveredAt == nil {
		t.Fatal("direct claim failed", err)
	}
	sum, err := s.Summary(ctx, "health", p.ID)
	if err != nil || sum.Requests != 1 || sum.Delivered != 1 || sum.LinkedVerified != 0 {
		t.Fatal("claim fabricated identity/redemption", sum, err)
	}
	r, _, err = s.AccessClaim(ctx, "health", r.ID, generation, "feedback", now)
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.SetClaimLink(ctx, "health", r.ID, "admin", r.Version, false, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.AccessClaim(ctx, "health", r.ID, generation, "claim", now); !errors.Is(err, ErrNotFound) {
		t.Fatal("revoked link still disclosed code", err)
	}
	r, err = s.SetClaimLink(ctx, "health", r.ID, "admin", r.Version, true, now)
	if err != nil {
		t.Fatal(err)
	}
	if r.ClaimGeneration == generation {
		t.Fatal("revocation generation reused")
	}
	if _, _, err = s.AccessClaim(ctx, "health", r.ID, generation, "status", now); !errors.Is(err, ErrNotFound) {
		t.Fatal("reissuing reactivated old link", err)
	}
	if _, _, err = s.AccessClaim(ctx, "health", r.ID, r.ClaimGeneration, "status", now.Add(91*24*time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatal("expired link accepted", err)
	}
}
