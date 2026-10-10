package albums

import (
	"context"
	"github.com/google/uuid"
	"strings"
	"testing"
)

type photoRenderer struct {
	*objectTestStore
	selected SealedObject
	calls    int
}

func (r *photoRenderer) RenderPhotoCandidate(_ context.Context, source SealedObject, _ PhotoPreset) (PhotoCandidate, error) {
	r.calls++
	r.selected = source
	return PhotoCandidate{Format: "jpeg"}, nil
}
func TestPhotoCandidateSelectsDisplayedVersionAndRejectsIncompleteBackup(t *testing.T) {
	for _, edited := range []bool{false, true} {
		service, repo, signer := readFixture(t)
		renderer := &photoRenderer{objectTestStore: signer.objectTestStore}
		service.objects = renderer
		expected := repo.version.Objects[0]
		if edited {
			current := repo.version.Manifest.Resources[0]
			current.ID = "current"
			current.Role = CurrentPhoto
			current.SHA256 = strings.Repeat("e", 64)
			repo.version.Manifest.Edited = true
			repo.version.Manifest.Resources = append(repo.version.Manifest.Resources, current)
			expected = SealedObject{ResourceID: current.ID, Key: sealedKey(Upload{ID: repo.version.VersionID, OwnerID: repo.owner}, current.ID), VersionID: "edited-immutable-version", SizeBytes: current.SizeBytes, SHA256: current.SHA256}
			repo.version.Objects = append(repo.version.Objects, expected)
			repo.version.OriginalBytes += current.SizeBytes
			repo.version.ManifestDigest, _ = repo.version.Manifest.Digest()
		}
		result, err := service.PhotoCandidate(context.Background(), repo.owner, repo.version.VersionID, PhotoGridJPEG)
		if err != nil || result.Format != "jpeg" || renderer.calls != 1 || renderer.selected != expected {
			t.Fatalf("wrong displayed input (edited=%v): %v", edited, err)
		}
		for _, mutate := range []func(){
			func() { repo.version.Objects[0].SHA256 = strings.Repeat("f", 64) },
			func() { repo.version.Objects[0].VersionID = "null" },
			func() { repo.version.Objects = nil },
		} {
			mutate()
			if _, err := service.PhotoCandidate(context.Background(), repo.owner, repo.version.VersionID, PhotoGridJPEG); err == nil || renderer.calls != 1 {
				t.Fatal("incomplete original set allowed processing")
			}
		}
	}
}
func TestPhotoCandidateDoesNotProcessOtherAccountsOrUnknownPresets(t *testing.T) {
	service, repo, signer := readFixture(t)
	renderer := &photoRenderer{objectTestStore: signer.objectTestStore}
	service.objects = renderer
	for _, request := range []struct {
		owner, version string
		preset         PhotoPreset
	}{
		{uuid.NewString(), repo.version.VersionID, PhotoDailyJPEG},
		{repo.owner, uuid.NewString(), PhotoDailyJPEG},
		{repo.owner, repo.version.VersionID, PhotoPreset("imageMogr2/arbitrary")},
	} {
		if _, err := service.PhotoCandidate(context.Background(), request.owner, request.version, request.preset); err == nil {
			t.Fatal("unauthorized processing accepted")
		}
	}
	repo.version.Manifest.Kind = Video
	repo.version.Manifest.Resources[0].Role = OriginalVideo
	repo.version.ManifestDigest, _ = repo.version.Manifest.Digest()
	if _, err := service.PhotoCandidate(context.Background(), repo.owner, repo.version.VersionID, PhotoGridJPEG); err == nil {
		t.Fatal("video processed as still")
	}
	if renderer.calls != 0 {
		t.Fatal("invalid request reached renderer")
	}
}
