package mysqlstore

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func TestIllustrationPayloadCannotMoveBetweenOwnersOrTasks(t *testing.T) {
	cipher, err := NewPayloadCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	aad := illustrationAAD("journal", "owner", "request")
	plain := []byte(`{"Prompt":"private description"}`)
	sealed, nonce, err := cipher.Encrypt(plain, aad)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, plain) {
		t.Fatal("unencrypted payload")
	}
	recovered, err := cipher.Decrypt(sealed, nonce, aad)
	if err != nil || !bytes.Equal(plain, recovered) {
		t.Fatal("roundtrip failed")
	}
	for _, other := range [][]byte{illustrationAAD("health", "owner", "request"), illustrationAAD("journal", "other", "request"), illustrationAAD("journal", "owner", "other")} {
		if _, err := cipher.Decrypt(sealed, nonce, other); err == nil {
			t.Fatal("ciphertext relocation accepted")
		}
	}
	if bytes.Equal(illustrationAAD("a:b", "c", "d"), illustrationAAD("a", "b:c", "d")) {
		t.Fatal("ambiguous AAD encoding")
	}
}
