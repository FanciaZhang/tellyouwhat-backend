package adminauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
)

func RandomToken(bytes int) (string, error) {
	value, err := RandomBytes(bytes)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func RandomBytes(count int) ([]byte, error) {
	if count <= 0 || count > 1024 {
		return nil, errors.New("invalid random byte count")
	}
	value := make([]byte, count)
	if _, err := rand.Read(value); err != nil {
		return nil, err
	}
	return value, nil
}

func TokenHash(token string) [32]byte { return sha256.Sum256([]byte(token)) }

func NewUUID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value)
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32], nil
}
