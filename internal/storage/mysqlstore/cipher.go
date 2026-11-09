package mysqlstore

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
)

type PayloadCipher struct{ aead cipher.AEAD }

func NewPayloadCipher(base64Key string) (*PayloadCipher, error) {
	key, err := base64.StdEncoding.DecodeString(base64Key)
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, errors.New("payload encryption key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &PayloadCipher{aead: aead}, nil
}

func (value *PayloadCipher) Encrypt(plaintext, associatedData []byte) ([]byte, []byte, error) {
	nonce := make([]byte, value.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, err
	}
	return value.aead.Seal(nil, nonce, plaintext, associatedData), nonce, nil
}

func (value *PayloadCipher) Decrypt(ciphertext, nonce, associatedData []byte) ([]byte, error) {
	return value.aead.Open(nil, nonce, ciphertext, associatedData)
}
