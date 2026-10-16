//go:build ignore

// Explicit Sandbox notification probe. No purchases or production API calls.
package main

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/tellyouwhat/backend/internal/appstore"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func privateFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 1<<20 {
		return nil, errors.New("private probe file missing or insecure")
	}
	return os.ReadFile(path)
}

func run() error {
	if len(os.Args) != 2 || (os.Args[1] != "request" && os.Args[1] != "status") {
		return errors.New("use request or status")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	root := filepath.Join(home, ".config/tellyouwhat/albums-test")
	data, err := privateFile(filepath.Join(root, "album-app-store.p8"))
	if err != nil {
		return err
	}
	key, err := appstore.ParseSigningKeyPEM(data)
	if err != nil {
		return errors.New("invalid probe signing key")
	}
	now := time.Now()
	token := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{"iss": "1f4f7f07-26b1-4226-9518-5bd19f8440b8", "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(), "aud": "appstoreconnect-v1", "bid": "cn.tellyouwhat.albums"})
	token.Header["kid"] = "YTX5GYG2Z5"
	bearer, err := token.SignedString(key)
	if err != nil {
		return errors.New("could not sign probe request")
	}
	directory := filepath.Join(root, "apple-notification-20261013")
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	endpoint := "https://api.storekit-sandbox.apple.com/inApps/v1/notifications/test"
	method := http.MethodPost
	if os.Args[1] == "request" {
		marker, err := os.OpenFile(filepath.Join(directory, "request-started"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return errors.New("request already started; inspect its outcome instead of repeating POST")
		}
		marker.Close()
	} else {
		saved, err := privateFile(filepath.Join(directory, "request.json"))
		if err != nil {
			return err
		}
		var value struct {
			Token string `json:"testNotificationToken"`
		}
		if json.Unmarshal(saved, &value) != nil {
			return errors.New("invalid saved token")
		}
		// Apple returns an opaque token, not necessarily a UUID.
		if len(value.Token) == 0 || len(value.Token) > 4096 {
			return errors.New("invalid saved token")
		}
		endpoint += "/" + url.PathEscape(value.Token)
		method = http.MethodGet
	}
	request, err := http.NewRequest(method, endpoint, nil)
	if err != nil {
		return errors.New("invalid probe request")
	}
	request.Header.Set("Authorization", "Bearer "+bearer)
	request.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect refused") }}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("Apple probe network failure; request outcome may be unknown")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return errors.New("invalid Apple response size")
	}
	if response.StatusCode != 200 && response.StatusCode != 202 {
		return fmt.Errorf("Apple probe HTTP %d", response.StatusCode)
	}
	if !json.Valid(body) {
		return errors.New("invalid Apple JSON")
	}
	if err := os.WriteFile(filepath.Join(directory, os.Args[1]+".json"), body, 0600); err != nil {
		return err
	}
	if os.Args[1] == "request" {
		fmt.Println("Apple accepted the Sandbox TEST request; token saved privately")
		return nil
	}
	var result struct {
		Signed   string `json:"signedPayload"`
		Attempts []struct {
			Date   int64  `json:"attemptDate"`
			Result string `json:"sendAttemptResult"`
		} `json:"sendAttempts"`
	}
	if json.Unmarshal(body, &result) != nil {
		return errors.New("invalid notification status")
	}
	rootsData, err := privateFile(filepath.Join(root, "apple-app-store-roots.pem"))
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(rootsData) {
		return errors.New("invalid Apple trust roots")
	}
	verifier := appstore.NewTransactionVerifier(appstore.VerifierConfig{Roots: roots, BundleID: "cn.tellyouwhat.albums", AppAppleID: 6822133304, Environment: "Sandbox"})
	verified, err := verifier.VerifyNotification(result.Signed)
	if err != nil || verified.NotificationType != "TEST" {
		return errors.New("Apple TEST signature or app binding invalid")
	}
	digest := sha256.Sum256([]byte(result.Signed))
	evidence := map[string]any{"http_status": response.StatusCode, "notification_id": verified.NotificationUUID, "environment": verified.Environment, "type": verified.NotificationType, "signed_at": verified.SignedAt, "signed_payload_sha256": hex.EncodeToString(digest[:]), "attempts": result.Attempts}
	public, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(directory, "evidence.json"), append(public, '\n'), 0600); err != nil {
		return err
	}
	fmt.Println(string(public))
	return nil
}
