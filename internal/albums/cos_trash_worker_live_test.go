//go:build albums_live

package albums

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/migrations"
	cos "github.com/tencentyun/cos-go-sdk-v5"
)

const liveTrashScope = "albums-owned-trash-worker-v1"

type liveTrashVersion struct {
	Key, VersionID, SHA256 string
}

type liveTrashIntent struct {
	Scope, Owner, Upload, Asset, Multipart string
	Keys                                   []string
	Versions                               []liveTrashVersion
	Ready                                  bool
}

func liveTrashPayload(variant byte) []byte {
	picture := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := range 8 {
		for x := range 8 {
			picture.SetRGBA(x, y, color.RGBA{R: variant, G: 100, B: 200, A: 255})
		}
	}
	var b bytes.Buffer
	if jpeg.Encode(&b, picture, nil) != nil || b.Len() > 4096 {
		panic("invalid synthetic JPEG fixture")
	}
	return append(b.Bytes(), make([]byte, 4096-b.Len())...)
}

func liveTrashLoad(t *testing.T, root string) liveTrashIntent {
	t.Helper()
	var in liveTrashIntent
	roundtripRead(t, root, "prepared.json", &in)
	if in.Scope != liveTrashScope || !in.Ready || !validOwner(in.Owner) || !validOwner(in.Upload) || !resourceIDPattern.MatchString(in.Asset) || len(in.Keys) != 3 || len(in.Versions) != 4 || !validCOSMultipartID(in.Multipart) ||
		in.Keys[0] != "albums/originals/"+in.Owner+"/"+in.Upload+"/photo" || in.Keys[1] != "albums/staging/"+in.Owner+"/"+in.Upload+"/photo" || in.Keys[2] != in.Keys[0]+"-neighbor" {
		t.Fatal("only this probe's completed synthetic preparation may be consumed")
	}
	for _, v := range in.Versions {
		if !concreteCOSVersion(v.VersionID) || !validSHA256(v.SHA256) || (v.Key != in.Keys[0] && v.Key != in.Keys[1] && v.Key != in.Keys[2]) {
			t.Fatal("invalid owned version receipt")
		}
	}
	var original liveTrashIntent
	roundtripRead(t, root, "intent.json", &original)
	if original.Scope != in.Scope || original.Owner != in.Owner || original.Upload != in.Upload || original.Asset != in.Asset || strings.Join(original.Keys, "\x00") != strings.Join(in.Keys, "\x00") {
		t.Fatal("preparation differs from the original ownership intent")
	}
	for i, expected := range []struct {
		key     string
		variant byte
	}{{in.Keys[0], 30}, {in.Keys[0], 80}, {in.Keys[1], 80}, {in.Keys[2], 160}} {
		if in.Versions[i].Key != expected.key || in.Versions[i].SHA256 != roundtripHash(liveTrashPayload(expected.variant)) {
			t.Fatal("only the fixed synthetic payloads may be consumed")
		}
	}
	return in
}

func liveTrashReadVersion(t *testing.T, ctx context.Context, reader *COSObjects, v liveTrashVersion) {
	t.Helper()
	response, err := reader.client.Object.Get(ctx, v.Key, nil, url.QueryEscape(v.VersionID))
	if response == nil || response.Body == nil {
		t.Fatal("owned synthetic version unavailable")
	}
	defer response.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil || readErr != nil || response.StatusCode != 200 || len(data) != 4096 || roundtripHash(data) != v.SHA256 || response.Header.Get("x-cos-version-id") != v.VersionID {
		t.Fatal("owned synthetic version bytes differ; preserve it")
	}
	if strings.HasPrefix(v.Key, "albums/originals/") && !strings.HasSuffix(v.Key, "-neighbor") && response.Header.Get("x-cos-storage-class") != "MAZ_STANDARD_IA" {
		t.Fatal("original fixture did not use the planned low-frequency class")
	}
}

func prepareLiveTrash(t *testing.T, ctx context.Context, root string, reader, cleaner *COSObjects) {
	t.Helper()
	in := liveTrashIntent{Scope: liveTrashScope, Owner: uuid.NewString(), Upload: uuid.NewString(), Asset: "trash-" + uuid.NewString()}
	in.Keys = []string{"albums/originals/" + in.Owner + "/" + in.Upload + "/photo", "albums/staging/" + in.Owner + "/" + in.Upload + "/photo"}
	in.Keys = append(in.Keys, in.Keys[0]+"-neighbor")
	for _, key := range in.Keys {
		vs, e := cleaner.listPurgeVersions(ctx, in.Owner, purgeTarget{key})
		ms, me := cleaner.listPurgeUploads(ctx, in.Owner, purgeTarget{key})
		if e != nil || me != nil || len(vs)+len(ms) != 0 {
			t.Fatal("new exact keys are not confirmed empty; preserve namespace")
		}
	}
	photoMultipartProbeCheckpoint(t, root, "intent.json", in)
	for i, item := range []struct {
		key     string
		variant byte
	}{{in.Keys[0], 30}, {in.Keys[0], 80}, {in.Keys[1], 80}, {in.Keys[2], 160}} {
		payload := liveTrashPayload(item.variant)
		name := string(rune('0' + i))
		photoMultipartProbeCheckpoint(t, root, "put-"+name+"-intent.json", map[string]any{"key": item.key, "sha256": roundtripHash(payload), "bytes": len(payload)})
		storageClass := "MAZ_STANDARD"
		if item.key == in.Keys[0] {
			storageClass = "MAZ_STANDARD_IA"
		}
		result, err := reader.client.Object.Put(ctx, item.key, bytes.NewReader(payload), &cos.ObjectPutOptions{ObjectPutHeaderOptions: &cos.ObjectPutHeaderOptions{ContentType: "image/jpeg", XCosStorageClass: storageClass, XCosServerSideEncryption: "AES256"}})
		status, code, requestID := 0, "", ""
		if result != nil {
			status, requestID = result.StatusCode, result.Header.Get("x-cos-request-id")
		}
		var provider *cos.ErrorResponse
		if errors.As(err, &provider) {
			code = provider.Code
		}
		photoMultipartProbeCheckpoint(t, root, "put-"+name+"-response.json", map[string]any{"status": status, "providerCode": code, "requestID": requestID, "error": err != nil})
		if result != nil && result.Body != nil {
			result.Body.Close()
		}
		if err != nil || result == nil || result.StatusCode != 200 || !concreteCOSVersion(result.Header.Get("x-cos-version-id")) {
			t.Fatalf("PUT not confirmed: status=%d code=%s; retain intent", status, code)
		}
		v := liveTrashVersion{item.key, result.Header.Get("x-cos-version-id"), roundtripHash(payload)}
		liveTrashReadVersion(t, ctx, reader, v)
		in.Versions = append(in.Versions, v)
		photoMultipartProbeCheckpoint(t, root, "put-"+name+"-closed.json", v)
	}
	photoMultipartProbeCheckpoint(t, root, "multipart-intent.json", map[string]string{"key": in.Keys[1]})
	id, err := reader.InitiateStagingMultipart(ctx, in.Keys[1])
	if err != nil {
		t.Fatal("multipart initiation unknown; preserve intent")
	}
	in.Multipart = id
	photoMultipartProbeCheckpoint(t, root, "multipart-session.json", map[string]string{"key": in.Keys[1], "uploadID": id})
	part, err := reader.uploadClient.Object.UploadPart(ctx, in.Keys[1], id, 1, bytes.NewReader(liveTrashPayload(30)), nil)
	if part != nil && part.Body != nil {
		part.Body.Close()
	}
	if err != nil || part == nil || part.StatusCode != 200 || part.Header.Get("ETag") == "" {
		t.Fatal("part writer did not close; preserve session")
	}
	in.Ready = true
	// A negative permission check affects only this newly created disposable key.
	// The worker itself never sends an unversioned DELETE.
	if os.Getenv("ALBUM_TRASH_COS_UNVERSIONED_PROBE") == "1" {
		photoMultipartProbeCheckpoint(t, root, "marker-probe-intent.json", map[string]string{"key": in.Keys[0]})
		response, err := cleaner.client.Object.Delete(ctx, in.Keys[0], nil)
		status, version := 0, ""
		if response != nil {
			status, version = response.StatusCode, response.Header.Get("x-cos-version-id")
			if response.Body != nil {
				response.Body.Close()
			}
		}
		if status != 403 && (err != nil || status != 204 || !concreteCOSVersion(version)) {
			t.Fatal("marker probe outcome unknown; retain intent")
		}
		photoMultipartProbeCheckpoint(t, root, "marker-probe-result.json", map[string]any{"status": status, "version": version, "expectedDenied": status == 403, "scopeReviewRequired": status == 204})
	}
	photoMultipartProbeCheckpoint(t, root, "prepared.json", in)
}

// Only exact owned keys and the frozen pre-worker inventory may be mutated.
type liveTrashStorage struct {
	cleaner              *COSObjects
	owner                string
	allowed              map[string]map[string]bool
	uploads              map[string]map[string]bool
	lose                 bool
	lostKey, lostVersion string
	deletes, aborts      int
	deleteAttempts       []liveTrashVersion
	afterMutation        func(string, string, string)
}

func (s *liveTrashStorage) listPurgeVersions(ctx context.Context, owner string, target purgeTarget) ([]DerivedObjectVersion, error) {
	if owner != s.owner || s.allowed[target.Key] == nil {
		return nil, ErrInvalidManifest
	}
	return s.cleaner.listPurgeVersions(ctx, owner, target)
}
func (s *liveTrashStorage) listPurgeUploads(ctx context.Context, owner string, target purgeTarget) ([]string, error) {
	if owner != s.owner || s.allowed[target.Key] == nil {
		return nil, ErrInvalidManifest
	}
	return s.cleaner.listPurgeUploads(ctx, owner, target)
}
func (s *liveTrashStorage) deletePurgeVersion(ctx context.Context, owner string, target purgeTarget, v DerivedObjectVersion) error {
	if owner != s.owner || !s.allowed[target.Key][v.VersionID] {
		return ErrInvalidManifest
	}
	s.deleteAttempts = append(s.deleteAttempts, liveTrashVersion{Key: target.Key, VersionID: v.VersionID})
	if err := s.cleaner.deletePurgeVersion(ctx, owner, target, v); err != nil {
		return err
	}
	s.deletes++
	if s.afterMutation != nil {
		s.afterMutation("delete", target.Key, v.VersionID)
	}
	if s.lose {
		s.lose = false
		s.lostKey, s.lostVersion = target.Key, v.VersionID
		return ErrCOSOperation
	}
	return nil
}
func (s *liveTrashStorage) abortPurgeUpload(ctx context.Context, owner string, target purgeTarget, id string) error {
	if owner != s.owner || !s.uploads[target.Key][id] {
		return ErrInvalidManifest
	}
	if err := s.cleaner.abortPurgeUpload(ctx, owner, target, id); err != nil {
		return err
	}
	s.aborts++
	if s.afterMutation != nil {
		s.afterMutation("abort", target.Key, id)
	}
	return nil
}

func liveTrashInventory(t *testing.T, ctx context.Context, reader, cleaner *COSObjects, in liveTrashIntent) (*liveTrashStorage, int) {
	t.Helper()
	s := &liveTrashStorage{cleaner: cleaner, owner: in.Owner, allowed: map[string]map[string]bool{}, uploads: map[string]map[string]bool{}}
	known := map[string]liveTrashVersion{}
	for _, v := range in.Versions {
		known[v.Key+"\x00"+v.VersionID] = v
	}
	markers := 0
	for _, key := range in.Keys {
		s.allowed[key], s.uploads[key] = map[string]bool{}, map[string]bool{}
		vs, err := cleaner.listPurgeVersions(ctx, in.Owner, purgeTarget{key})
		if err != nil || len(vs) > 8 {
			t.Fatal("bounded exact-key inventory unavailable")
		}
		for _, v := range vs {
			if !v.DeleteMarker {
				owned, ok := known[key+"\x00"+v.VersionID]
				if !ok || v.SizeBytes != 4096 {
					t.Fatal("unexpected version preserved")
				}
				liveTrashReadVersion(t, ctx, reader, owned)
			} else {
				markers++
			}
			s.allowed[key][v.VersionID] = true
		}
		ids, err := cleaner.listPurgeUploads(ctx, in.Owner, purgeTarget{key})
		if err != nil {
			t.Fatal("exact-key upload inventory unavailable")
		}
		for _, id := range ids {
			if key != in.Keys[1] || id != in.Multipart {
				t.Fatal("unexpected multipart session preserved")
			}
			s.uploads[key][id] = true
		}
	}
	return s, markers
}

func runLiveTrashWorker(t *testing.T, ctx context.Context, root string, reader, cleaner *COSObjects, in liveTrashIntent) {
	t.Helper()
	admin, err := mysql.ParseDSN(os.Getenv("MYSQL_TEST_ADMIN_DSN"))
	if err != nil || admin.Net != "unix" || !strings.HasSuffix(admin.DBName, "_test") {
		t.Fatal("isolated socket-only MySQL is required")
	}
	info, err := os.Lstat(filepath.Dir(admin.Addr))
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		t.Fatal("private MySQL socket directory required")
	}
	storage, markers := liveTrashInventory(t, ctx, reader, cleaner, in)
	for _, v := range in.Versions {
		if !storage.allowed[v.Key][v.VersionID] {
			t.Fatal("preparation was already partially consumed; use its cleanup handoff")
		}
	}
	if !storage.uploads[in.Keys[1]][in.Multipart] {
		t.Fatal("prepared multipart is no longer available")
	}
	if os.Getenv("ALBUM_TRASH_COS_REQUIRE_MARKER") == "1" && markers == 0 {
		t.Fatal("real delete marker required before running; prepared resources retained")
	}
	// The sibling shares an inventory prefix but is outside the worker's exact keys.
	delete(storage.allowed, in.Keys[2])
	delete(storage.uploads, in.Keys[2])
	db, _ := gatewayTestDatabase(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	now := seedLiveTrashDatabase(t, ctx, db, in)
	r := NewMySQLUploads(db).WithSubscriptionEnvironment("Sandbox", func() time.Time { return now })
	r.productionSandboxQuota = 1_000_000_000
	storage.lose = os.Getenv("ALBUM_TRASH_COS_LOSE_REPLY") == "1"
	w := &retentionCleanupWorker{repository: r, storage: storage, batchSize: 10, now: func() time.Time { return now }, trashEnabled: true}
	settled, lostObserved := 0, false
	attempt := uuid.NewString()
	for pass := 0; pass < 16 && settled == 0; pass++ {
		b, err := w.runOnce(ctx)
		if err != nil {
			t.Fatal("live worker failed; retain owned intent", err)
		}
		if b.Deferred > 0 && storage.lostVersion != "" && !lostObserved {
			used, _ := photoJournalBalance(t, db, in.Owner)
			if used != 4096 {
				t.Fatal("lost delete reply refunded capacity")
			}
			lostObserved = true
			now = now.Add(11 * time.Minute)
			w.repository = NewMySQLUploads(db).WithSubscriptionEnvironment("Sandbox", w.now)
			w.repository.productionSandboxQuota = 1_000_000_000
		} else {
			now = now.Add(6 * time.Second)
		}
		settled += b.Settled
		photoMultipartProbeCheckpoint(t, root, "worker-"+attempt+"-pass-"+string(rune('a'+pass))+".json", b)
	}
	if settled != 1 || os.Getenv("ALBUM_TRASH_COS_LOSE_REPLY") == "1" && !lostObserved {
		t.Fatal("live worker did not settle")
	}
	lostAttempts := 0
	for _, v := range storage.deleteAttempts {
		if v.Key == storage.lostKey && v.VersionID == storage.lostVersion {
			lostAttempts++
		}
	}
	if lostObserved && lostAttempts != 2 {
		t.Fatal("lost reply was not retried against the same immutable version")
	}
	used, reserved := photoJournalBalance(t, db, in.Owner)
	var receipts int
	if err := db.QueryRow(`SELECT COUNT(*) FROM album_asset_deletion_ledger WHERE owner_id=? AND asset_id=? AND reason='trash'`, in.Owner, in.Asset).Scan(&receipts); err != nil || receipts != 1 || used != 0 || reserved != 0 {
		t.Fatal("live settlement receipt or capacity invalid")
	}
	for _, key := range in.Keys[:2] {
		vs, err := cleaner.listPurgeVersions(ctx, in.Owner, purgeTarget{key})
		ids, e := cleaner.listPurgeUploads(ctx, in.Owner, purgeTarget{key})
		if err != nil || e != nil || len(vs)+len(ids) != 0 {
			t.Fatal("live target not empty after settlement")
		}
	}
	liveTrashReadVersion(t, ctx, reader, in.Versions[3])
	photoMultipartProbeCheckpoint(t, root, "worker-result.json", map[string]any{"passed": true, "realCOS": true, "realMySQL": true, "deleteMarkersObserved": markers, "deletes": storage.deletes, "aborts": storage.aborts, "lostReplyObserved": lostObserved, "lostVersionAttempts": lostAttempts, "deleteAttempts": storage.deleteAttempts, "usedBytes": used, "reservedBytes": reserved, "deletionReceipts": receipts, "siblingPreserved": true, "syntheticAccount": true, "appOrSubscriptionAcceptance": false})
}

func seedLiveTrashDatabase(t *testing.T, ctx context.Context, db *sql.DB, in liveTrashIntent) time.Time {
	t.Helper()
	r := NewMySQLUploads(db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	resource := Resource{ID: "photo", Role: OriginalPhoto, SizeBytes: 4096, SHA256: roundtripHash(liveTrashPayload(80))}
	manifest := Manifest{AssetID: in.Asset, SourceRevision: liveTrashScope, Kind: Photo, Resources: []Resource{resource}}
	digest, _ := manifest.Digest()
	u := Upload{ID: in.Upload, OwnerID: in.Owner, RequestID: uuid.NewString(), Manifest: manifest, Digest: digest, State: Uploading, OriginalBytes: 4096, DerivedBudget: 4096, ExpiresAt: now.Add(time.Hour)}
	if err := r.ProvisionAccount(ctx, in.Owner, 16384); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reserve(ctx, u); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Submit(ctx, in.Owner, in.Upload, now); err != nil {
		t.Fatal(err)
	}
	token := uuid.NewString()
	if _, err := r.Claim(ctx, in.Owner, in.Upload, token, now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	source := in.Versions[1]
	if source.Key != sealedKey(u, "photo") || source.SHA256 != resource.SHA256 {
		t.Fatal("actual source receipt differs from metadata")
	}
	sealed := []SealedObject{{ResourceID: "photo", Key: source.Key, VersionID: source.VersionID, SizeBytes: 4096, SHA256: source.SHA256}}
	recordFixtureSeals(t, r, u, token, sealed, now)
	u, err := r.Complete(ctx, in.Owner, in.Upload, token, sealed, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO album_accounts(account_id,apple_subject_sha256) VALUES(?,SHA2(?,256))`, in.Owner, liveTrashScope+in.Owner); err != nil {
		t.Fatal(err)
	}
	// No preview job is dispatched by this storage-erasure probe.
	if _, err := db.Exec(`UPDATE album_photo_jobs SET state='attention' WHERE owner_id=?`, in.Owner); err != nil {
		t.Fatal(err)
	}
	if _, err := r.FinalizeDerivedBudget(ctx, DerivedVersionRef{in.Owner, in.Upload}, now); err != nil {
		t.Fatal(err)
	}
	setDerivedAssetTrash(t, db, u, true, now)
	// Only the isolated SQL fixture clock advances; COS signing uses real time.
	return now.Add(30 * 24 * time.Hour)
}

// Explicit operator probe: unique synthetic keys, no subscription grant, real
// COS requests. Preparation transfers ownership through a private durable intent.
// Unknown ordinary PUT/initiation outcomes retain that intent; never infer closure.
func TestLiveCOSTrashWorker(t *testing.T) {
	if os.Getenv("ALBUM_TRASH_COS_WORKER_PROBE") != "1" {
		t.Skip("explicit owned-resource erasure probe required")
	}
	phase := os.Getenv("ALBUM_TRASH_COS_PHASE")
	if phase != "prepare" && phase != "run" && phase != "cleanup" && phase != "status" && phase != "pending" && phase != "process" {
		t.Fatal("explicit preparation, run, status or cleanup phase required")
	}
	reader, root, ctx := liveRoundtripFixture(t)
	lease := completionProbeLease(t, root)
	cleaner := completionProbeCleaner(t)
	if phase == "pending" {
		var in liveTrashIntent
		roundtripRead(t, root, "intent.json", &in)
		if in.Scope != liveTrashScope || !validOwner(in.Owner) || !validOwner(in.Upload) || len(in.Keys) != 3 || in.Keys[0] != "albums/originals/"+in.Owner+"/"+in.Upload+"/photo" || in.Keys[1] != "albums/staging/"+in.Owner+"/"+in.Upload+"/photo" || in.Keys[2] != in.Keys[0]+"-neighbor" {
			t.Fatal("invalid pending ownership intent")
		}
		counts := map[string]any{}
		for _, key := range in.Keys {
			vs, err := cleaner.listPurgeVersions(ctx, in.Owner, purgeTarget{key})
			ids, e := cleaner.listPurgeUploads(ctx, in.Owner, purgeTarget{key})
			if err != nil || e != nil || len(vs)+len(ids) > 8 {
				t.Fatal("pending exact-key inventory unavailable")
			}
			counts[key] = map[string]any{"versions": vs, "uploads": ids}
		}
		roundtripJSON(t, root, "pending-result.json", map[string]any{"readOnly": true, "counts": counts, "writerClosureProven": false})
		return
	}
	if phase == "prepare" {
		prepareLiveTrash(t, ctx, root, reader, cleaner)
		return
	}
	in := liveTrashLoad(t, root)
	if phase == "process" {
		runLiveTrashProcessRecovery(t, ctx, root, lease, reader, cleaner, in)
		return
	}
	if phase == "run" {
		runLiveTrashWorker(t, ctx, root, reader, cleaner, in)
		return
	}
	s, markers := liveTrashInventory(t, ctx, reader, cleaner, in)
	if phase == "cleanup" {
		for _, key := range in.Keys {
			ids, err := s.listPurgeUploads(ctx, in.Owner, purgeTarget{key})
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range ids {
				if err := s.abortPurgeUpload(ctx, in.Owner, purgeTarget{key}, id); err != nil {
					t.Fatal(err)
				}
			}
			vs, err := s.listPurgeVersions(ctx, in.Owner, purgeTarget{key})
			if err != nil {
				t.Fatal(err)
			}
			for _, v := range vs {
				if err := s.deletePurgeVersion(ctx, in.Owner, purgeTarget{key}, v); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	counts := map[string]any{}
	for _, key := range in.Keys {
		vs, err := cleaner.listPurgeVersions(ctx, in.Owner, purgeTarget{key})
		ids, e := cleaner.listPurgeUploads(ctx, in.Owner, purgeTarget{key})
		if err != nil || e != nil {
			t.Fatal("final inventory unavailable")
		}
		counts[key] = map[string]int{"versionsAndMarkers": len(vs), "multipart": len(ids)}
		if phase == "cleanup" && len(vs)+len(ids) != 0 {
			t.Fatal("owned resources remain; retain intent")
		}
	}
	roundtripJSON(t, root, phase+"-result.json", map[string]any{"passed": true, "counts": counts, "markersBefore": markers, "exactOwnedKeysOnly": true})
}

var _ objectPurgeStorage = (*liveTrashStorage)(nil)
