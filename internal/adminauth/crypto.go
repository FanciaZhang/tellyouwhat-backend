package adminauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

const recoveryCodePrefix = "HADM"

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

func NewRecoveryCodes(count int) ([]string, []string, error) {
	if count <= 0 || count > 20 {
		return nil, nil, errors.New("invalid recovery code count")
	}
	plain := make([]string, 0, count)
	hashes := make([]string, 0, count)
	for range count {
		value := make([]byte, 10)
		if _, err := rand.Read(value); err != nil {
			return nil, nil, err
		}
		encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(value)
		code := recoveryCodePrefix + "-" + encoded[0:4] + "-" + encoded[4:8] + "-" + encoded[8:12] + "-" + encoded[12:16]
		hash, err := HashRecoveryCode(code)
		if err != nil {
			return nil, nil, err
		}
		plain = append(plain, code)
		hashes = append(hashes, hash)
	}
	return plain, hashes, nil
}

func HashRecoveryCode(code string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	digest := argon2.IDKey([]byte(normalizeRecoveryCode(code)), salt, 3, 64*1024, 2, 32)
	return fmt.Sprintf("$argon2id$v=19$m=65536,t=3,p=2$%s$%s",
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(digest)), nil
}

func VerifyRecoveryCode(code, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" || parts[3] != "m=65536,t=3,p=2" {
		return false
	}
	salt, saltErr := base64.RawStdEncoding.DecodeString(parts[4])
	expected, digestErr := base64.RawStdEncoding.DecodeString(parts[5])
	if saltErr != nil || digestErr != nil || len(salt) != 16 || len(expected) != 32 {
		return false
	}
	actual := argon2.IDKey([]byte(normalizeRecoveryCode(code)), salt, 3, 64*1024, 2, 32)
	return subtleEqual(actual, expected)
}

func normalizeRecoveryCode(code string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(code), " ", ""))
}

func subtleEqual(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	var difference byte
	for index := range left {
		difference |= left[index] ^ right[index]
	}
	return difference == 0
}
