package recommend

import (
	"math/rand"
	"testing"
)

func TestClaimRandomMissingAlbumClaimsWholeAlbum(t *testing.T) {
	s := &Service{
		photosByID: map[string]PhotoRecord{
			imageID("album-a", 0): {ImageID: imageID("album-a", 0), AlbumID: "album-a", PhotoIndex: 0},
			imageID("album-a", 1): {ImageID: imageID("album-a", 1), AlbumID: "album-a", PhotoIndex: 1},
			imageID("album-b", 0): {ImageID: imageID("album-b", 0), AlbumID: "album-b", PhotoIndex: 0},
		},
		missingByAlbum: map[string]map[int]struct{}{
			"album-a": {
				0: {},
				1: {},
			},
			"album-b": {
				0: {},
			},
		},
		rng: rand.New(rand.NewSource(1)),
	}

	initialMissing := map[string]int{
		"album-a": len(s.missingByAlbum["album-a"]),
		"album-b": len(s.missingByAlbum["album-b"]),
	}

	albumID, photos, ok := s.claimRandomMissingAlbum()
	if !ok {
		t.Fatalf("expected album claim")
	}
	if albumID == "" {
		t.Fatalf("expected claimed album id")
	}
	if len(photos) != initialMissing[albumID] {
		t.Fatalf("expected %d claimed photos for %s, got %d", initialMissing[albumID], albumID, len(photos))
	}
	for _, photo := range photos {
		if photo.AlbumID != albumID {
			t.Fatalf("claimed mixed album photos, got %s want %s", photo.AlbumID, albumID)
		}
	}
	if len(s.missingByAlbum[albumID]) != 0 {
		t.Fatalf("expected claimed album missing set to be empty")
	}

	otherAlbum := "album-a"
	if albumID == "album-a" {
		otherAlbum = "album-b"
	}
	if len(s.missingByAlbum[otherAlbum]) != initialMissing[otherAlbum] {
		t.Fatalf("expected unclaimed album missing count to stay %d, got %d", initialMissing[otherAlbum], len(s.missingByAlbum[otherAlbum]))
	}
}

func TestClaimRandomMissingAlbumSkipsStalePhotoRecords(t *testing.T) {
	s := &Service{
		photosByID: map[string]PhotoRecord{},
		missingByAlbum: map[string]map[int]struct{}{
			"album-a": {
				0: {},
			},
		},
		rng: rand.New(rand.NewSource(1)),
	}

	albumID, photos, ok := s.claimRandomMissingAlbum()
	if ok {
		t.Fatalf("expected no claim, got album=%s photos=%d", albumID, len(photos))
	}
	if len(s.missingByAlbum["album-a"]) != 0 {
		t.Fatalf("expected stale missing entries to be cleared after claim attempt")
	}
}

func TestRequeueMissingPhotosRestoresPendingSet(t *testing.T) {
	s := &Service{
		photosByID: map[string]PhotoRecord{
			imageID("album-a", 0): {ImageID: imageID("album-a", 0), AlbumID: "album-a", PhotoIndex: 0},
			imageID("album-a", 1): {ImageID: imageID("album-a", 1), AlbumID: "album-a", PhotoIndex: 1},
		},
		failedByID: map[string]string{
			imageID("album-a", 0): "old failure",
			imageID("album-a", 1): "old failure",
		},
		missingByAlbum: map[string]map[int]struct{}{
			"album-a": {},
		},
	}

	s.requeueMissingPhotos("album-a", []int{0, 1, 999})

	if _, ok := s.missingByAlbum["album-a"][0]; !ok {
		t.Fatalf("expected photo 0 to be requeued")
	}
	if _, ok := s.missingByAlbum["album-a"][1]; !ok {
		t.Fatalf("expected photo 1 to be requeued")
	}
	if _, ok := s.missingByAlbum["album-a"][999]; ok {
		t.Fatalf("did not expect unknown photo to be requeued")
	}
	if _, ok := s.failedByID[imageID("album-a", 0)]; ok {
		t.Fatalf("expected stale failure marker for photo 0 to be removed")
	}
	if _, ok := s.failedByID[imageID("album-a", 1)]; ok {
		t.Fatalf("expected stale failure marker for photo 1 to be removed")
	}
}
