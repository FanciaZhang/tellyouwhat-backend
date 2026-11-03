package albums

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

type paidMultipartFixture struct {
	*multipartRepositoryStub
	deadline time.Time
	denied   error
}

func (r *paidMultipartFixture) CloudMediaGrantDeadline(context.Context, string, time.Time) (time.Time, error) {
	return r.deadline, r.denied
}

type paidLiveFixture struct {
	UploadRepository
	LivePairRepository
	pair     LivePairUpload
	deadline time.Time
	denied   error
}

func (r *paidLiveFixture) CloudMediaGrantDeadline(context.Context, string, time.Time) (time.Time, error) {
	return r.deadline, r.denied
}
func (r *paidLiveFixture) LivePairByID(_ context.Context, owner, id string) (LivePairUpload, error) {
	if r.pair.OwnerID != owner || r.pair.ID != id {
		return LivePairUpload{}, ErrNotFound
	}
	return r.pair, nil
}

type paidLiveObjects struct {
	AlbumObjects
	LivePairStorage
	expiries []time.Time
}

func (s *paidLiveObjects) AuthorizeLivePairPut(_ context.Context, _ LivePairUpload, _ string, expiry time.Time) (PutGrant, error) {
	s.expiries = append(s.expiries, expiry)
	return PutGrant{ExpiresAt: expiry}, nil
}

func TestAlbumSubscriptionBoundsMultipartAndLivePhotoAuthorization(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []string{"multipart-part", "multipart-init", "live-pair"} {
		for _, denied := range []bool{false, true} {
			name := kind + "-paid"
			if denied {
				name = kind + "-expired"
			}
			t.Run(name, func(t *testing.T) {
				uploads, repository, objects, owner, manifest := uploadFixture(t)
				u, err := uploads.Create(ctx, owner, uuid.NewString(), manifest)
				if err != nil {
					t.Fatal(err)
				}
				now := uploads.now()
				deadline := now.Add(time.Second)
				var rejection error
				if denied {
					rejection = ErrSubscriptionRequired
				}
				if kind == "live-pair" {
					pair := LivePairUpload{ID: uuid.NewString(), OwnerID: owner, State: "uploading", ExpiresAt: now.Add(time.Hour)}
					r := &paidLiveFixture{UploadRepository: repository, pair: pair, deadline: deadline, denied: rejection}
					o := &paidLiveObjects{AlbumObjects: objects}
					s, err := NewUploadService(r, o, uploads.policy, uploads.now)
					if err != nil {
						t.Fatal(err)
					}
					_, err = s.LivePairGrants(ctx, owner, pair.ID)
					if denied {
						if !errors.Is(err, ErrSubscriptionRequired) || len(o.expiries) != 0 {
							t.Fatal("expired Live Photo issued PUT", err)
						}
						return
					}
					if err != nil || len(o.expiries) != 2 {
						t.Fatal("Live Photo pair grants", err)
					}
					for _, expiry := range o.expiries {
						if !expiry.Equal(deadline) {
							t.Fatal("Live Photo PUT outlived capacity")
						}
					}
					return
				}
				r := &paidMultipartFixture{multipartRepositoryStub: &multipartRepositoryStub{uploadTestRepository: repository}, deadline: deadline, denied: rejection}
				calls := 0
				p := &multipartProviderStub{init: func(context.Context, string) (string, error) { calls++; return "stored-id", nil }, part: func(_ context.Context, _ string, _ string, _ Resource, n int, expiry time.Time) (MultipartPartGrant, error) {
					calls++
					if !expiry.Equal(deadline) {
						t.Fatal("part grant outlived capacity")
					}
					return MultipartPartGrant{PartNumber: n}, nil
				}}
				if kind == "multipart-part" {
					r.intent = MultipartIntent{UploadID: u.ID, ResourceID: "photo", IntentID: uuid.NewString(), State: "ready", ProviderUploadID: "stored-id"}
				}
				s, _ := NewMultipartService(r, p, uploads.now)
				if kind == "multipart-part" {
					_, err = s.Part(ctx, owner, u.ID, "photo", 1)
				} else {
					_, err = s.Initialize(ctx, owner, u.ID, "photo")
				}
				if denied {
					if !errors.Is(err, ErrSubscriptionRequired) || calls != 0 {
						t.Fatal("expired multipart reached provider", err)
					}
					return
				}
				if err != nil || calls != 1 {
					t.Fatal("paid multipart did not reach provider once", err, calls)
				}
			})
		}
	}
}
