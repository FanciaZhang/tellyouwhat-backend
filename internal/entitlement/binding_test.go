package entitlement

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/attestation"
)

func TestVerifiedBindingTransitionPolicy(t *testing.T) {
	for _, test := range []struct {
		name, bound, existingID, existingEnv, incomingID, incomingEnv string
		allowed                                                       bool
	}{
		{"first purchase", "", "", "", "paid", "production", true},
		{"first sandbox", "", "", "", "test", "sandbox", true},
		{"renewal", "paid", "paid", "production", "paid", "production", true},
		{"sandbox to production", "test", "test", "sandbox", "paid", "production", true},
		{"production to sandbox", "paid", "paid", "production", "test", "sandbox", true},
		{"different production purchase", "paid", "paid", "production", "other", "production", false},
		{"different sandbox purchase", "test", "test", "sandbox", "other", "sandbox", false},
		{"missing previous evidence", "test", "", "", "paid", "production", false},
		{"mismatched previous evidence", "test", "other", "sandbox", "paid", "production", false},
		{"invalid environment", "", "", "", "paid", "unverified", false},
		{"empty transaction", "", "", "", "", "production", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, got := BindVerifiedTransaction(test.bound, Record{TransactionID: test.existingID, Environment: test.existingEnv}, Record{TransactionID: test.incomingID, Environment: test.incomingEnv}, VerifiedTransactionBindings{})
			if got != test.allowed {
				t.Fatalf("allowed=%v, want %v", got, test.allowed)
			}
		})
	}
}

type atomicVerifiedStore struct {
	*MemoryStore
	calls  int
	err    error
	record Record
}

func (store *atomicVerifiedStore) UpsertVerified(_ context.Context, record Record) error {
	store.calls++
	store.record = record
	return store.err
}

func TestProductionSyncUsesAtomicStoreOnlyAfterVerification(t *testing.T) {
	now := time.Now()
	for _, test := range []struct {
		name       string
		resolveErr error
		expiry     time.Time
		storageErr error
		calls      int
	}{
		{"active", nil, now.Add(time.Hour), nil, 1},
		{"unverified", ErrProductionSyncDenied, now.Add(time.Hour), nil, 0},
		{"expired", nil, now.Add(-time.Hour), nil, 0},
		{"conflict", nil, now.Add(time.Hour), ErrSubscriptionBindingConflict, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &atomicVerifiedStore{MemoryStore: NewMemoryStore(), err: test.storageErr}
			service := NewProductionService(store, &fakeSubscriptionResolver{state: SubscriptionState{OriginalTransactionID: "paid", Environment: "Production", ExpiresAt: test.expiry}, err: test.resolveErr}, func() time.Time { return now })
			_, err := service.Sync(context.Background(), attestation.Principal{KeyID: "device"}, "signed")
			if store.calls != test.calls {
				t.Fatalf("writes=%d", store.calls)
			}
			if test.storageErr != nil && !errors.Is(err, test.storageErr) {
				t.Fatalf("lost binding error: %v", err)
			}
			if test.name == "active" && (err != nil || store.record.Environment != "production") {
				t.Fatalf("active sync: %v", err)
			}
		})
	}
}

func TestVerifiedBindingRetainsBothEnvironments(t *testing.T) {
	for _, first := range []string{"sandbox", "production"} {
		t.Run(first, func(t *testing.T) {
			var bound string
			var existing Record
			var bindings VerifiedTransactionBindings
			other := "production"
			if first == other {
				other = "sandbox"
			}
			for _, env := range []string{first, other, first, other} {
				incoming := Record{TransactionID: env + "-purchase", Environment: env}
				next, allowed := BindVerifiedTransaction(bound, existing, incoming, bindings)
				if !allowed {
					t.Fatalf("switch to %s rejected", env)
				}
				bindings, bound, existing = next, incoming.TransactionID, incoming
			}
			for _, env := range []string{"production", "sandbox"} {
				if _, allowed := BindVerifiedTransaction(bound, existing, Record{TransactionID: "other", Environment: env}, bindings); allowed {
					t.Fatalf("switching environments replaced %s purchase", env)
				}
			}
		})
	}
	if _, allowed := BindVerifiedTransaction("paid", Record{TransactionID: "paid", Environment: "production"}, Record{TransactionID: "test", Environment: "sandbox"}, VerifiedTransactionBindings{ProductionID: "other"}); allowed {
		t.Fatal("inconsistent stored anchor accepted")
	}
}
