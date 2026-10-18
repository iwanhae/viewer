package admin

import (
	"context"
	"fmt"
	"time"

	"viewer/internal/catalog"
)

// OverviewUnits makes the units and timestamp format of the overview explicit.
// Pipeline breakdowns are unique-blob counts. Library photo and album totals
// are inventory counts, not pipeline breakdowns.
type OverviewUnits struct {
	Counts     string `json:"counts"`
	MeasuredAt string `json:"measuredAt"`
}

type OverviewLibraryCounts struct {
	Albums int64 `json:"albums"`
	Photos int64 `json:"photos"`
	Blobs  int64 `json:"blobs"`
}

// EncodingCategories classify each blob by its raw encoder status.
type EncodingCategories struct {
	Queued     catalog.BlobCounts `json:"queued"`
	Processing catalog.BlobCounts `json:"processing"`
	Validating catalog.BlobCounts `json:"validating"`
	Committing catalog.BlobCounts `json:"committing"`
	Completed  catalog.BlobCounts `json:"completed"`
	Skipped    catalog.BlobCounts `json:"skipped"`
	Failed     catalog.BlobCounts `json:"failed"`
	Unknown    catalog.BlobCounts `json:"unknown"`
}

// EncodingSkippedReasons distinguishes only what stored content_type can
// reliably establish. It does not infer why a non-WebP blob was skipped.
type EncodingSkippedReasons struct {
	WebP    catalog.BlobCounts `json:"webp"`
	NonWebP catalog.BlobCounts `json:"nonWebp"`
}

type EncodingOverview struct {
	Categories     EncodingCategories            `json:"categories"`
	SkippedReasons EncodingSkippedReasons        `json:"skippedReasons"`
	RawStates      map[string]catalog.BlobCounts `json:"rawStates"`
}

// EmbeddingCategories classify blobs independently of their encoding status.
type EmbeddingCategories struct {
	Queued     catalog.BlobCounts `json:"queued"`
	Processing catalog.BlobCounts `json:"processing"`
	Completed  catalog.BlobCounts `json:"completed"`
	Failed     catalog.BlobCounts `json:"failed"`
	Unknown    catalog.BlobCounts `json:"unknown"`
}

type EmbeddingOverview struct {
	Categories EmbeddingCategories           `json:"categories"`
	RawStates  map[string]catalog.BlobCounts `json:"rawStates"`
}

// Overview is a catalog-only view of the ingest pipeline. measuredAt is the
// UTC RFC3339Nano time recorded after the short catalog snapshot completes.
// No external vector-store or model/recommendation reads are included.
type Overview struct {
	MeasuredAt string                `json:"measuredAt"`
	Units      OverviewUnits         `json:"units"`
	Library    OverviewLibraryCounts `json:"library"`
	Encoding   EncodingOverview      `json:"encoding"`
	Embedding  EmbeddingOverview     `json:"embedding"`
}

// Overview returns one consistent catalog snapshot and intentionally does not
// consult Qdrant or the recommendation service, so their outages cannot affect
// this endpoint.
func (s *Service) Overview(ctx context.Context) (Overview, error) {
	if s == nil || s.cat == nil {
		return Overview{}, fmt.Errorf("catalog is not available")
	}

	snapshot, err := s.cat.AdminOverview(ctx)
	if err != nil {
		return Overview{}, err
	}
	measuredAt := time.Now().UTC().Format(time.RFC3339Nano)
	return Overview{
		MeasuredAt: measuredAt,
		Units: OverviewUnits{
			Counts:     "pipeline categories, skipped reasons, and raw states count each unique blob once; library albums and photos are inventory totals and are not split by pipeline status",
			MeasuredAt: "RFC3339 UTC",
		},
		Library: OverviewLibraryCounts{Albums: snapshot.Albums, Photos: snapshot.Photos, Blobs: snapshot.Blobs},
		Encoding: EncodingOverview{
			Categories: EncodingCategories{
				Queued:     snapshot.EncodingCategories["queued"],
				Processing: snapshot.EncodingCategories["processing"],
				Validating: snapshot.EncodingCategories["validating"],
				Committing: snapshot.EncodingCategories["committing"],
				Completed:  snapshot.EncodingCategories["completed"],
				Skipped:    snapshot.EncodingCategories["skipped"],
				Failed:     snapshot.EncodingCategories["failed"],
				Unknown:    snapshot.EncodingCategories["unknown"],
			},
			SkippedReasons: EncodingSkippedReasons{
				WebP:    snapshot.EncodingSkippedReasons["webp"],
				NonWebP: snapshot.EncodingSkippedReasons["nonWebp"],
			},
			RawStates: snapshot.EncodingRawStates,
		},
		Embedding: EmbeddingOverview{
			Categories: EmbeddingCategories{
				Queued:     snapshot.EmbeddingCategories["queued"],
				Processing: snapshot.EmbeddingCategories["processing"],
				Completed:  snapshot.EmbeddingCategories["completed"],
				Failed:     snapshot.EmbeddingCategories["failed"],
				Unknown:    snapshot.EmbeddingCategories["unknown"],
			},
			RawStates: snapshot.EmbeddingRawStates,
		},
	}, nil
}
