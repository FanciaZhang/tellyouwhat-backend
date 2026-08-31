// Created by OpenAI Codex on 2026-08-03.

package appstore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const notificationVersionV2 = "2.0"

type notificationPayload struct {
	NotificationType string           `json:"notificationType"`
	NotificationUUID string           `json:"notificationUUID"`
	Version          string           `json:"version"`
	SignedDate       int64            `json:"signedDate"`
	Data             notificationData `json:"data"`
}

type notificationData struct {
	AppAppleID            int64  `json:"appAppleId"`
	BundleID              string `json:"bundleId"`
	Environment           string `json:"environment"`
	Status                int    `json:"status"`
	SignedTransactionInfo string `json:"signedTransactionInfo"`
	SignedRenewalInfo     string `json:"signedRenewalInfo"`
}

type Notification struct {
	NotificationUUID      string
	NotificationType      string
	Environment           string
	SignedAt              time.Time
	SignedTransactionInfo string
}

type NotificationResult struct {
	NotificationUUID      string
	OriginalTransactionID string
	Environment           string
	TransactionID         string
	OfferIdentifier       string
	OfferType             int32
	SignedAt              time.Time
	ExpiresAt             time.Time
	Active                bool
}

type NotificationProcessor struct {
	verifier *TransactionVerifier
	resolver *SubscriptionResolver
}

func NewNotificationProcessor(
	verifier *TransactionVerifier,
	resolver *SubscriptionResolver,
) *NotificationProcessor {
	return &NotificationProcessor{verifier: verifier, resolver: resolver}
}

func (verifier *TransactionVerifier) VerifyNotification(signedData string) (Notification, error) {
	if verifier == nil || verifier.config.Roots == nil || verifier.config.BundleID == "" ||
		len(signedData) == 0 || len(signedData) > maximumSignedDataBytes {
		return Notification{}, ErrInvalidSignedData
	}
	header, payload, segments, err := decodeNotificationJWS(signedData)
	if err != nil {
		return Notification{}, ErrInvalidSignedData
	}
	certificates, err := verifier.verifyCertificateChain(header, payload.SignedDate)
	if err != nil {
		return Notification{}, ErrInvalidSignedData
	}
	if err := verifyJWSSignature(certificates[0], segments); err != nil {
		return Notification{}, ErrInvalidSignedData
	}
	if payload.NotificationUUID == "" || len(payload.NotificationUUID) > 128 ||
		payload.Version != notificationVersionV2 || payload.Data.BundleID != verifier.config.BundleID ||
		!strings.EqualFold(payload.Data.Environment, verifier.config.Environment) ||
		(strings.EqualFold(verifier.config.Environment, "Production") &&
			payload.Data.AppAppleID != verifier.config.AppAppleID) ||
		time.UnixMilli(payload.SignedDate).After(verifier.config.Now().Add(5*time.Minute)) {
		return Notification{}, ErrInvalidSignedData
	}
	if payload.NotificationType != "TEST" &&
		(payload.Data.Status < 1 || payload.Data.Status > 5 || payload.Data.SignedTransactionInfo == "") {
		return Notification{}, ErrInvalidSignedData
	}
	return Notification{
		NotificationUUID:      payload.NotificationUUID,
		NotificationType:      payload.NotificationType,
		Environment:           payload.Data.Environment,
		SignedAt:              time.UnixMilli(payload.SignedDate),
		SignedTransactionInfo: payload.Data.SignedTransactionInfo,
	}, nil
}

func (processor *NotificationProcessor) Process(
	ctx context.Context,
	signedPayload string,
) (NotificationResult, error) {
	if processor == nil || processor.verifier == nil || processor.resolver == nil {
		return NotificationResult{}, ErrStatusUnavailable
	}
	notification, err := processor.verifier.VerifyNotification(signedPayload)
	if err != nil {
		return NotificationResult{}, err
	}
	if notification.NotificationType == "TEST" {
		return NotificationResult{
			NotificationUUID:      notification.NotificationUUID,
			OriginalTransactionID: "test:" + notification.NotificationUUID,
			Environment:           notification.Environment,
			ExpiresAt:             notification.SignedAt,
			Active:                false,
		}, nil
	}
	transaction, err := processor.verifier.VerifyTransaction(notification.SignedTransactionInfo)
	if err != nil {
		return NotificationResult{}, err
	}
	state, err := processor.resolver.Resolve(ctx, notification.SignedTransactionInfo)
	if err != nil {
		if errors.Is(err, ErrSubscriptionInactive) || errors.Is(err, ErrTransactionNotFound) {
			expiresAt := transaction.ExpiresAt
			if now := processor.resolver.now(); now.Before(expiresAt) {
				expiresAt = now
			}
			return NotificationResult{
				NotificationUUID:      notification.NotificationUUID,
				OriginalTransactionID: transaction.OriginalTransactionID,
				Environment:           transaction.Environment,
				TransactionID:         transaction.TransactionID,
				OfferIdentifier:       transaction.OfferIdentifier,
				OfferType:             transaction.OfferType,
				SignedAt:              transaction.SignedAt,
				ExpiresAt:             expiresAt,
				Active:                false,
			}, nil
		}
		return NotificationResult{}, err
	}
	return NotificationResult{
		NotificationUUID:      notification.NotificationUUID,
		OriginalTransactionID: state.OriginalTransactionID,
		Environment:           state.Environment,
		TransactionID:         state.TransactionID,
		OfferIdentifier:       state.OfferIdentifier,
		OfferType:             state.OfferType,
		SignedAt:              state.SignedAt,
		ExpiresAt:             state.ExpiresAt,
		Active:                true,
	}, nil
}

func decodeNotificationJWS(signedData string) (jwsHeader, notificationPayload, []string, error) {
	segments := strings.Split(signedData, ".")
	if len(segments) != expectedJWSSegments {
		return jwsHeader{}, notificationPayload{}, nil, ErrInvalidSignedData
	}
	headerData, err := base64.RawURLEncoding.DecodeString(segments[0])
	if err != nil {
		return jwsHeader{}, notificationPayload{}, nil, err
	}
	payloadData, err := base64.RawURLEncoding.DecodeString(segments[1])
	if err != nil {
		return jwsHeader{}, notificationPayload{}, nil, err
	}
	var header jwsHeader
	if err := json.Unmarshal(headerData, &header); err != nil {
		return jwsHeader{}, notificationPayload{}, nil, err
	}
	var payload notificationPayload
	if err := json.Unmarshal(payloadData, &payload); err != nil {
		return jwsHeader{}, notificationPayload{}, nil, err
	}
	return header, payload, segments, nil
}
