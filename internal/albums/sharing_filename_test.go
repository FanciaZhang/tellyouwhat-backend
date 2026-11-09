package albums

import "testing"

func TestAlbumSharingFilenameRemovesPathsControlsAndBidiSpoofing(t *testing.T) {
	resource := Resource{ID: "photo", Descriptor: &ResourceDescriptor{Filename: "../folder\\回忆\u202eMOV.exe\r\n"}}
	if got := sharedFilename(resource); got != "回忆_MOV.exe__" {
		t.Fatal("unsafe attachment filename", got)
	}
	resource.Descriptor.Filename = ".."
	if got := sharedFilename(resource); got != "photo.bin" {
		t.Fatal("missing descriptive fallback")
	}
}
