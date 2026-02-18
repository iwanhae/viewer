package recommend

import (
	"context"
	"math/rand"
	"testing"

	"viewer/internal/models"
)

func TestApplyAlbumIndexReplacesOnlyTargetAlbumRecords(t *testing.T) {
	s := &Service{
		photosByID: map[string]PhotoRecord{
			imageID("album-a", 0): {ImageID: imageID("album-a", 0), AlbumID: "album-a", PhotoIndex: 0},
			imageID("album-a", 1): {ImageID: imageID("album-a", 1), AlbumID: "album-a", PhotoIndex: 1},
			imageID("album-b", 0): {ImageID: imageID("album-b", 0), AlbumID: "album-b", PhotoIndex: 0},
		},
		photoIDsByAlbum: map[string]map[string]struct{}{
			"album-a": {
				imageID("album-a", 0): {},
				imageID("album-a", 1): {},
			},
			"album-b": {
				imageID("album-b", 0): {},
			},
		},
		embeddingsByID: map[string]EmbeddingRecord{
			imageID("album-a", 0): {ImageID: imageID("album-a", 0), Vector: []float32{1, 0}},
			imageID("album-b", 0): {ImageID: imageID("album-b", 0), Vector: []float32{0, 1}},
		},
		failedByID: map[string]string{
			imageID("album-a", 1): "old failure",
		},
		missingByAlbum: make(map[string]map[int]struct{}),
	}

	s.applyAlbumIndex(models.AlbumIndex{
		AlbumID: "album-a",
		Photos: []models.PhotoMeta{
			{I: 2, Name: "new.jpg", W: 100, H: 100, Ratio: 1},
		},
		Embeddings: map[string]models.PhotoEmbedding{
			"2": {
				Status: embeddingStatusReady,
				Vector: []float32{1, 1},
				Model:  "m",
			},
		},
	})

	if _, ok := s.photosByID[imageID("album-a", 0)]; ok {
		t.Fatalf("expected old album-a photo 0 to be removed")
	}
	if _, ok := s.photosByID[imageID("album-a", 1)]; ok {
		t.Fatalf("expected old album-a photo 1 to be removed")
	}
	if _, ok := s.failedByID[imageID("album-a", 1)]; ok {
		t.Fatalf("expected stale failed marker to be removed")
	}
	if _, ok := s.photosByID[imageID("album-a", 2)]; !ok {
		t.Fatalf("expected new album-a photo to be present")
	}
	if _, ok := s.photosByID[imageID("album-b", 0)]; !ok {
		t.Fatalf("expected album-b records to remain")
	}
	if _, ok := s.photoIDsByAlbum["album-a"][imageID("album-a", 2)]; !ok {
		t.Fatalf("expected reverse index to include new album-a photo")
	}
	if _, ok := s.photoIDsByAlbum["album-a"][imageID("album-a", 0)]; ok {
		t.Fatalf("expected reverse index to drop old album-a photo")
	}
}

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
	if _, ok := s.albumMissingPos[albumID]; ok {
		t.Fatalf("expected claimed album to be removed from active missing index")
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
	if _, ok := s.albumMissingPos["album-a"]; !ok {
		t.Fatalf("expected album to be present in active missing index")
	}
}

func TestRecommendExcludesSameAlbumAndReturnsPartial(t *testing.T) {
	s := &Service{
		photosByID: map[string]PhotoRecord{
			imageID("album-a", 0): {ImageID: imageID("album-a", 0), AlbumID: "album-a", PhotoIndex: 0},
			imageID("album-a", 1): {ImageID: imageID("album-a", 1), AlbumID: "album-a", PhotoIndex: 1},
			imageID("album-b", 0): {ImageID: imageID("album-b", 0), AlbumID: "album-b", PhotoIndex: 0},
			imageID("album-c", 0): {ImageID: imageID("album-c", 0), AlbumID: "album-c", PhotoIndex: 0},
		},
		embeddingsByID: map[string]EmbeddingRecord{
			imageID("album-a", 0): {ImageID: imageID("album-a", 0), Vector: []float32{1, 0}},
			imageID("album-a", 1): {ImageID: imageID("album-a", 1), Vector: []float32{1, 0}},
			imageID("album-b", 0): {ImageID: imageID("album-b", 0), Vector: []float32{0.95, 0.05}},
			imageID("album-c", 0): {ImageID: imageID("album-c", 0), Vector: []float32{0.7, 0.3}},
		},
		failedByID: make(map[string]string),
	}

	resp, err := s.Recommend(context.Background(), "album-a", 0, 3)
	if err != nil {
		t.Fatalf("recommend failed: %v", err)
	}
	if resp.Status != "partial" {
		t.Fatalf("expected partial status, got %q", resp.Status)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("expected 2 cross-album items, got %d", len(resp.Items))
	}
	for _, item := range resp.Items {
		if item.AlbumID == "album-a" {
			t.Fatalf("same-album recommendation leaked: %+v", item)
		}
	}
	if resp.Items[0].AlbumID != "album-b" || resp.Items[1].AlbumID != "album-c" {
		t.Fatalf("unexpected recommendation order: %+v", resp.Items)
	}
}

func TestRecommendReturnsReadyWhenOnlySameAlbumCandidates(t *testing.T) {
	s := &Service{
		photosByID: map[string]PhotoRecord{
			imageID("album-a", 0): {ImageID: imageID("album-a", 0), AlbumID: "album-a", PhotoIndex: 0},
			imageID("album-a", 1): {ImageID: imageID("album-a", 1), AlbumID: "album-a", PhotoIndex: 1},
		},
		embeddingsByID: map[string]EmbeddingRecord{
			imageID("album-a", 0): {ImageID: imageID("album-a", 0), Vector: []float32{1, 0}},
			imageID("album-a", 1): {ImageID: imageID("album-a", 1), Vector: []float32{0.9, 0.1}},
		},
		failedByID: make(map[string]string),
	}

	resp, err := s.Recommend(context.Background(), "album-a", 0, 12)
	if err != nil {
		t.Fatalf("recommend failed: %v", err)
	}
	if resp.Status != "ready" {
		t.Fatalf("expected ready status for filtered-empty result, got %q", resp.Status)
	}
	if len(resp.Items) != 0 {
		t.Fatalf("expected no recommendations, got %d", len(resp.Items))
	}
}

func TestRecommendPendingWhenQueryEmbeddingMissing(t *testing.T) {
	s := &Service{
		photosByID: map[string]PhotoRecord{
			imageID("album-a", 0): {ImageID: imageID("album-a", 0), AlbumID: "album-a", PhotoIndex: 0},
			imageID("album-b", 0): {ImageID: imageID("album-b", 0), AlbumID: "album-b", PhotoIndex: 0},
		},
		embeddingsByID: map[string]EmbeddingRecord{
			imageID("album-b", 0): {ImageID: imageID("album-b", 0), Vector: []float32{1, 0}},
		},
		failedByID: make(map[string]string),
	}

	resp, err := s.Recommend(context.Background(), "album-a", 0, 12)
	if err != nil {
		t.Fatalf("recommend failed: %v", err)
	}
	if resp.Status != "pending" {
		t.Fatalf("expected pending status, got %q", resp.Status)
	}
}

func TestRecommendFailedWhenQueryEmbeddingFailed(t *testing.T) {
	s := &Service{
		photosByID: map[string]PhotoRecord{
			imageID("album-a", 0): {ImageID: imageID("album-a", 0), AlbumID: "album-a", PhotoIndex: 0},
		},
		embeddingsByID: make(map[string]EmbeddingRecord),
		failedByID: map[string]string{
			imageID("album-a", 0): "embed failed",
		},
	}

	resp, err := s.Recommend(context.Background(), "album-a", 0, 12)
	if err != nil {
		t.Fatalf("recommend failed: %v", err)
	}
	if resp.Status != "failed" {
		t.Fatalf("expected failed status, got %q", resp.Status)
	}
}
