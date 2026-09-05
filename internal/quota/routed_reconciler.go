package quota

import (
	"context"
	"strings"
	"time"
)

const FreeRecognitionTransactionPrefix = "free:"

type RoutedTokenReconciler struct {
	managed         TokenReconciler
	freeRecognition TokenReconciler
}

func NewRoutedTokenReconciler(managed, freeRecognition TokenReconciler) *RoutedTokenReconciler {
	return &RoutedTokenReconciler{managed: managed, freeRecognition: freeRecognition}
}

func (reconciler *RoutedTokenReconciler) Reconcile(
	ctx context.Context,
	transactionID, reservationID string,
	reserved,
	actual int,
	now time.Time,
) error {
	target := reconciler.managed
	if strings.HasPrefix(transactionID, FreeRecognitionTransactionPrefix) {
		target = reconciler.freeRecognition
	}
	if target == nil {
		return ErrInvalidIdentity
	}
	return target.Reconcile(ctx, transactionID, reservationID, reserved, actual, now)
}

var _ TokenReconciler = (*RoutedTokenReconciler)(nil)
