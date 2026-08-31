package mysqlstore

import (
	"encoding/base64"
	"testing"
)

func TestPayloadCipherBindsCiphertextToJobID(t *testing.T) {
	t.Parallel()

	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	cipher, err := NewPayloadCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, nonce, err := cipher.Encrypt([]byte("sensitive prompt"), []byte("job-1"))
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := cipher.Decrypt(ciphertext, nonce, []byte("job-1"))
	if err != nil || string(plaintext) != "sensitive prompt" {
		t.Fatalf("decrypt: %q, %v", plaintext, err)
	}
	if _, err := cipher.Decrypt(ciphertext, nonce, []byte("job-2")); err == nil {
		t.Fatal("ciphertext must not be movable between jobs")
	}
}
