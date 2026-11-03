package attestation

import "errors"

// Numeric stages prevent request-derived strings from entering diagnostic logs.
type failureStage uint8

const (
	stageUnknown failureStage = iota
	stageDependencies
	stageProofHeaders
	stageTimestamp
	stageKeyLookup
	stageEnvironment
	stageNonce
	stageNonceIssue
	stageAssertionEncoding
	stageRequestDigest
	stageAssertion
	stageAssertionCBOR
	stageAuthenticatorData
	stageSignatureEncoding
	stageRPID
	stageExtensions
	stagePublicKey
	stageSignature
	stageCounter
	stageCounterUpdate
	stageEnrollmentPolicy
	stageAttestationEncoding
	stageAttestationVerification
	stageRegistrationStorage
	stageRegistrationIdentity
)

var stageNames = [...]string{
	stageUnknown:                 "unknown",
	stageDependencies:            "dependencies",
	stageProofHeaders:            "proof_headers",
	stageTimestamp:               "timestamp",
	stageKeyLookup:               "key_lookup",
	stageEnvironment:             "environment",
	stageNonce:                   "nonce",
	stageNonceIssue:              "nonce_issue",
	stageAssertionEncoding:       "assertion_encoding",
	stageRequestDigest:           "request_digest",
	stageAssertion:               "assertion",
	stageAssertionCBOR:           "assertion_cbor",
	stageAuthenticatorData:       "authenticator_data",
	stageSignatureEncoding:       "signature_encoding",
	stageRPID:                    "rp_id",
	stageExtensions:              "extensions",
	stagePublicKey:               "public_key",
	stageSignature:               "signature",
	stageCounter:                 "counter",
	stageCounterUpdate:           "counter_update",
	stageEnrollmentPolicy:        "enrollment_policy",
	stageAttestationEncoding:     "attestation_encoding",
	stageAttestationVerification: "attestation_verification",
	stageRegistrationStorage:     "registration_storage",
	stageRegistrationIdentity:    "registration_identity",
}

// Unwrap preserves authentication, replay and availability error contracts.
type authenticationFailure struct {
	stage failureStage
	cause error
}

func (failure *authenticationFailure) Error() string { return failure.cause.Error() }
func (failure *authenticationFailure) Unwrap() error { return failure.cause }

func diagnosticFailure(stage failureStage, cause error) error {
	return &authenticationFailure{stage: stage, cause: cause}
}

func failureStageOf(err error) failureStage {
	var failure *authenticationFailure
	if !errors.As(err, &failure) || failure == nil || int(failure.stage) >= len(stageNames) {
		return stageUnknown
	}
	return failure.stage
}

// FailureStage returns only server-defined labels, never proof data or causes.
func FailureStage(err error) string { return stageNames[failureStageOf(err)] }
