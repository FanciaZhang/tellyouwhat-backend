package privacy

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"

	"github.com/tellyouwhat/backend/internal/attestation"
)

type DeletionReceipt [sha256.Size]byte

var ErrInvalidDeletionReceipt = errors.New("invalid privacy deletion receipt")

// NewDeletionReceipt binds completion to one application and one signed DELETE.
// It contains no raw identity, assertion, transaction or private content.
func NewDeletionReceipt(appID string, proof attestation.RequestProof) (DeletionReceipt, error) {
	if appID == "" || proof.Method != "DELETE" || proof.Path != "/v1/privacy/data" {
		return DeletionReceipt{}, ErrInvalidDeletionReceipt
	}
	values := []string{"privacy-deletion-receipt-v1", appID, proof.Method, proof.Path,
		proof.RequestID, proof.KeyID, proof.Assertion, proof.Nonce, proof.Timestamp, proof.BodySHA256}
	hash := sha256.New()
	for _, value := range values {
		if value == "" {
			return DeletionReceipt{}, ErrInvalidDeletionReceipt
		}
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		hash.Write(size[:])
		hash.Write([]byte(value))
	}
	var receipt DeletionReceipt
	copy(receipt[:], hash.Sum(nil))
	return receipt, nil
}

func (service *Service) DeletionCompleted(ctx context.Context, receipt DeletionReceipt) (bool, error) {
	if service == nil || service.repository == nil || receipt == (DeletionReceipt{}) {
		return false, ErrInvalidDeletionReceipt
	}
	return service.repository.DeletionCompleted(ctx, receipt)
}

func (service *Service) DeletePrincipalWithReceipt(ctx context.Context, principal attestation.Principal, receipt DeletionReceipt) error {
	if receipt == (DeletionReceipt{}) {
		return ErrInvalidDeletionReceipt
	}
	return service.deletePrincipal(ctx, principal, &receipt)
}
