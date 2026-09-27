package mysqlstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"testing"

	"github.com/tellyouwhat/backend/internal/journal/illustration"
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

func TestIllustrationResultBindsRequestAndImage(t *testing.T) {
	data := []byte("image fixture")
	hash := sha256.Sum256(data)
	task := illustration.Task{AppID: "journal", OwnerID: "owner", ID: "task", RequestDigest: "request-hash", ResultDigest: hex.EncodeToString(hash[:])}
	cipher, _ := NewPayloadCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32)))
	encrypted, nonce, err := cipher.Encrypt(data, illustrationResultAAD(task))
	if err != nil {
		t.Fatal(err)
	}
	if !resultMatches(task, illustration.Result{Image: data}) || resultMatches(task, illustration.Result{Image: []byte("changed")}) {
		t.Fatal("image digest not enforced")
	}
	for _, field := range []string{"app", "owner", "task", "request", "image"} {
		changed := task
		switch field {
		case "app":
			changed.AppID = "other"
		case "owner":
			changed.OwnerID = "other"
		case "task":
			changed.ID = "other"
		case "request":
			changed.RequestDigest = "other"
		case "image":
			changed.ResultDigest = "other"
		}
		if _, err := cipher.Decrypt(encrypted, nonce, illustrationResultAAD(changed)); err == nil {
			t.Fatalf("unbound %s", field)
		}
	}
	if _, err := cipher.Decrypt(encrypted, nonce, illustrationAAD(task.AppID, task.OwnerID, task.ID)); err == nil {
		t.Fatal("request/result ciphertext substitution")
	}
}
