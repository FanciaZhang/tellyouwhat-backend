package appledelivery

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"
	"time"
)

const MaxPackageBytes int64 = 512 << 20
const MaxStoreBytes int64 = 2 << 30
const MaxTTL = 24 * time.Hour
const OrphanGrace = 2 * time.Hour

var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var versionPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+){0,2}$`)
var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

var appNames = map[string]string{
	"cn.tellyouwhat.healthapp.dev":  "告你健康·开发",
	"cn.tellyouwhat.journalapp.dev": "告你手记·开发",
	"cn.tellyouwhat.birthday.dev":   "告你生日开发",
	"cn.tellyouwhat.albums.dev":     "告你相册·开发",
}

// Candidate is control-plane input from a reviewed, signature-verified build.
// Only the local operator can publish; HTTP never accepts uploads or grants.
type Candidate struct {
	BundleID       string `json:"bundle_id"`
	Source         string `json:"source_commit"`
	Version        string `json:"version"`
	Build          string `json:"build"`
	Digest         string `json:"ipa_sha256"`
	Size           int64  `json:"ipa_bytes"`
	State          string `json:"state"`
	OTAPublished   bool   `json:"ota_published"`
	DeviceVerified bool   `json:"device_installation_verified"`
	GatesVerified  bool   `json:"release_gates_verified"`
}

func (c Candidate) Validate() error {
	if appNames[c.BundleID] == "" || !commitPattern.MatchString(c.Source) || !digestPattern.MatchString(c.Digest) ||
		!versionPattern.MatchString(c.Version) || !versionPattern.MatchString(c.Build) ||
		c.Size <= 0 || c.Size > MaxPackageBytes || c.State != "verified-local-candidate" {
		return errors.New("invalid development candidate")
	}
	return nil
}

type grant struct {
	Candidate Candidate `json:"candidate"`
	Created   time.Time `json:"created_at"`
	Expires   time.Time `json:"expires_at"`
}

func (g grant) validate() error {
	if err := g.Candidate.Validate(); err != nil {
		return err
	}
	if g.Created.IsZero() || !g.Expires.After(g.Created) || g.Expires.Sub(g.Created) > MaxTTL {
		return errors.New("invalid installation lease")
	}
	return nil
}

type Receipt struct {
	URL     string    `json:"url"`
	Expires time.Time `json:"expires_at"`
	Digest  string    `json:"ipa_sha256"`
	Source  string    `json:"source_commit"`
}

type Store struct {
	root     *os.Root
	lockFile *os.File
	mu       sync.Mutex
}

func Open(directory string) (*Store, error) {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("installation store must be a private directory")
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(owner.Uid) != os.Geteuid() {
		return nil, errors.New("installation store must belong to its operator")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	s := &Store{root: root}
	for _, name := range []string{"packages", "grants"} {
		if err := root.MkdirAll(name, 0700); err != nil {
			root.Close()
			return nil, err
		}
		info, err := root.Lstat(name)
		if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
			root.Close()
			return nil, errors.New("installation store contains an unsafe directory")
		}
	}
	lock, err := root.Open(".lock")
	if errors.Is(err, os.ErrNotExist) {
		lock, err = root.OpenFile(".lock", os.O_CREATE|os.O_EXCL|os.O_RDONLY, 0600)
		if errors.Is(err, os.ErrExist) {
			lock, err = root.Open(".lock")
		}
	}
	if err != nil {
		root.Close()
		return nil, err
	}
	info, err = root.Lstat(".lock")
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		lock.Close()
		root.Close()
		return nil, errors.New("unsafe installation lock")
	}
	s.lockFile = lock
	return s, nil
}

func (s *Store) Close() error { _ = s.lockFile.Close(); return s.root.Close() }

func (s *Store) lock() (func(), error) {
	s.mu.Lock()
	if err := syscall.Flock(int(s.lockFile.Fd()), syscall.LOCK_EX); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	return func() { _ = syscall.Flock(int(s.lockFile.Fd()), syscall.LOCK_UN); s.mu.Unlock() }, nil
}

func decode(r io.Reader, value any) error {
	data, err := io.ReadAll(io.LimitReader(r, 16*1024+1))
	if err != nil || len(data) > 16*1024 {
		return errors.New("installation metadata is oversized or unreadable")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return errors.New("invalid installation metadata")
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("unexpected installation metadata")
	}
	return nil
}

func ReadCandidate(path string) (Candidate, error) {
	f, err := os.Open(path)
	if err != nil {
		return Candidate{}, err
	}
	defer f.Close()
	var c Candidate
	if err := decode(f, &c); err != nil {
		return c, err
	}
	return c, c.Validate()
}

func (s *Store) regular(name string) (*os.File, error) {
	info, err := s.root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("installation file is missing or unsafe")
	}
	return s.root.Open(name)
}

func (s *Store) Publish(c Candidate, input io.Reader, origin string, ttl time.Duration, now time.Time) (Receipt, error) {
	if err := c.Validate(); err != nil {
		return Receipt{}, err
	}
	if err := validOrigin(origin); err != nil {
		return Receipt{}, err
	}
	if ttl < time.Minute || ttl > MaxTTL {
		return Receipt{}, errors.New("installation lease must be between one minute and 24 hours")
	}
	if now.IsZero() {
		return Receipt{}, errors.New("installation time is required")
	}
	unlock, err := s.lock()
	if err != nil {
		return Receipt{}, err
	}
	defer unlock()
	entries, err := fs.ReadDir(s.root.FS(), "packages")
	if err != nil {
		return Receipt{}, err
	}
	var size int64
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			return Receipt{}, errors.New("unsafe package store")
		}
		size += info.Size()
	}
	name := "packages/" + c.Digest + ".ipa"
	if _, err := s.root.Lstat(name); errors.Is(err, os.ErrNotExist) {
		if size+c.Size > MaxStoreBytes {
			return Receipt{}, errors.New("installation store capacity reached; clean expired artifacts first")
		}
		pending := "packages/.pending-" + randomToken()
		f, err := s.root.OpenFile(pending, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return Receipt{}, err
		}
		defer s.root.Remove(pending)
		h := sha256.New()
		n, copyErr := io.Copy(io.MultiWriter(f, h), io.LimitReader(input, c.Size+1))
		syncErr := f.Sync()
		closeErr := f.Close()
		if copyErr != nil || syncErr != nil || closeErr != nil || n != c.Size || hex.EncodeToString(h.Sum(nil)) != c.Digest {
			return Receipt{}, errors.New("package bytes do not match verified candidate")
		}
		if err := s.root.Rename(pending, name); err != nil {
			return Receipt{}, err
		}
	} else if err != nil {
		return Receipt{}, err
	}
	if err := s.verifyPackage(c); err != nil {
		return Receipt{}, err
	}
	token := randomToken()
	key := sha256.Sum256([]byte(token))
	g := grant{Candidate: c, Created: now.UTC(), Expires: now.Add(ttl).UTC()}
	data, _ := json.Marshal(g)
	grantName := fmt.Sprintf("grants/%x.json", key)
	pendingGrant := "packages/.pending-" + randomToken()
	f, err := s.root.OpenFile(pendingGrant, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return Receipt{}, err
	}
	defer s.root.Remove(pendingGrant)
	_, err = f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	if err != nil || syncErr != nil || closeErr != nil {
		return Receipt{}, errors.New("cannot save installation lease")
	}
	if _, err := s.root.Lstat(grantName); !errors.Is(err, os.ErrNotExist) {
		return Receipt{}, errors.New("installation lease already exists or cannot be checked")
	}
	if err := s.root.Rename(pendingGrant, grantName); err != nil {
		return Receipt{}, err
	}
	return Receipt{URL: origin + "/d/" + token + "/", Expires: g.Expires, Digest: c.Digest, Source: c.Source}, nil
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("secure randomness unavailable")
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *Store) verifyPackage(c Candidate) error {
	f, err := s.regular("packages/" + c.Digest + ".ipa")
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, c.Size+1))
	if err != nil || n != c.Size || hex.EncodeToString(h.Sum(nil)) != c.Digest {
		return errors.New("stored package integrity check failed")
	}
	return nil
}

func (s *Store) readGrant(name string) (grant, error) {
	f, err := s.regular("grants/" + name)
	if err != nil {
		return grant{}, err
	}
	defer f.Close()
	var g grant
	if err := decode(f, &g); err != nil {
		return g, err
	}
	return g, g.validate()
}

type CleanupResult struct {
	Grants   int `json:"grants_removed"`
	Packages int `json:"packages_removed"`
}

func (s *Store) Cleanup(now time.Time) (CleanupResult, error) {
	unlock, err := s.lock()
	if err != nil {
		return CleanupResult{}, err
	}
	defer unlock()
	entries, err := fs.ReadDir(s.root.FS(), "grants")
	if err != nil {
		return CleanupResult{}, err
	}
	active := map[string]bool{}
	expired := []string{}
	for _, entry := range entries {
		name := entry.Name()
		if filepath.Ext(name) != ".json" || !digestPattern.MatchString(name[:len(name)-5]) {
			return CleanupResult{}, errors.New("unknown installation lease; cleanup stopped")
		}
		g, err := s.readGrant(name)
		if err != nil {
			return CleanupResult{}, err
		}
		if now.Before(g.Expires) {
			active[g.Candidate.Digest] = true
		} else {
			expired = append(expired, name)
		}
	}
	// Validate the complete inventory before removing any item.
	packages, err := fs.ReadDir(s.root.FS(), "packages")
	if err != nil {
		return CleanupResult{}, err
	}
	remove := []string{}
	for _, entry := range packages {
		name := entry.Name()
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return CleanupResult{}, errors.New("unsafe package inventory")
		}
		if len(name) == 68 && digestPattern.MatchString(name[:64]) && name[64:] == ".ipa" {
			if !active[name[:64]] && now.Sub(info.ModTime()) >= OrphanGrace {
				remove = append(remove, name)
			}
		} else if len(name) == 52 && name[:9] == ".pending-" && tokenPattern.MatchString(name[9:]) {
			if now.Sub(info.ModTime()) >= OrphanGrace {
				remove = append(remove, name)
			}
		} else {
			return CleanupResult{}, errors.New("unknown package artifact; cleanup stopped")
		}
	}
	var result CleanupResult
	for _, name := range expired {
		if err := s.root.Remove("grants/" + name); err != nil {
			return result, err
		}
		result.Grants++
	}
	for _, name := range remove {
		if err := s.root.Remove("packages/" + name); err != nil {
			return result, err
		}
		result.Packages++
	}
	return result, nil
}
