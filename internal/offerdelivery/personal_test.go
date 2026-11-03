package offerdelivery

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
)

func TestPersonalAllocatesExistingOneTimeCodesAndKeepsClaimsSeparate(t *testing.T) {
	s, p, now := fixture(t)
	ctx := context.Background()
	if err := s.ImportCodes(ctx, "health", p.ID, "admin", []string{"AAAA111", "BBBB222"}, now); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	if _, _, err := s.IssuePersonal(ctx, "health", p.OfferID, p.ID, id, "Friend", "admin", now); !errors.Is(err, ErrUnavailable) {
		t.Fatal("allocated unconfirmed inventory", err)
	}
	if err := s.ConfirmExternalInventory(ctx, "health", p.ID, "admin", 2, now); err != nil {
		t.Fatal(err)
	}
	v, r, err := s.IssuePersonal(ctx, "health", p.OfferID, p.ID, id, "Friend", "admin", now)
	if err != nil || v.State != "ready" || r.CodeID == "" || r.Source != "personal" || r.ClaimedAt != nil || r.DeliveredAt != nil || r.ClaimGeneration != 1 {
		t.Fatalf("allocation %+v %+v %v", v, r, err)
	}
	_, other, err := s.IssuePersonal(ctx, "health", p.OfferID, p.ID, uuid.NewString(), "Second friend", "admin", now)
	if err != nil || other.CodeID == r.CodeID {
		t.Fatal("same pool must support distinct recipients", err)
	}
	if _, _, err = s.IssuePersonal(ctx, "health", p.OfferID, p.ID, uuid.NewString(), "No inventory", "admin", now); !errors.Is(err, ErrUnavailable) {
		t.Fatal("oversold", err)
	}
	_, replay, err := s.IssuePersonal(ctx, "health", p.OfferID, p.ID, id, "Friend", "admin", now)
	if err != nil || replay.ID != r.ID {
		t.Fatal("duplicate allocation", err)
	}
	if _, _, err = s.IssuePersonal(ctx, "health", p.OfferID, p.ID, id, "Other name", "admin", now); !errors.Is(err, ErrConflict) {
		t.Fatal("idempotency mismatch", err)
	}
	if _, _, err = s.IssuePersonal(ctx, "journal", p.OfferID, p.ID, id, "Friend", "admin", now); !errors.Is(err, ErrNotFound) {
		t.Fatal("cross-App access", err)
	}
	var raw []byte
	if err = s.DB.QueryRow(`SELECT ciphertext FROM offer_personal_deliveries WHERE app_id='health' AND id=?`, id).Scan(&raw); err != nil || bytes.Contains(raw, []byte("Friend")) {
		t.Fatal("plaintext name", err)
	}
	r, err = s.Transition(ctx, "health", r.ID, "admin", "deliver", r.Version, now)
	if err != nil || r.ClaimedAt != nil {
		t.Fatal("manual delivery implied claim", err)
	}
	r, code, err := s.AccessClaim(ctx, "health", r.ID, r.ClaimGeneration, "claim", now)
	if err != nil || code == "" || r.ClaimedAt == nil || r.VerifiedAt != nil {
		t.Fatal("claim implied redemption", err)
	}
	summary, err := s.Summary(ctx, "health", p.ID)
	if err != nil || summary.Applications != 0 || summary.AssignedCodes != 2 || summary.Claimed != 1 || summary.LinkedVerified != 0 {
		t.Fatalf("summary %+v %v", summary, err)
	}
	r, err = s.SetClaimLink(ctx, "health", r.ID, "admin", r.Version, false, now)
	if err != nil {
		t.Fatal(err)
	}
	_, replay, err = s.IssuePersonal(ctx, "health", p.OfferID, p.ID, id, "Friend", "admin", now)
	if err != nil || replay.ClaimExpiresAt != nil {
		t.Fatal("retry revived revoked link", err)
	}
	var count int
	if err = s.DB.QueryRow(`SELECT COUNT(*) FROM offer_personal_deliveries`).Scan(&count); err != nil || count != 2 {
		t.Fatal("failed allocation left a partial record", count, err)
	}
}

func TestPersonalConcurrentAllocationsCannotReuseOrOversell(t *testing.T) {
	s, p, now := fixture(t)
	ctx := context.Background()
	if err := s.ImportCodes(ctx, "health", p.ID, "admin", []string{"AAAA111", "BBBB222"}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmExternalInventory(ctx, "health", p.ID, "admin", 2, now); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 3)
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := s.IssuePersonal(ctx, "health", p.OfferID, p.ID, uuid.NewString(), "Friend", "admin", now)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if !errors.Is(err, ErrUnavailable) {
			t.Fatal(err)
		}
	}
	if success != 2 {
		t.Fatal("wrong allocation count", success)
	}
	var codes int
	if err := s.DB.QueryRow(`SELECT COUNT(DISTINCT code_id) FROM offer_delivery_requests`).Scan(&codes); err != nil || codes != 2 {
		t.Fatal("reused code", codes, err)
	}
}

func TestPersonalRetryAfterTransactionFailureKeepsInventory(t *testing.T) {
	s, p, now := fixture(t)
	ctx := context.Background()
	if err := s.ImportCodes(ctx, "health", p.ID, "admin", []string{"AAAA111", "BBBB222"}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmExternalInventory(ctx, "health", p.ID, "admin", 2, now); err != nil {
		t.Fatal(err)
	}
	normal := s.Cipher
	s.Cipher = personalFailCipher{normal}
	id := uuid.NewString()
	if _, _, err := s.IssuePersonal(ctx, "health", p.OfferID, p.ID, id, "Friend", "admin", now); err == nil {
		t.Fatal("injected persistence failure was ignored")
	}
	summary, err := s.Summary(ctx, "health", p.ID)
	if err != nil || summary.Available != 2 || summary.Requests != 0 {
		t.Fatalf("partial allocation %+v %v", summary, err)
	}
	s.Cipher = normal
	var wg sync.WaitGroup
	results := make(chan Request, 2)
	failures := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, r, err := s.IssuePersonal(ctx, "health", p.OfferID, p.ID, id, "Friend", "admin", now)
			results <- r
			failures <- err
		}()
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	requestID := ""
	for r := range results {
		if requestID != "" && requestID != r.ID {
			t.Fatal("concurrent retry assigned twice")
		}
		requestID = r.ID
	}
	summary, err = s.Summary(ctx, "health", p.ID)
	if err != nil || summary.Available != 1 || summary.Requests != 1 {
		t.Fatalf("retry duplicated assignment %+v %v", summary, err)
	}
}

type personalFailCipher struct{ Cipher }

func (c personalFailCipher) Encrypt(raw, aad []byte) ([]byte, []byte, error) {
	if bytes.Contains(aad, []byte("personal")) {
		return nil, nil, ErrInvalid
	}
	return c.Cipher.Encrypt(raw, aad)
}

func TestPersonalRejectsSharedSandboxInactiveAndWrongOfferPools(t *testing.T) {
	for _, kind := range []string{"custom", "sandbox", "inactive", "wrong-offer"} {
		t.Run(kind, func(t *testing.T) {
			s, p, now := fixture(t)
			ctx := context.Background()
			offer := p.OfferID
			p.ID = "unsupported-pool"
			switch kind {
			case "custom":
				p.Kind = "custom"
				p.Code = "SHAREDCODE"
			case "sandbox":
				p.Environment = "sandbox"
			case "inactive":
				p.Active = false
			case "wrong-offer":
				offer = "other"
			}
			if err := s.SyncPool(ctx, "health", p); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.IssuePersonal(ctx, "health", offer, p.ID, uuid.NewString(), "Friend", "admin", now); err == nil {
				t.Fatal("unsupported pool allocated")
			}
		})
	}
}

func TestAppleCodeExpiryUsesPacificMidnight(t *testing.T) {
	for _, v := range []struct{ day, hour string }{{"2026-09-09", "07:00"}, {"2026-12-09", "08:00"}} {
		got, err := AppleCodeExpiry(v.day)
		if err != nil || got.Format("15:04") != v.hour {
			t.Fatalf("expiry=%v err=%v", got, err)
		}
	}
}

func TestAppDeletionRemovesTransactionAssociationButPreservesOperatorLedger(t *testing.T) {
	s, p, now := fixture(t)
	ctx := context.Background()
	if err := s.ImportCodes(ctx, "health", p.ID, "admin", []string{"AAAA111", "BBBB222"}, now); err != nil {
		t.Fatal(err)
	}
	r, err := s.RecordExternal(ctx, "health", p.ID, "historic", "admin", Recipient{Name: "Operator-known friend"}, "AAAA111", now, now)
	if err != nil {
		t.Fatal(err)
	}
	original := "private-original-transaction"
	if _, err = s.DB.Exec(`INSERT INTO app_store_offer_redemptions(app_id,environment,transaction_hash,original_transaction_hash,offer_identifier,offer_type,redeemed_at,expires_at) VALUES('health','production',UNHEX(SHA2('tx',256)),UNHEX(SHA2(?,256)),'FRIENDS',3,?,?)`, original, now, now); err != nil {
		t.Fatal(err)
	}
	evidence, err := s.VerifiedSubscriptions(ctx, "health", p.ID)
	if err != nil || len(evidence) != 1 {
		t.Fatal(err)
	}
	r, err = s.LinkVerified(ctx, "health", r.ID, "admin", evidence[0].Reference, r.Version, now)
	if err != nil || r.VerifiedAt == nil {
		t.Fatal(err)
	}
	privacy := mysqlstore.NewPrivacyRepository(s.DB, "health")
	if err = privacy.DeletePrincipal(ctx, attestation.Principal{KeyID: "fixture", TransactionID: original}); err != nil {
		t.Fatal(err)
	}
	r, err = s.Get(ctx, "health", r.ID)
	if err != nil || r.VerifiedAt != nil || r.Recipient.Name != "Operator-known friend" || r.DeliveredAt == nil {
		t.Fatalf("incorrect deletion boundary %+v %v", r, err)
	}
	summary, err := s.Summary(ctx, "health", p.ID)
	if err != nil || summary.LinkedVerified != 0 || summary.Delivered != 1 {
		t.Fatalf("stale verified count %+v %v", summary, err)
	}
}

func TestPersonalConfirmImportAndAssignAreAtomicAndRetrySafe(t *testing.T) {
	s, p, now := fixture(t)
	ctx := context.Background()
	codes := []string{"AAAA111", "BBBB222"}
	id := uuid.NewString()
	broken := s
	broken.Cipher = personalFailCipher{s.Cipher}
	if _, _, err := broken.IssuePersonalWithUnissuedCodes(ctx, "health", p.OfferID, p.ID, id, "Friend", "admin", codes, now); err == nil {
		t.Fatal("expected transaction failure")
	}
	summary, err := s.Summary(ctx, "health", p.ID)
	if err != nil || summary.Imported != 0 {
		t.Fatalf("failed issuance imported codes %+v %v", summary, err)
	}
	_, r, err := s.IssuePersonalWithUnissuedCodes(ctx, "health", p.OfferID, p.ID, id, "Friend", "admin", codes, now)
	if err != nil {
		t.Fatal(err)
	}
	summary, err = s.Summary(ctx, "health", p.ID)
	if err != nil || summary.Available != 1 || summary.AssignedCodes != 1 || summary.External != 0 {
		t.Fatalf("one-step issuance %+v %v", summary, err)
	}
	// An export after issuance must not be undone by a lost-response retry.
	if _, err = s.DB.Exec(`UPDATE offer_delivery_codes SET inventory_state='external' WHERE app_id='health' AND pool_id=? AND inventory_state='available'`, p.ID); err != nil {
		t.Fatal(err)
	}
	_, retry, err := s.IssuePersonalWithUnissuedCodes(ctx, "health", p.OfferID, p.ID, id, "Friend", "admin", codes, now)
	if err != nil || retry.ID != r.ID {
		t.Fatal("retry", err)
	}
	summary, _ = s.Summary(ctx, "health", p.ID)
	if summary.Available != 0 || summary.External != 1 || summary.AssignedCodes != 1 {
		t.Fatalf("retry revived exported stock %+v", summary)
	}
}

func TestPersonalFreshImportIsReadyWithoutRevivingExistingCodes(t *testing.T) {
	s, p, now := fixture(t)
	ctx := context.Background()
	codes := []string{"AAAA111", "BBBB222"}
	if err := s.ImportFreshCodes(ctx, "health", p.ID, "admin", codes, now); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.IssuePersonal(ctx, "health", p.OfferID, p.ID, uuid.NewString(), "Friend", "admin", now)
	if err != nil {
		t.Fatal("fresh batch not immediately usable", err)
	}
	if _, err = s.DB.Exec(`UPDATE offer_delivery_codes SET inventory_state='external' WHERE app_id='health' AND pool_id=? AND inventory_state='available'`, p.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.ImportFreshCodes(ctx, "health", p.ID, "admin", codes, now); err != nil {
		t.Fatal(err)
	}
	summary, _ := s.Summary(ctx, "health", p.ID)
	if summary.Available != 0 || summary.External != 1 || summary.AssignedCodes != 1 {
		t.Fatalf("reimport revived old codes %+v", summary)
	}
}
