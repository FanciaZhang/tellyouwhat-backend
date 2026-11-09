package appledelivery

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
	"syscall"
	"time"
)

const MaxJournalConnectionTTL = 15 * time.Minute
const maxConnectionBytes = 1024
const connectionKeyName = ".journal-connection-key"

var journalTokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43,128}$`)
var journalVoicePattern = regexp.MustCompile(`^journal-voice-v[1-9][0-9]*$`)
var sealedConnectionPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
var errConnectionExpired = errors.New("private connection link expired")

func normalizeConnection(data []byte) ([]byte, error) {
	var value struct {
		Version         int    `json:"version"`
		Origin          string `json:"origin"`
		ServiceProtocol string `json:"serviceProtocol"`
		VoiceProtocol   string `json:"voiceProtocol"`
		Token           string `json:"token"`
	}
	if len(data) > maxConnectionBytes || decode(bytes.NewReader(data), &value) != nil || value.Version != 1 ||
		value.Origin != "https://api-test.journal.tellyouwhat.cn" || value.ServiceProtocol != "subscription-v2" ||
		!journalVoicePattern.MatchString(value.VoiceProtocol) || !journalTokenPattern.MatchString(value.Token) {
		return nil, errors.New("invalid private Journal connection")
	}
	// A canonical object gives the installer and Swift decoder identical field values.
	return json.Marshal(value)
}

func ReadJournalConnection(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("Journal connection must be an owned private regular file")
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(owner.Uid) != os.Geteuid() {
		return nil, errors.New("Journal connection belongs to another operator")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot read private Journal connection")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxConnectionBytes+1))
	if err != nil {
		return nil, errors.New("cannot read private Journal connection")
	}
	return normalizeConnection(data)
}

type sealedConnection struct {
	Source  string          `json:"source"`
	Digest  string          `json:"digest"`
	Created time.Time       `json:"created_at"`
	Expires time.Time       `json:"expires_at"`
	Profile json.RawMessage `json:"profile"`
}

func (s *Store) connectionCipher(create bool) (cipher.AEAD, error) {
	if _, err := s.root.Lstat(connectionKeyName); errors.Is(err, os.ErrNotExist) && create {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		f, err := s.root.OpenFile(connectionKeyName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return nil, err
		}
		_, writeErr := f.Write(key)
		syncErr, closeErr := f.Sync(), f.Close()
		if writeErr != nil || syncErr != nil || closeErr != nil {
			if err := s.root.Remove(connectionKeyName); err != nil {
				return nil, errors.New("cannot remove incomplete connection key")
			}
			return nil, errors.New("cannot save private connection key")
		}
	}
	f, err := s.regular(connectionKeyName)
	if err != nil {
		return nil, errors.New("private connection key unavailable")
	}
	defer f.Close()
	key, err := io.ReadAll(io.LimitReader(f, 33))
	if err != nil || len(key) != 32 {
		return nil, errors.New("private connection key is invalid")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (s *Store) JournalConnection(c Candidate, data []byte, origin string, ttl time.Duration, now time.Time) (Receipt, error) {
	if c.Validate() != nil || c.BundleID != "cn.tellyouwhat.journalapp.dev" || validOrigin(origin) != nil ||
		ttl < time.Minute || ttl > MaxJournalConnectionTTL || now.IsZero() {
		return Receipt{}, errors.New("private connections require a verified Journal development package and a one-to-fifteen-minute HTTPS lease")
	}
	canonical, err := normalizeConnection(data)
	if err != nil {
		return Receipt{}, err
	}
	unlock, err := s.lock()
	if err != nil {
		return Receipt{}, err
	}
	defer unlock()
	if err := s.verifyPackage(c); err != nil {
		return Receipt{}, err
	}
	aead, err := s.connectionCipher(true)
	if err != nil {
		return Receipt{}, err
	}
	expires := now.UTC().Add(ttl)
	plain, _ := json.Marshal(sealedConnection{c.Source, c.Digest, now.UTC(), expires, canonical})
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return Receipt{}, err
	}
	sealed := aead.Seal(nonce, nonce, plain, []byte(origin+"/d/connection/v1"))
	token := base64.RawURLEncoding.EncodeToString(sealed)
	return Receipt{URL: origin + "/d/connection/" + token + "/connection.json", Expires: expires, Digest: c.Digest, Source: c.Source}, nil
}

func (s *Store) openConnection(token, origin string, now time.Time) ([]byte, error) {
	if len(token) > 2048 || !sealedConnectionPattern.MatchString(token) {
		return nil, errors.New("invalid connection link")
	}
	sealed, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil {
		return nil, errors.New("invalid connection link")
	}
	aead, err := s.connectionCipher(false)
	if err != nil {
		return nil, err
	}
	if len(sealed) < aead.NonceSize()+aead.Overhead() {
		return nil, errors.New("invalid connection link")
	}
	nonce := sealed[:aead.NonceSize()]
	plain, err := aead.Open(nil, nonce, sealed[aead.NonceSize():], []byte(origin+"/d/connection/v1"))
	if err != nil {
		return nil, errors.New("invalid connection link")
	}
	var value sealedConnection
	if decode(bytes.NewReader(plain), &value) != nil || !commitPattern.MatchString(value.Source) || !digestPattern.MatchString(value.Digest) ||
		value.Created.IsZero() || !value.Expires.After(value.Created) || value.Expires.Sub(value.Created) > MaxJournalConnectionTTL || now.Before(value.Created) {
		return nil, errors.New("invalid connection link")
	}
	if !now.Before(value.Expires) {
		return nil, errConnectionExpired
	}
	return normalizeConnection(value.Profile)
}
