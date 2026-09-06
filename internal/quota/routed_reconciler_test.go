package quota

import (
	"context"
	"testing"
	"time"
)

type recordingReconciler struct {
	transactionID string
	reservationID string
}

func (value *recordingReconciler) Reconcile(_ context.Context, transactionID, reservationID string, _, _ int, _ time.Time) error {
	value.transactionID = transactionID
	value.reservationID = reservationID
	return nil
}

func TestRoutedTokenReconcilerSeparatesFreeRecognition(t *testing.T) {
	managed := &recordingReconciler{}
	free := &recordingReconciler{}
	reconciler := NewRoutedTokenReconciler(managed, free)
	if err := reconciler.Reconcile(context.Background(), "transaction-1", "paid-reservation", 10, 5, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.Reconcile(context.Background(), "free:key-1", "free-reservation", 10, 5, time.Now()); err != nil {
		t.Fatal(err)
	}
	if managed.transactionID != "transaction-1" || free.transactionID != "free:key-1" {
		t.Fatalf("unexpected routing: managed=%q free=%q", managed.transactionID, free.transactionID)
	}
	if managed.reservationID != "paid-reservation" || free.reservationID != "free-reservation" {
		t.Fatalf("reservation identities were not preserved across billing routes")
	}
}
