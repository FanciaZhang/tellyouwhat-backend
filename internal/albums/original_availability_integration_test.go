package albums

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestMySQLOriginalAvailabilityRechecksAfterStorageIO(t *testing.T) {
	for _, mode := range []string{"available", "trashed-during-check", "disabled-during-check", "retention-ended-during-check"} {
		t.Run(mode, func(t *testing.T) {
			f, _, deadline := expiredReadFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			before, err := f.repo.CloudAccount(ctx, f.login.AccountID, *f.now)
			if err != nil {
				t.Fatal(err)
			}
			objects := &availabilityObjects{AlbumObjects: f.uploads.objects}
			f.uploads.objects = objects
			// One connection also catches accidentally running storage I/O inside
			// the account transaction; the simulated concurrent mutation must finish.
			f.db.SetMaxOpenConns(1)
			objects.check = func(SealedObject) error {
				switch mode {
				case "trashed-during-check":
					state, err := f.repo.CloudAsset(ctx, f.login.AccountID, f.version.AssetID, *f.now)
					if err != nil {
						return err
					}
					_, err = f.repo.MutateCloudAsset(ctx, f.login.AccountID, f.version.AssetID, "trash", CloudAssetMutation{uuid.NewString(), state.Revision}, *f.now)
					return err
				case "disabled-during-check":
					_, err := f.db.ExecContext(ctx, `UPDATE album_accounts SET state='disabled' WHERE account_id=?`, f.login.AccountID)
					return err
				case "retention-ended-during-check":
					*f.now = deadline
				}
				return nil
			}
			result, err := f.uploads.CheckOriginalAvailability(ctx, f.login.AccountID, f.version.VersionID)
			if ctx.Err() != nil {
				t.Fatal("storage check held the account lock", ctx.Err())
			}
			if len(objects.seen) != len(f.version.Objects) {
				t.Fatal("incomplete storage check")
			}
			if mode == "available" {
				if err != nil || result.ManifestDigest != f.version.ManifestDigest {
					t.Fatal(result, err)
				}
				after, err := f.repo.CloudAccount(ctx, f.login.AccountID, *f.now)
				if err != nil || after.UsedBytes != before.UsedBytes || after.ReservedBytes != before.ReservedBytes || after.Revision != before.Revision {
					t.Fatal("availability mutated quota or subscription", err)
				}
			} else if err == nil || result != (OriginalAvailability{}) {
				t.Fatal("stale lifecycle returned available", result, err)
			}
		})
	}
}
