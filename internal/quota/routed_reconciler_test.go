package quota

import (
	"context"
	"testing"
	"time"
)

type recordingReconciler struct {
	transactionID string
}

func (value *recordingReconciler) Reconcile(_ context.Context, transactionID string, _, _ int, _ time.Time) error {
	value.transactionID = transactionID
	return nil
}

func TestRoutedTokenReconcilerSeparatesFreeRecognition(t *testing.T) {
	managed := &recordingReconciler{}
	free := &recordingReconciler{}
	reconciler := NewRoutedTokenReconciler(managed, free)
	if err := reconciler.Reconcile(context.Background(), "transaction-1", 10, 5, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.Reconcile(context.Background(), "free:key-1", 10, 5, time.Now()); err != nil {
		t.Fatal(err)
	}
	if managed.transactionID != "transaction-1" || free.transactionID != "free:key-1" {
		t.Fatalf("unexpected routing: managed=%q free=%q", managed.transactionID, free.transactionID)
	}
}
