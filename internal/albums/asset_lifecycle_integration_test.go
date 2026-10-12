package albums

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/migrations"
)

func lifecycleUpload(t *testing.T, f sharingFixture, source string) Upload {
	t.Helper()
	m := f.version.Manifest
	m.SourceRevision = source
	u, err := f.uploads.Create(context.Background(), f.login.AccountID, uuid.NewString(), m)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
func completeLifecycleUpload(t *testing.T, f sharingFixture, u Upload) (Upload, error) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.repo.Submit(ctx, u.OwnerID, u.ID, *f.now); err != nil {
		return Upload{}, err
	}
	token := uuid.NewString()
	if _, err := f.repo.Claim(ctx, u.OwnerID, u.ID, token, *f.now, f.now.Add(time.Minute)); err != nil {
		return Upload{}, err
	}
	var objects []SealedObject
	for _, r := range u.Manifest.Resources {
		objects = append(objects, SealedObject{r.ID, sealedKey(u, r.ID), "immutable-lifecycle-version", r.SizeBytes, r.SHA256})
	}
	return f.repo.Complete(ctx, u.OwnerID, u.ID, token, objects, *f.now)
}
func cloudState(t *testing.T, f sharingFixture) CloudAssetState {
	t.Helper()
	state, err := f.repo.CloudAsset(context.Background(), f.login.AccountID, f.version.AssetID, *f.now)
	if err != nil {
		t.Fatal(err)
	}
	return state
}
func mutateAsset(t *testing.T, f sharingFixture, action string, state CloudAssetState) CloudAssetState {
	t.Helper()
	out, err := f.repo.MutateCloudAsset(context.Background(), f.login.AccountID, f.version.AssetID, action, CloudAssetMutation{uuid.NewString(), state.Revision}, *f.now)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func TestMySQLCloudTrashRestoresEveryVersionAndRevokesSharing(t *testing.T) {
	f := newSharingFixture(t)
	ctx := context.Background()
	second := lifecycleUpload(t, f, "second-version")
	if _, err := completeLifecycleUpload(t, f, second); err != nil {
		t.Fatal(err)
	}
	before, _ := f.repo.CloudAccount(ctx, f.login.AccountID, *f.now)
	state := cloudState(t, f)
	if state.Revision != 2 || state.State != "active" {
		t.Fatal(state)
	}
	link, token := f.create(t, "", 10)
	claim, err := f.service.Claim(ctx, token, "", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	initial, err := f.repo.LibraryChanges(ctx, f.login.AccountID, 0, 100)
	if err != nil || len(initial.Changes) != 2 {
		t.Fatal("publication events", err, len(initial.Changes))
	}
	request := CloudAssetMutation{uuid.NewString(), state.Revision}
	trashed, err := f.repo.MutateCloudAsset(ctx, f.login.AccountID, f.version.AssetID, "trash", request, *f.now)
	if err != nil || trashed.State != "trashed" || trashed.Revision != 3 || trashed.TrashExpiresAt == nil || !trashed.TrashExpiresAt.Equal(f.now.Add(30*24*time.Hour)) {
		t.Fatal("trash", trashed, err)
	}
	reopened := NewMySQLUploads(f.db).WithSubscriptionEnvironment("Sandbox", func() time.Time { return *f.now })
	replay, err := reopened.MutateCloudAsset(ctx, f.login.AccountID, f.version.AssetID, "trash", request, f.now.Add(time.Second))
	a, _ := json.Marshal(trashed)
	b, _ := json.Marshal(replay)
	if err != nil || string(a) != string(b) {
		t.Fatal("replay extended or changed trash", err)
	}
	changes, err := reopened.LibraryChanges(ctx, f.login.AccountID, initial.NextSequence, 1)
	if err != nil || len(changes.Changes) != 1 || !changes.HasMore || changes.Changes[0].State != "trashed" || changes.Changes[0].Version != nil {
		t.Fatal("first tombstone", changes, err)
	}
	rest, err := reopened.LibraryChanges(ctx, f.login.AccountID, changes.NextSequence, 1)
	if err != nil || len(rest.Changes) != 1 || rest.HasMore || rest.Changes[0].VersionID == changes.Changes[0].VersionID {
		t.Fatal("remaining tombstone", rest, err)
	}
	if _, err := f.uploads.Get(ctx, f.login.AccountID, second.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("trash returned verified upload receipt", err)
	}
	if _, err := f.uploads.Submit(ctx, f.login.AccountID, second.ID); !errors.Is(err, ErrConflict) {
		t.Fatal("trash submit returned verified receipt", err)
	}
	for _, id := range []string{f.version.VersionID, second.ID} {
		if _, err := f.repo.VisibleAssetVersion(ctx, f.login.AccountID, id); !errors.Is(err, ErrNotFound) {
			t.Fatal("trash visible", err)
		}
		if _, err := f.repo.AssetVersion(ctx, f.login.AccountID, id); err != nil {
			t.Fatal("raw recovery evidence removed", err)
		}
		if err := f.repo.WithCloudReadGrant(ctx, f.login.AccountID, AssetVersion{VersionID: id}, *f.now, f.now.Add(time.Minute), func(time.Time) error { t.Error("trashed object signed"); return nil }); !errors.Is(err, ErrNotFound) {
			t.Fatal("trash grant", err)
		}
	}
	page, err := f.repo.AssetVersions(ctx, f.login.AccountID, 0, 100)
	if err != nil || len(page.Versions) != 0 {
		t.Fatal("legacy list exposed trash", err)
	}
	if _, err := f.service.ReadGrants(ctx, claim.Token); !errors.Is(err, ErrShareNotAvailable) {
		t.Fatal("old sharing claim usable", err)
	}
	restored := mutateAsset(t, f, "restore", trashed)
	if restored.Revision != 4 || restored.TrashedAt != nil {
		t.Fatal("restore", restored)
	}
	recovery, err := f.repo.LibraryChanges(ctx, f.login.AccountID, rest.NextSequence, 100)
	if err != nil || len(recovery.Changes) != 2 {
		t.Fatal("restored change count", err)
	}
	for _, c := range recovery.Changes {
		if c.State != "active" || c.Version == nil || c.Version.Manifest.AssetID != f.version.AssetID || c.Revision != 4 {
			t.Fatal("incomplete restore event")
		}
	}
	if _, err := f.service.Lookup(ctx, token); !errors.Is(err, ErrShareNotAvailable) {
		t.Fatal("restore reopened revoked share", link.ID, err)
	}
	after, _ := f.repo.CloudAccount(ctx, f.login.AccountID, *f.now)
	if after.UsedBytes != before.UsedBytes || after.ReservedBytes != before.ReservedBytes {
		t.Fatal("trash released actual storage quota")
	}
	if err := migrations.Run(ctx, f.db); err != nil {
		t.Fatal("migration replay", err)
	}
}
func TestMySQLCloudTrashFencesLateUploadEvenAfterRestore(t *testing.T) {
	f := newSharingFixture(t)
	ctx := context.Background()
	queued := lifecycleUpload(t, f, "late-version")
	trash := mutateAsset(t, f, "trash", cloudState(t, f))
	if _, err := f.uploads.Create(ctx, f.login.AccountID, uuid.NewString(), queued.Manifest); !errors.Is(err, ErrConflict) {
		t.Fatal("new upload into trash", err)
	}
	restored := mutateAsset(t, f, "restore", trash)
	if _, err := completeLifecycleUpload(t, f, queued); !errors.Is(err, ErrConflict) {
		t.Fatal("late upload republished", err)
	}
	if got := cloudState(t, f); got.Revision != restored.Revision {
		t.Fatal("failed publication advanced revision")
	}
	if _, err := f.uploads.Create(ctx, f.login.AccountID, queued.RequestID, queued.Manifest); !errors.Is(err, ErrConflict) {
		t.Fatal("old request resumed new epoch", err)
	}
	fresh, err := f.uploads.Create(ctx, f.login.AccountID, uuid.NewString(), queued.Manifest)
	if err != nil || fresh.ID == queued.ID || fresh.LifecycleEpoch == queued.LifecycleEpoch {
		t.Fatal("fresh intent could not restart after restore", err)
	}
	if _, err := completeLifecycleUpload(t, f, fresh); err != nil {
		t.Fatal(err)
	}
	if got := cloudState(t, f); got.Revision != restored.Revision+1 {
		t.Fatal("fresh version not published")
	}
}
func TestMySQLCloudTrashConflictsScopeAndConcurrentRequests(t *testing.T) {
	f := newSharingFixture(t)
	ctx := context.Background()
	state := cloudState(t, f)
	request := CloudAssetMutation{uuid.NewString(), state.Revision}
	var wg sync.WaitGroup
	var mu sync.Mutex
	success := 0
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.repo.MutateCloudAsset(ctx, f.login.AccountID, f.version.AssetID, "trash", request, *f.now)
			if err != nil {
				t.Error(err)
			} else {
				mu.Lock()
				success++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if success != 8 || cloudState(t, f).Revision != state.Revision+1 {
		t.Fatal("replay was not serialized", success)
	}
	for _, entry := range []struct {
		owner, asset, action string
		request              CloudAssetMutation
	}{
		{uuid.NewString(), f.version.AssetID, "trash", request},
		{f.login.AccountID, "missing-asset", "trash", request},
		{f.login.AccountID, f.version.AssetID, "restore", request},
		{f.login.AccountID, f.version.AssetID, "restore", CloudAssetMutation{uuid.NewString(), state.Revision}},
	} {
		if _, err := f.repo.MutateCloudAsset(ctx, entry.owner, entry.asset, entry.action, entry.request, *f.now); err == nil {
			t.Fatal("scope or stale revision accepted")
		}
	}
	foreign, err := f.repo.LibraryChanges(ctx, uuid.NewString(), 0, 100)
	if err != nil || len(foreign.Changes) != 0 {
		t.Fatal("foreign changes exposed", err)
	}
	encoded, _ := json.Marshal(foreign)
	if strings.Contains(string(encoded), "albums/originals/") {
		t.Fatal("provider keys exposed")
	}
}
func TestMySQLCloudTrashRestoreDeadlineIsNotExtendedByRenewal(t *testing.T) {
	f, proof, deadline := expiredReadFixture(t)
	ctx := context.Background()
	trash := mutateAsset(t, f, "trash", cloudState(t, f))
	if trash.RestoreUntil == nil || !trash.RestoreUntil.Equal(deadline) {
		t.Fatal("trash extended account retention")
	}
	*f.now = deadline
	if _, err := f.repo.MutateCloudAsset(ctx, f.login.AccountID, f.version.AssetID, "restore", CloudAssetMutation{uuid.NewString(), trash.Revision}, *f.now); err == nil {
		t.Fatal("restored past account cutoff")
	}
	proof.Active = true
	proof.ExpiresAt = f.now.AddDate(1, 0, 0)
	proof.SignedAt = f.now.Add(time.Millisecond)
	applyReminderSubscription(t, f.repo, f.login.AccountID, proof, *f.now)
	current := cloudState(t, f)
	if current.RestoreUntil == nil || !current.RestoreUntil.Equal(*trash.TrashExpiresAt) {
		t.Fatal("renewal did not retain original 30-day limit")
	}
	*f.now = *trash.TrashExpiresAt
	if _, err := f.repo.MutateCloudAsset(ctx, f.login.AccountID, f.version.AssetID, "restore", CloudAssetMutation{uuid.NewString(), trash.Revision}, *f.now); !errors.Is(err, ErrConflict) {
		t.Fatal("renewal reset trash retention", err)
	}
}

func TestMySQLCloudTrashRollsBackStateEventsAndSharesTogether(t *testing.T) {
	f := newSharingFixture(t)
	ctx := context.Background()
	state := cloudState(t, f)
	_, token := f.create(t, "", 5)
	before, err := f.repo.LibraryChanges(ctx, f.login.AccountID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	// Fail the final durable operation receipt, after changing state and shares.
	if _, err := f.db.Exec(`ALTER TABLE album_asset_operations ADD CONSTRAINT reject_asset_receipt CHECK(action='restore')`); err != nil {
		t.Fatal(err)
	}
	request := CloudAssetMutation{uuid.NewString(), state.Revision}
	if _, err := f.repo.MutateCloudAsset(ctx, f.login.AccountID, state.AssetID, "trash", request, *f.now); err == nil {
		t.Fatal("injected error ignored")
	}
	if _, err := f.db.Exec(`ALTER TABLE album_asset_operations DROP CHECK reject_asset_receipt`); err != nil {
		t.Fatal(err)
	}
	if got := cloudState(t, f); got.Revision != state.Revision || got.State != "active" {
		t.Fatal("partial state commit")
	}
	after, err := f.repo.LibraryChanges(ctx, f.login.AccountID, before.NextSequence, 100)
	if err != nil || len(after.Changes) != 0 || after.NextSequence != before.NextSequence {
		t.Fatal("partial event commit", err)
	}
	if _, err := f.service.Lookup(ctx, token); err != nil {
		t.Fatal("share revoked by rolled-back operation", err)
	}
	if _, err := f.repo.MutateCloudAsset(ctx, f.login.AccountID, state.AssetID, "trash", request, *f.now); err != nil {
		t.Fatal("retry after rollback", err)
	}
}

func TestMySQLCloudTrashDifferentRequestsHaveSingleWinner(t *testing.T) {
	f := newSharingFixture(t)
	state := cloudState(t, f)
	results := make(chan error, 8)
	for range 8 {
		go func() {
			_, err := f.repo.MutateCloudAsset(context.Background(), f.login.AccountID, state.AssetID, "trash", CloudAssetMutation{uuid.NewString(), state.Revision}, *f.now)
			results <- err
		}()
	}
	successes := 0
	for range 8 {
		if err := <-results; err == nil {
			successes++
		} else if !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatal("multiple intents won", successes)
	}
	f.db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := f.repo.LibraryChanges(ctx, f.login.AccountID, 0, 100); err != nil {
		t.Fatal("feed requires second connection", err)
	}
}

func TestMySQLCloudLifecycleMigrationBackfillsPublishedVersions(t *testing.T) {
	f := newSharingFixture(t)
	ctx := context.Background()
	second := lifecycleUpload(t, f, "before-lifecycle-migration")
	if _, err := completeLifecycleUpload(t, f, second); err != nil {
		t.Fatal(err)
	}
	// Recreate the prior schema around real published versions and upload rows.
	for _, query := range []string{
		`DROP TABLE album_asset_operations`, `DROP TABLE album_library_changes`, `DROP TABLE album_asset_lifecycle`,
		`ALTER TABLE album_uploads DROP INDEX album_upload_manifest, DROP COLUMN lifecycle_epoch, ADD UNIQUE KEY album_upload_manifest(owner_id,manifest_digest)`,
		`DELETE FROM schema_migrations WHERE name='0034_album_asset_lifecycle.sql'`,
	} {
		if _, err := f.db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrations.Run(ctx, f.db); err != nil {
		t.Fatal(err)
	}
	state := cloudState(t, f)
	if state.Revision != 2 || state.State != "active" {
		t.Fatal("backfill state", state)
	}
	page, err := f.repo.LibraryChanges(ctx, f.login.AccountID, 0, 100)
	if err != nil || len(page.Changes) != 2 {
		t.Fatal("backfill feed", err)
	}
	for i, c := range page.Changes {
		if c.Version == nil || c.State != "active" || c.Revision != int64(i+1) || c.AssetID != state.AssetID {
			t.Fatal("backfill lost version")
		}
	}
	restored := mutateAsset(t, f, "restore", mutateAsset(t, f, "trash", state))
	if restored.Revision != 4 {
		t.Fatal("migrated asset cannot restore")
	}
}

func TestMySQLCloudTrashRechecksClockAfterAccountLock(t *testing.T) {
	for _, boundary := range []string{"account", "trash"} {
		t.Run(boundary, func(t *testing.T) {
			f, _, accountDeadline := expiredReadFixture(t)
			if boundary == "trash" {
				f = newSharingFixture(t)
			}
			state := mutateAsset(t, f, "trash", cloudState(t, f))
			cutoff := accountDeadline
			if boundary == "trash" {
				cutoff = *state.TrashExpiresAt
				*f.now = cutoff.Add(-time.Second)
			}
			calls := 0
			f.repo.billingNow = func() time.Time {
				calls++
				if calls > 1 {
					return cutoff
				}
				return *f.now
			}
			if _, err := f.repo.MutateCloudAsset(context.Background(), f.login.AccountID, state.AssetID, "restore", CloudAssetMutation{uuid.NewString(), state.Revision}, *f.now); err == nil {
				t.Fatal("restored after cutoff crossed while acquiring lock")
			}
			var actual string
			if err := f.db.QueryRow(`SELECT state FROM album_asset_lifecycle WHERE owner_id=? AND asset_id=?`, f.login.AccountID, state.AssetID).Scan(&actual); err != nil || actual != "trashed" {
				t.Fatal("late restore changed state", err)
			}
		})
	}
}
