package albums

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
	"github.com/tellyouwhat/backend/migrations"
)

type sharingTestSigner struct {
	*objectTestStore
	calls atomic.Int32
	fail  atomic.Bool
	mu    sync.Mutex
	hold  <-chan struct{}
	began chan struct{}
}

func (s *sharingTestSigner) AuthorizeSharedOriginalRead(ctx context.Context, object SealedObject, filename string, expiry time.Time) (string, error) {
	s.calls.Add(1)
	if s.fail.Load() {
		return "", errors.New("private-signing-detail")
	}
	s.mu.Lock()
	hold, began := s.hold, s.began
	s.began = nil
	s.mu.Unlock()
	if began != nil {
		close(began)
	}
	if hold != nil {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-hold:
		}
	}
	return "https://storage.example/" + object.Key + "?versionId=" + url.QueryEscape(object.VersionID) + "&name=" + url.QueryEscape(filename), nil
}

type sharingFixture struct {
	db      *sql.DB
	dsn     string
	repo    *MySQLUploads
	service *ShareService
	uploads *UploadService
	signer  *sharingTestSigner
	login   AccountLogin
	session AlbumSession
	version AssetVersion
	now     *time.Time
	key     []byte
}

func newSharingFixture(t *testing.T) sharingFixture {
	t.Helper()
	db, dsn := gatewayTestDatabase(t)
	ctx := context.Background()
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal("migration replay", err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	cipher, err := mysqlstore.NewPayloadCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{41}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := NewMySQLAccounts(db, cipher)
	if err != nil {
		t.Fatal(err)
	}
	login, err := accounts.RegisterAppleLogin(ctx, AppleGrant{Identity: AppleIdentity{Subject: uuid.NewString()}, RefreshToken: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	sessions, _ := NewMySQLSessions(db, func() time.Time { return now })
	session, err := sessions.Issue(ctx, login)
	if err != nil {
		t.Fatal(err)
	}
	repo := NewMySQLUploads(db).WithSubscriptionEnvironment("Sandbox", func() time.Time { return now })
	if _, err := repo.ApplySubscription(ctx, login.AccountID, 0, VerifiedAlbumSubscription{uuid.NewString(), uuid.NewString(), Cloud50GBAnnualProduct, "Sandbox", now.AddDate(1, 0, 0), now, true}, now); err != nil {
		t.Fatal(err)
	}
	_, _, objects, _, _ := uploadFixture(t)
	signer := &sharingTestSigner{objectTestStore: objects}
	uploads, err := NewUploadService(repo, signer, UploadPolicy{1 << 30, 1 << 20, 4 * time.Hour, time.Minute}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("synthetic original bytes")
	sum := sha256.Sum256(content)
	manifest := Manifest{AssetID: uuid.NewString(), SourceRevision: "sharing-fixture-v1", Kind: Photo, Resources: []Resource{{ID: "photo", Role: OriginalPhoto, SizeBytes: int64(len(content)), SHA256: hex.EncodeToString(sum[:]), Descriptor: &ResourceDescriptor{PhotoKitType: 1, Filename: "回忆.jpeg", UniformTypeIdentifier: "public.jpeg"}}}}
	digest, _ := manifest.Digest()
	u := Upload{ID: uuid.NewString(), OwnerID: login.AccountID, Manifest: manifest, Digest: digest, OriginalBytes: int64(len(content))}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repo.lockAccount(ctx, tx, login.AccountID); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	sealed := []SealedObject{{ResourceID: "photo", Key: sealedKey(u, "photo"), VersionID: "immutable-share-v1", SizeBytes: int64(len(content)), SHA256: hex.EncodeToString(sum[:])}}
	if err = publishAssetVersion(ctx, tx, u, sealed, now); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE album_storage_accounts SET used_bytes=? WHERE owner_id=?`, int64(len(content)), login.AccountID); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	version, err := repo.AssetVersion(ctx, login.AccountID, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{53}, 32)
	service, err := NewShareService(repo, uploads, "https://shares.example", [][]byte{key}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	return sharingFixture{db, dsn, repo, service, uploads, signer, login, session, version, &now, key}
}
func (f sharingFixture) create(t *testing.T, password string, max int) (ShareLink, string) {
	t.Helper()
	link, err := f.service.Create(context.Background(), f.login.AccountID, f.version.VersionID, ShareCreateInput{uuid.NewString(), 7 * 86400, max, password})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(link.URL)
	if err != nil || parsed.Fragment == "" {
		t.Fatal("missing fragment-only link")
	}
	return link, parsed.Fragment
}
func TestMySQLAlbumSharingPasswordIdempotencyRestartAndRecovery(t *testing.T) {
	f := newSharingFixture(t)
	ctx := context.Background()
	input := ShareCreateInput{uuid.NewString(), 7 * 86400, 2, "私密123"}
	link, err := f.service.Create(ctx, f.login.AccountID, f.version.VersionID, input)
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.service.Create(ctx, f.login.AccountID, f.version.VersionID, input)
	if err != nil || again.URL != link.URL || again.ID != link.ID {
		t.Fatal("create replay mutated link", err)
	}
	altered := input
	altered.MaxClaims = 3
	if _, err := f.service.Create(ctx, f.login.AccountID, f.version.VersionID, altered); !errors.Is(err, ErrShareConflict) {
		t.Fatal("changed replay accepted", err)
	}
	parsed, _ := url.Parse(link.URL)
	token := parsed.Fragment
	if parsed.RawQuery != "" || parsed.Path != "/share/" {
		t.Fatal("secret exposed in site path or query")
	}
	info, err := f.service.Lookup(ctx, token)
	if err != nil || !info.RequiresPassword || info.RemainingClaims != 2 || len(info.Resources) != 1 || info.Resources[0].Filename != "回忆.jpeg" {
		t.Fatal("lookup metadata or counter", err)
	}
	request := uuid.NewString()
	if _, err := f.service.Claim(ctx, token, "wrong", request); !errors.Is(err, ErrSharePassword) {
		t.Fatal("wrong password accepted", err)
	}
	claim, err := f.service.Claim(ctx, token, input.Password, request)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := f.service.Claim(ctx, token, input.Password, request)
	if err != nil || repeated != claim {
		t.Fatal("retry consumed another claim", err)
	}
	f.signer.fail.Store(true)
	if _, err := f.service.ReadGrants(ctx, claim.Token); !errors.Is(err, ErrShareUnavailable) {
		t.Fatal("signing failure accepted", err)
	}
	f.signer.fail.Store(false)
	grants, err := f.service.ReadGrants(ctx, claim.Token)
	if err != nil || len(grants) != 1 || grants[0].ObjectVersionID != "immutable-share-v1" || grants[0].Filename != "回忆.jpeg" || grants[0].ExpiresAt.After(f.now.Add(5*time.Minute)) {
		t.Fatal("immutable resource grant", err)
	}
	if info, err = f.service.Lookup(ctx, token); err != nil || info.RemainingClaims != 1 {
		t.Fatal("grant renewal consumed count", err)
	}
	var proof, storedToken []byte
	if err := f.db.QueryRowContext(ctx, `SELECT password_proof,token_digest FROM album_share_links WHERE share_id=?`, link.ID).Scan(&proof, &storedToken); err != nil {
		t.Fatal(err)
	}
	if len(proof) != 49 || bytes.Contains(proof, []byte(input.Password)) || bytes.Contains(storedToken, []byte(token)) {
		t.Fatal("plaintext secret persisted")
	}
	restarted, err := NewShareService(f.repo, f.uploads, "https://shares.example", [][]byte{bytes.Repeat([]byte{61}, 32), f.key}, func() time.Time { return *f.now })
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := restarted.Create(ctx, f.login.AccountID, f.version.VersionID, input)
	if err != nil || recovered.URL != link.URL {
		t.Fatal("restart/rotation broke link replay", err)
	}
	recoveredClaim, err := restarted.Claim(ctx, token, input.Password, request)
	if err != nil || recoveredClaim.Token != claim.Token {
		t.Fatal("restart/rotation charged retry", err)
	}
	if _, err := f.service.Create(ctx, uuid.NewString(), f.version.VersionID, ShareCreateInput{uuid.NewString(), 86400, 1, ""}); !errors.Is(err, ErrNotFound) {
		t.Fatal("foreign account created link", err)
	}
	if err := f.service.Revoke(ctx, uuid.NewString(), link.ID); !errors.Is(err, ErrOwner) && !errors.Is(err, ErrNotFound) {
		t.Fatal("foreign revoke", err)
	}
}
func TestMySQLAlbumSharingLastClaimConcurrencyAndPoolBoundedGrants(t *testing.T) {
	f := newSharingFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, token := f.create(t, "", 3)
	f.db.SetMaxOpenConns(2)
	var group sync.WaitGroup
	var successes atomic.Int32
	claims := make(chan ShareClaim, 30)
	errs := make(chan error, 30)
	for range 30 {
		group.Add(1)
		go func() {
			defer group.Done()
			claim, err := f.service.Claim(ctx, token, "", uuid.NewString())
			if err == nil {
				successes.Add(1)
				claims <- claim
			} else if !errors.Is(err, ErrShareLimit) {
				errs <- err
			}
		}()
	}
	group.Wait()
	close(errs)
	close(claims)
	for err := range errs {
		t.Fatal("claim contention", err)
	}
	if successes.Load() != 3 {
		t.Fatal("last allowance overspent", successes.Load())
	}
	claim := <-claims
	f.db.SetMaxOpenConns(1)
	for range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			grants, err := f.service.ReadGrants(ctx, claim.Token)
			if err != nil || len(grants) != 1 {
				t.Errorf("one-connection authorization blocked: %v", err)
			}
		}()
	}
	group.Wait()
	info, err := f.service.Lookup(ctx, token)
	if err != nil || info.RemainingClaims != 0 {
		t.Fatal("incorrect durable count", err)
	}
	if _, err := f.service.ReadGrants(ctx, token); !errors.Is(err, ErrShareNotAvailable) {
		t.Fatal("link token used as claim token", err)
	}
}
func TestMySQLAlbumSharingRevocationExpiryRetentionAndAccountState(t *testing.T) {
	f := newSharingFixture(t)
	ctx := context.Background()
	link, token := f.create(t, "", 2)
	request := uuid.NewString()
	claim, err := f.service.Claim(ctx, token, "", request)
	if err != nil {
		t.Fatal(err)
	}
	held := make(chan struct{})
	began := make(chan struct{})
	f.signer.mu.Lock()
	f.signer.hold = held
	f.signer.began = began
	f.signer.mu.Unlock()
	done := make(chan error, 1)
	go func() { _, err := f.service.ReadGrants(ctx, claim.Token); done <- err }()
	select {
	case <-began:
	case <-time.After(3 * time.Second):
		t.Fatal("grant signer not reached")
	}
	revoked := make(chan error, 1)
	go func() { revoked <- f.service.Revoke(ctx, f.login.AccountID, link.ID) }()
	close(held)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-revoked; err != nil {
		t.Fatal(err)
	}
	before := f.signer.calls.Load()
	if _, err := f.service.ReadGrants(ctx, claim.Token); !errors.Is(err, ErrShareNotAvailable) || f.signer.calls.Load() != before {
		t.Fatal("revoked link renewed grant", err)
	}
	if _, err := f.service.Claim(ctx, token, "", request); !errors.Is(err, ErrShareNotAvailable) {
		t.Fatal("revoked idempotent claim resurrected", err)
	}
	// A separate valid link expires exactly at the original retained deadline.
	f.signer.mu.Lock()
	f.signer.hold = nil
	f.signer.mu.Unlock()
	a, err := f.repo.CloudAccount(ctx, f.login.AccountID, *f.now)
	if err != nil {
		t.Fatal(err)
	}
	var original, transaction string
	if err := f.db.QueryRowContext(ctx, `SELECT original_transaction_id,transaction_id FROM album_cloud_subscriptions WHERE owner_id=?`, f.login.AccountID).Scan(&original, &transaction); err != nil {
		t.Fatal(err)
	}
	ended := f.now.AddDate(0, -6, 0).Add(2 * time.Minute)
	_, err = f.repo.ApplySubscription(ctx, f.login.AccountID, a.Revision, VerifiedAlbumSubscription{original, transaction, Cloud50GBAnnualProduct, "Sandbox", ended, f.now.Add(time.Second), false}, *f.now)
	if err != nil {
		t.Fatal(err)
	}
	clipped, newToken := f.create(t, "", 2)
	if !clipped.ExpiresAt.Equal(f.now.Add(2 * time.Minute)) {
		t.Fatal("link outlived retention")
	}
	current, err := f.service.Claim(ctx, newToken, "", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	grants, err := f.service.ReadGrants(ctx, current.Token)
	if err != nil || !grants[0].ExpiresAt.Equal(clipped.ExpiresAt) {
		t.Fatal("grant outlived retention", err)
	}
	if _, err := f.db.ExecContext(ctx, `UPDATE album_accounts SET state='disabled' WHERE account_id=?`, f.login.AccountID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Lookup(ctx, newToken); !errors.Is(err, ErrShareNotAvailable) {
		t.Fatal("disabled account shared media", err)
	}
	if _, err := f.db.ExecContext(ctx, `UPDATE album_accounts SET state='active' WHERE account_id=?`, f.login.AccountID); err != nil {
		t.Fatal(err)
	}
	*f.now = clipped.ExpiresAt
	if _, err := f.service.ReadGrants(ctx, current.Token); !errors.Is(err, ErrShareNotAvailable) {
		t.Fatal("retention boundary issued grant", err)
	}
	var versions int
	if err := f.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM album_asset_versions WHERE owner_id=?`, f.login.AccountID).Scan(&versions); err != nil || versions != 1 {
		t.Fatal("sharing destroyed original", err)
	}
}
func TestMySQLAlbumSharingLeaseExpiryCannotBeReplayedForFree(t *testing.T) {
	f := newSharingFixture(t)
	ctx := context.Background()
	_, token := f.create(t, "", 2)
	id := uuid.NewString()
	claim, err := f.service.Claim(ctx, token, "", id)
	if err != nil {
		t.Fatal(err)
	}
	*f.now = claim.ExpiresAt
	if _, err := f.service.ReadGrants(ctx, claim.Token); !errors.Is(err, ErrShareClaimExpired) {
		t.Fatal("expired session renewed", err)
	}
	if _, err := f.service.Claim(ctx, token, "", id); !errors.Is(err, ErrShareClaimExpired) {
		t.Fatal("old request extended session", err)
	}
	if _, err := f.service.Claim(ctx, token, "", uuid.NewString()); err != nil {
		t.Fatal("new explicit authorization", err)
	}
}

func TestMySQLAlbumSharingLiveBundleRejectsIncompleteEvidence(t *testing.T) {
	f := newSharingFixture(t)
	ctx := context.Background()
	manifest := liveManifest()
	manifest.AssetID = uuid.NewString()
	manifest.Resources[0].Descriptor = &ResourceDescriptor{PhotoKitType: 1, Filename: "实况.jpeg", UniformTypeIdentifier: "public.jpeg"}
	manifest.Resources[1].Descriptor = &ResourceDescriptor{PhotoKitType: 9, Filename: "实况.mov", UniformTypeIdentifier: "com.apple.quicktime-movie"}
	digest, _ := manifest.Digest()
	u := Upload{ID: uuid.NewString(), OwnerID: f.login.AccountID, Manifest: manifest, Digest: digest, OriginalBytes: 300}
	objects := []SealedObject{}
	for _, resource := range manifest.Resources {
		objects = append(objects, SealedObject{ResourceID: resource.ID, Key: sealedKey(u, resource.ID), VersionID: "immutable-live-" + resource.ID, SizeBytes: resource.SizeBytes, SHA256: resource.SHA256})
	}
	tx, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.lockAccount(ctx, tx, f.login.AccountID); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := publishAssetVersion(ctx, tx, u, objects, *f.now); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	link, err := f.service.Create(ctx, f.login.AccountID, u.ID, ShareCreateInput{uuid.NewString(), 86400, 1, ""})
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(link.URL)
	info, err := f.service.Lookup(ctx, parsed.Fragment)
	if err != nil || info.Kind != "live_photo" || len(info.Resources) != 2 {
		t.Fatal("live resource lookup", err)
	}
	claim, err := f.service.Claim(ctx, parsed.Fragment, "", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	grants, err := f.service.ReadGrants(ctx, claim.Token)
	if err != nil || len(grants) != 2 || grants[0].Role != string(OriginalPhoto) || grants[1].Role != string(PairedVideo) {
		t.Fatal("one live claim did not cover full pair", err)
	}
	broken, _ := json.Marshal(objects[:1])
	if _, err := f.db.ExecContext(ctx, `UPDATE album_asset_versions SET sealed_objects=? WHERE owner_id=? AND version_id=?`, broken, f.login.AccountID, u.ID); err != nil {
		t.Fatal(err)
	}
	before := f.signer.calls.Load()
	if _, err := f.service.ReadGrants(ctx, claim.Token); !errors.Is(err, ErrShareNotAvailable) || f.signer.calls.Load() != before {
		t.Fatal("partial live proof issued grants", err)
	}
}
