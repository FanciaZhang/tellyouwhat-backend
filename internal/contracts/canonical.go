package contracts

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

type RequestBinding struct {
	Method     string
	Path       string
	RequestID  string
	Nonce      string
	Timestamp  string
	BodySHA256 string
}

func RequestBindingDigest(binding RequestBinding) string {
	canonical := strings.Join([]string{
		strings.ToUpper(binding.Method),
		binding.Path,
		binding.RequestID,
		binding.Nonce,
		binding.Timestamp,
		strings.ToLower(binding.BodySHA256),
	}, "\n")
	digest := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(digest[:])
}

func BodySHA256(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

func MediaDigest(media []Media) (string, error) {
	encoded, err := json.Marshal(media)
	if err != nil {
		return "", err
	}
	return BodySHA256(encoded), nil
}
