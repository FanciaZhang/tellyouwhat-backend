package adminauth

import (
	"strings"
	"testing"
)

func TestRecoveryCodesRoundTripAndNormalize(t *testing.T) {
	plain, hashes, err := NewRecoveryCodes(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(plain) != 10 || len(hashes) != 10 {
		t.Fatalf("unexpected recovery code count: %d/%d", len(plain), len(hashes))
	}
	seen := map[string]bool{}
	for index, code := range plain {
		if seen[code] || !strings.HasPrefix(code, "HADM-") {
			t.Fatalf("invalid or duplicate code: %q", code)
		}
		seen[code] = true
		if !VerifyRecoveryCode(strings.ToLower(code), hashes[index]) {
			t.Fatalf("code %d did not verify", index)
		}
		if VerifyRecoveryCode(code+"X", hashes[index]) {
			t.Fatalf("modified code %d verified", index)
		}
	}
}

func TestRecoveryCodeRejectsMalformedHash(t *testing.T) {
	for _, encoded := range []string{"", "$argon2id$", "$argon2id$v=19$m=1,t=1,p=1$bad$bad"} {
		if VerifyRecoveryCode("HADM-AAAA-AAAA-AAAA-AAAA", encoded) {
			t.Fatalf("malformed hash verified: %q", encoded)
		}
	}
}
