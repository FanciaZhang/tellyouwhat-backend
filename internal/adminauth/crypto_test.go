package adminauth

import "testing"

func TestRandomTokensAndUUIDsHaveExpectedShape(t *testing.T) {
	token, err := RandomToken(32)
	if err != nil || len(token) != 43 {
		t.Fatalf("token = %q, err = %v", token, err)
	}
	identifier, err := NewUUID()
	if err != nil || len(identifier) != 36 || identifier[14] != '4' {
		t.Fatalf("uuid = %q, err = %v", identifier, err)
	}
	if TokenHash(token) == TokenHash(token+"x") {
		t.Fatal("different tokens produced the same test hash")
	}
}
