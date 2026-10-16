package albums

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
)

func sharingDeletionAccounts(t *testing.T, f sharingFixture) *MySQLAccounts {
	t.Helper()
	cipher, err := mysqlstore.NewPayloadCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{41}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewMySQLAccounts(albumDevelopmentAudience, f.db, cipher)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestMySQLAccountDeletionFencesMediaAndKeepsLateReceipts(t *testing.T) {
	f := newSharingFixture(t)
	ctx, owner, now := context.Background(), f.login.AccountID, *f.now
	r := f.repo
	u := lifecycleUpload(t, f, "staging")
	intent, _, err := r.BeginMultipart(ctx, owner, u.ID, "photo", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.ResolveMultipart(ctx, owner, u.ID, "photo", intent.IntentID, "provider-upload", now); err != nil {
		t.Fatal(err)
	}
	completion, _, err := r.ClaimMultipartCompletion(ctx, owner, u.ID, "photo", now)
	if err != nil {
		t.Fatal(err)
	}
	partsUpload := lifecycleUpload(t, f, "part-grant")
	partIntent, _, err := r.BeginMultipart(ctx, owner, partsUpload.ID, "photo", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.ResolveMultipart(ctx, owner, partsUpload.ID, "photo", partIntent.IntentID, "part-provider", now); err != nil {
		t.Fatal(err)
	}
	partCalls := 0
	provider := &multipartProviderStub{part: func(context.Context, string, string, Resource, int, time.Time) (MultipartPartGrant, error) {
		partCalls++
		return MultipartPartGrant{}, nil
	}}
	parts, _ := NewMultipartService(r, provider, func() time.Time { return now })
	if _, err = parts.Part(ctx, owner, partsUpload.ID, "photo", 1); err != nil || partCalls != 1 {
		t.Fatal("active part signing failed", err)
	}
	verifying := lifecycleUpload(t, f, "verifying")
	if _, err = r.Submit(ctx, owner, verifying.ID, now); err != nil {
		t.Fatal(err)
	}
	verifying, err = r.Claim(ctx, owner, verifying.ID, uuid.NewString(), now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, fresh, err := r.BeginOriginalSeal(ctx, verifying, "photo", now); err != nil || !fresh {
		t.Fatal("seal not prepared", err)
	}
	queued := lifecycleUpload(t, f, "queued")
	if _, err = r.Submit(ctx, owner, queued.ID, now); err != nil {
		t.Fatal(err)
	}
	sealWaiting := lifecycleUpload(t, f, "seal-waiting")
	if _, err = r.Submit(ctx, owner, sealWaiting.ID, now); err != nil {
		t.Fatal(err)
	}
	sealWaiting, err = r.Claim(ctx, owner, sealWaiting.ID, uuid.NewString(), now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	state := cloudState(t, f)
	_, shareToken := f.create(t, "", 2)
	if _, err := sharingDeletionAccounts(t, f).requestAccountDeletion(ctx, f.session.Token, uuid.NewString(), func() time.Time { return now }); err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		name string
		run  func() error
		want error
	}{
		{"reserve", func() error { _, e := f.uploads.Create(ctx, owner, uuid.NewString(), u.Manifest); return e }, ErrOwner},
		{"put-grants", func() error { _, e := f.uploads.Grants(ctx, owner, u.ID); return e }, ErrOwner},
		{"submit", func() error { _, e := r.Submit(ctx, owner, u.ID, now); return e }, ErrOwner},
		{"part-grant", func() error { _, e := parts.Part(ctx, owner, partsUpload.ID, "photo", 1); return e }, ErrOwner},
		{"multipart-start", func() error { _, _, e := r.BeginMultipart(ctx, owner, u.ID, "photo", now); return e }, ErrOwner},
		{"multipart-complete", func() error { _, _, e := r.ClaimMultipartCompletion(ctx, owner, u.ID, "photo", now); return e }, ErrOwner},
		{"verification-claim", func() error {
			_, e := r.Claim(ctx, owner, queued.ID, uuid.NewString(), now, now.Add(time.Minute))
			return e
		}, ErrOwner},
		{"new-copy-intent", func() error { _, _, e := r.BeginOriginalSeal(ctx, sealWaiting, "photo", now); return e }, ErrOwner},
		{"publish", func() error {
			res := verifying.Manifest.Resources[0]
			_, e := r.Complete(ctx, owner, verifying.ID, verifying.LeaseToken, []SealedObject{{res.ID, sealedKey(verifying, res.ID), "late-copy", res.SizeBytes, res.SHA256}}, now)
			return e
		}, ErrOwner},
		{"share-create", func() error {
			_, e := f.service.Create(ctx, owner, f.version.VersionID, ShareCreateInput{uuid.NewString(), 3600, 1, ""})
			return e
		}, ErrShareNotAvailable},
		{"share-claim", func() error { _, e := f.service.Claim(ctx, shareToken, "", uuid.NewString()); return e }, ErrShareNotAvailable},
		{"trash", func() error {
			_, e := r.MutateCloudAsset(ctx, owner, f.version.AssetID, "trash", CloudAssetMutation{uuid.NewString(), state.Revision}, now)
			return e
		}, ErrNotFound},
		{"restore", func() error {
			_, e := r.MutateCloudAsset(ctx, owner, f.version.AssetID, "restore", CloudAssetMutation{uuid.NewString(), state.Revision}, now)
			return e
		}, ErrNotFound},
		{"read-grant", func() error {
			return r.WithCloudReadGrant(ctx, owner, f.version, now, now.Add(time.Minute), func(time.Time) error { t.Fatal("read signed after deletion"); return nil })
		}, ErrNotFound},
	}
	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			if err := c.run(); !errors.Is(err, c.want) {
				t.Fatal("cutover failed", err)
			}
		})
	}
	if partCalls != 1 {
		t.Fatal("part signed after cutover")
	}
	if jobs, err := r.Pending(ctx, now, 100); err != nil || len(jobs) != 0 {
		t.Fatal("deleted owner still queued", len(jobs), err)
	}
	if jobs, err := r.PendingPhotoJobs(ctx, now, 100); err != nil || len(jobs) != 0 {
		t.Fatal("deleted owner still queued for photos", len(jobs), err)
	}
	if err := r.RecordOriginalSeal(ctx, verifying, "photo", "late-copy"); err != nil {
		t.Fatal("late copy receipt lost", err)
	}
	if _, err := r.ResolveMultipart(ctx, owner, u.ID, "photo", intent.IntentID, "provider-upload", now); err != nil {
		t.Fatal("late initialization receipt lost", err)
	}
	if _, err := r.ResolveMultipartCompletion(ctx, owner, u.ID, "photo", completion.OperationID, "assembled", "late-assembly", now); err != nil {
		t.Fatal("late assembly receipt lost", err)
	}
	if _, err := r.AssetVersion(ctx, owner, verifying.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("late original was published", err)
	}
	if _, err := r.AssetVersion(ctx, owner, f.version.VersionID); err != nil {
		t.Fatal("cleanup lost access to original catalogue", err)
	}
}

func TestMySQLUploadGrantRevalidatesParentAndMultipartBinding(t *testing.T) {
	for _, mode := range []string{"digest", "submitted", "multipart-rebound", "multipart-completing", "signer-error", "expires-during-signing"} {
		t.Run(mode, func(t *testing.T) {
			f := newSharingFixture(t)
			ctx := context.Background()
			u := lifecycleUpload(t, f, "grant-check")
			resource, provider := "", ""
			if strings.HasPrefix(mode, "multipart") {
				resource, provider = "photo", "provider-id"
				intent, _, err := f.repo.BeginMultipart(ctx, u.OwnerID, u.ID, resource, *f.now)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = f.repo.ResolveMultipart(ctx, u.OwnerID, u.ID, resource, intent.IntentID, provider, *f.now); err != nil {
					t.Fatal(err)
				}
			}
			switch mode {
			case "digest":
				u.Digest = strings.Repeat("f", 64)
			case "submitted":
				if _, err := f.repo.Submit(ctx, u.OwnerID, u.ID, *f.now); err != nil {
					t.Fatal(err)
				}
			case "multipart-rebound":
				provider = "different-provider"
			case "multipart-completing":
				if _, _, err := f.repo.ClaimMultipartCompletion(ctx, u.OwnerID, u.ID, resource, *f.now); err != nil {
					t.Fatal(err)
				}
			}
			called := false
			signingError := errors.New("local signing failed")
			expiry := f.now.Add(time.Minute)
			err := f.repo.WithUploadGrant(ctx, u, resource, provider, *f.now, expiry, func(time.Time) error {
				called = true
				if mode == "signer-error" {
					return signingError
				}
				if mode == "expires-during-signing" {
					*f.now = expiry
					return nil
				}
				t.Fatal("invalid parent reached signer")
				return nil
			})
			want := ErrConflict
			if mode == "signer-error" {
				want = signingError
			} else if mode == "expires-during-signing" {
				want = ErrExpired
			}
			if !errors.Is(err, want) || called != (mode == "signer-error" || mode == "expires-during-signing") {
				t.Fatal("invalid signing outcome", called, err)
			}
		})
	}
}

func deleteProtocolFixtureOwner(t *testing.T, f *pushFixture, owner string) {
	t.Helper()
	f.now = time.Now().UTC().Truncate(time.Microsecond)
	subject := uuid.NewString()
	if _, err := f.r.db.Exec(`INSERT INTO album_accounts(account_id,apple_subject_sha256) VALUES(?,?)`, owner, appleSubjectHash(albumDevelopmentAudience, subject)); err != nil {
		t.Fatal(err)
	}
	s, _ := f.login(t, subject, f.now)
	if s.AccountID != owner {
		t.Fatal("protocol fixture account mismatch")
	}
	acceptAppleDeletion(t, f, s.Token)
}

func TestMySQLAccountDeletionFencesDerivedWorkButRetainsReceipts(t *testing.T) {
	t.Run("photo", func(t *testing.T) {
		f := newPushFixture(t)
		u, candidate := photoJournalUpload(t, f.r.db)
		r, ctx, now := NewMySQLUploads(f.r.db), context.Background(), time.Now().UTC()
		spec := photoJournalSpec(u, candidate, PhotoDailyPNG)
		prior, _, err := r.BeginPhotoWrite(ctx, u.ID, u.Digest, spec, now)
		if err != nil {
			t.Fatal(err)
		}
		jobs, err := r.PendingPhotoJobs(ctx, now, 100)
		if err != nil || len(jobs) != 2 {
			t.Fatal("missing fixture jobs", err)
		}
		deleteProtocolFixtureOwner(t, f, u.OwnerID)
		if _, fresh, err := r.BeginPhotoWrite(ctx, u.ID, u.Digest, photoJournalSpec(u, candidate, PhotoGridPNG), now); !errors.Is(err, ErrOwner) || fresh {
			t.Fatal("new photo write permitted", err)
		}
		if _, err := r.ClaimPhotoJob(ctx, u.OwnerID, jobs[0].ID, uuid.NewString(), now); !errors.Is(err, ErrOwner) {
			t.Fatal("new photo lease permitted", err)
		}
		if _, err := r.CompletePhotoWrite(ctx, prior, photoJournalReceipt(spec), now); err != nil {
			t.Fatal("late photo receipt lost", err)
		}
		if jobs, err := r.PendingPhotoJobs(ctx, now, 100); err != nil || len(jobs) != 0 {
			t.Fatal("deleted photo owner still scheduled", err)
		}
	})
	for _, submitted := range []bool{false, true} {
		name := "video-new"
		if submitted {
			name = "video-submitted"
		}
		t.Run(name, func(t *testing.T) {
			f := newPushFixture(t)
			u, job := videoJournalUpload(t, f.r.db)
			r, ctx, now := NewMySQLUploads(f.r.db), context.Background(), time.Now().UTC()
			job, err := r.ClaimVideoJob(ctx, u.OwnerID, job.ID, uuid.NewString(), now)
			if err != nil {
				t.Fatal(err)
			}
			if submitted {
				job, _, err = r.BeginVideoSubmission(ctx, job, journalVideoSpec(u), now)
				if err != nil {
					t.Fatal(err)
				}
			}
			deleteProtocolFixtureOwner(t, f, u.OwnerID)
			if submitted {
				job, err = r.AttachVideoSubmission(ctx, job, "j"+strings.Repeat("b", 32), now)
				if err != nil {
					t.Fatal("late CI job ID lost", err)
				}
				if _, err = r.CompleteVideoJob(ctx, job, journalVideoResult(job), now); err != nil {
					t.Fatal("late video receipt lost", err)
				}
			} else if _, fresh, err := r.BeginVideoSubmission(ctx, job, journalVideoSpec(u), now); !errors.Is(err, ErrOwner) || fresh {
				t.Fatal("new paid CI submission permitted", err)
			}
			if jobs, err := r.PendingVideoJobs(ctx, now.Add(time.Hour), 100); err != nil || len(jobs) != 0 {
				t.Fatal("deleted video owner still scheduled", err)
			}
		})
	}
	for _, queued := range []bool{false, true} {
		name := "live-uploading"
		if queued {
			name = "live-queued"
		}
		t.Run(name, func(t *testing.T) {
			f := newPushFixture(t)
			u, descriptor := livePairJournalFixture(t, f.r.db)
			r, ctx, now := NewMySQLUploads(f.r.db), context.Background(), time.Now().UTC()
			_, _, objects, _, _ := uploadFixture(t)
			storage := &paidLiveObjects{AlbumObjects: objects}
			s, err := NewUploadService(r, storage, UploadPolicy{1 << 30, 1 << 20, time.Hour, time.Minute}, func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			pair, err := s.CreateLivePair(ctx, u.OwnerID, u.ID, uuid.NewString(), descriptor)
			if err != nil {
				t.Fatal(err)
			}
			if queued {
				if _, err = r.SubmitLivePair(ctx, u.OwnerID, pair.ID, now); err != nil {
					t.Fatal(err)
				}
			}
			deleteProtocolFixtureOwner(t, f, u.OwnerID)
			if queued {
				if jobs, err := r.PendingLivePairs(ctx, now, 100); err != nil || len(jobs) != 0 {
					t.Fatal("deleted live owner still scheduled", err)
				}
				if _, err := r.ClaimLivePair(ctx, u.OwnerID, pair.ID, uuid.NewString(), now, time.Minute); !errors.Is(err, ErrOwner) {
					t.Fatal("new pair claim permitted", err)
				}
			} else {
				if _, err := s.LivePairGrants(ctx, u.OwnerID, pair.ID); !errors.Is(err, ErrOwner) || len(storage.expiries) != 0 {
					t.Fatal("pair PUT signed after deletion", err)
				}
				if _, err := r.SubmitLivePair(ctx, u.OwnerID, pair.ID, now); !errors.Is(err, ErrOwner) {
					t.Fatal("pair submitted after deletion", err)
				}
			}
		})
	}
}

func TestMySQLAccountDeletionFenceIgnoresOldSnapshotAndRollsBack(t *testing.T) {
	f := newPushFixture(t)
	s, _ := f.login(t, "delete", f.now)
	ctx := context.Background()
	// Establish a repeatable-read snapshot before deletion, without taking locks.
	tx, err := f.r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var old sql.NullTime
	if err = tx.QueryRow(`SELECT deletion_requested_at FROM album_storage_accounts WHERE owner_id=?`, s.AccountID).Scan(&old); err != nil || old.Valid {
		t.Fatal(err)
	}
	if _, err = f.r.db.Exec(`ALTER TABLE album_share_links RENAME COLUMN revoked_at TO unavailable_revoked_at`); err != nil {
		t.Fatal(err)
	}
	if _, err = f.accounts.requestAccountDeletion(ctx, s.Token, uuid.NewString(), func() time.Time { return f.now }); err == nil {
		t.Fatal("failure not injected")
	}
	if _, err = f.r.db.Exec(`ALTER TABLE album_share_links RENAME COLUMN unavailable_revoked_at TO revoked_at`); err != nil {
		t.Fatal(err)
	}
	if err = f.r.db.QueryRow(`SELECT deletion_requested_at FROM album_storage_accounts WHERE owner_id=?`, s.AccountID).Scan(&old); err != nil || old.Valid {
		t.Fatal("failed request left fence", err)
	}
	acceptAppleDeletion(t, f, s.Token)
	if err = tx.QueryRow(`SELECT deletion_requested_at FROM album_storage_accounts WHERE owner_id=?`, s.AccountID).Scan(&old); err != nil || old.Valid {
		t.Fatal("test did not retain old snapshot", err)
	}
	if err = requireMediaAccount(ctx, tx, s.AccountID); !errors.Is(err, ErrOwner) {
		t.Fatal("old snapshot bypassed fence", err)
	}
}

func TestMySQLAccountDeletionSerializesUploadGrantSigning(t *testing.T) {
	for _, multipart := range []bool{false, true} {
		name := "put"
		if multipart {
			name = "part"
		}
		t.Run(name, func(t *testing.T) {
			f := newSharingFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			u := lifecycleUpload(t, f, "grant-race")
			resource, provider := "", ""
			if multipart {
				resource, provider = "photo", "multipart-id"
				intent, _, err := f.repo.BeginMultipart(ctx, u.OwnerID, u.ID, resource, *f.now)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = f.repo.ResolveMultipart(ctx, u.OwnerID, u.ID, resource, intent.IntentID, provider, *f.now); err != nil {
					t.Fatal(err)
				}
			}
			entered, release := make(chan struct{}), make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			grantDone := make(chan error, 1)
			go func() {
				grantDone <- f.repo.WithUploadGrant(ctx, u, resource, provider, *f.now, f.now.Add(time.Minute), func(time.Time) error {
					close(entered)
					select {
					case <-release:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				})
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("signer not reached")
			}
			accounts := sharingDeletionAccounts(t, f)
			deletionDone := make(chan error, 1)
			go func() {
				_, err := accounts.requestAccountDeletion(ctx, f.session.Token, uuid.NewString(), func() time.Time { return *f.now })
				deletionDone <- err
			}()
			select {
			case err := <-deletionDone:
				t.Fatal("deletion crossed active signer", err)
			case <-time.After(40 * time.Millisecond):
			}
			close(release)
			if err := <-grantDone; err != nil {
				t.Fatal(err)
			}
			if err := <-deletionDone; err != nil {
				t.Fatal(err)
			}
			called := false
			if err := f.repo.WithUploadGrant(ctx, u, resource, provider, *f.now, f.now.Add(time.Minute), func(time.Time) error { called = true; return nil }); !errors.Is(err, ErrOwner) || called {
				t.Fatal("signed after cutover", err)
			}
		})
	}
}

func TestMySQLAccountDeletionDoesNotReopenOnSubscriptionNotification(t *testing.T) {
	f := newSharingFixture(t)
	ctx, owner, now := context.Background(), f.login.AccountID, *f.now
	accounts := sharingDeletionAccounts(t, f)
	if _, err := accounts.requestAccountDeletion(ctx, f.session.Token, uuid.NewString(), func() time.Time { return now }); err != nil {
		t.Fatal(err)
	}
	account, err := f.repo.CloudAccount(ctx, owner, now)
	if err != nil {
		t.Fatal(err)
	}
	v := VerifiedAlbumSubscription{uuid.NewString(), uuid.NewString(), Cloud50GBAnnualProduct, "Sandbox", now.AddDate(1, 0, 0), now, true}
	if _, err = f.repo.ApplySubscription(ctx, owner, account.Revision, v, now); !errors.Is(err, ErrOwner) {
		t.Fatal("client subscription bypassed deletion", err)
	}
	n := albumNotificationReceipt{ID: uuid.NewString(), Owner: owner, Original: v.OriginalTransactionID, Environment: "Sandbox", Kind: "DID_RENEW", Digest: [32]byte{1}, SignedAt: now}
	if err = f.repo.ApplySubscriptionNotification(ctx, n, account.Revision, v, now); err != nil {
		t.Fatal("verified billing observation lost", err)
	}
	var pending int
	if err = f.db.QueryRow(`SELECT COUNT(*) FROM album_retention_reminders WHERE owner_id=? AND state='pending'`, owner).Scan(&pending); err != nil || pending != 0 {
		t.Fatal("notification recreated reminders", err)
	}
	if _, err = f.uploads.Create(ctx, owner, uuid.NewString(), f.version.Manifest); !errors.Is(err, ErrOwner) {
		t.Fatal("renewal reopened media writes", err)
	}
}

func TestMySQLAccountDeletionStorageFenceMigrationBackfill(t *testing.T) {
	f := newPushFixture(t)
	s, _ := f.login(t, "delete", f.now)
	other, _ := f.login(t, "keep", f.now)
	acceptAppleDeletion(t, f, s.Token)
	if _, err := f.r.db.Exec(`ALTER TABLE album_storage_accounts DROP CHECK album_subscription_completion_receipt, DROP COLUMN deletion_requested_at`); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("../../migrations/0043_album_deletion_storage_fence.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range strings.Split(string(data), ";") {
		if strings.TrimSpace(q) != "" {
			if _, err = f.r.db.Exec(q); err != nil {
				t.Fatal(err)
			}
		}
	}
	var count int
	if err = f.r.db.QueryRow(`SELECT COUNT(*) FROM album_storage_accounts WHERE owner_id=? AND deletion_requested_at=?`, s.AccountID, f.now).Scan(&count); err != nil || count != 1 {
		t.Fatal("existing deletion not fenced", err)
	}
	if err = f.r.db.QueryRow(`SELECT COUNT(*) FROM album_storage_accounts WHERE owner_id=? AND deletion_requested_at IS NULL`, other.AccountID).Scan(&count); err != nil || count != 1 {
		t.Fatal("unrelated storage fenced", err)
	}
}
