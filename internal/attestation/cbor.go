package attestation

import "github.com/fxamacker/cbor/v2"

// A single immutable decoding policy rejects ambiguous map keys in both
// registration objects and request assertions. Unknown extensions remain signed.
var proofCBOR = func() cbor.DecMode {
	mode, err := (cbor.DecOptions{DupMapKey: cbor.DupMapKeyEnforcedAPF}).DecMode()
	if err != nil {
		panic(err) // Invalid static decoder configuration is a startup failure.
	}
	return mode
}()
