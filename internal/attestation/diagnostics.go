package attestation

import "errors"

// authenticationFailure carries a bounded diagnostic stage, never proof data.
// Unwrap preserves the authentication, replay and availability error contracts.
type authenticationFailure struct {
	stage string
	cause error
}

func (failure *authenticationFailure) Error() string { return failure.cause.Error() }
func (failure *authenticationFailure) Unwrap() error { return failure.cause }

func diagnosticFailure(stage string, cause error) error {
	return &authenticationFailure{stage: stage, cause: cause}
}

// FailureStage returns only server-defined stages suitable for request logs.
func FailureStage(err error) string {
	var failure *authenticationFailure
	if !errors.As(err, &failure) {
		return "unknown"
	}
	switch failure.stage {
	case "dependencies", "proof_headers", "timestamp", "key_lookup", "environment",
		"nonce", "assertion_encoding", "request_digest", "assertion", "assertion_cbor",
		"authenticator_data", "signature_encoding", "rp_id", "extensions", "public_key",
		"signature", "counter", "counter_update":
		return failure.stage
	default:
		return "unknown"
	}
}
