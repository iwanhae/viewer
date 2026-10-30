package admin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"viewer/internal/catalog"
)

const maxIssueGroupResults = 100

type IssueGroup struct {
	Stage        string `json:"stage"`
	ErrorCode    string `json:"errorCode"`
	FailureCount int64  `json:"failureCount"`
	CountUnit    string `json:"countUnit"`
}

type Issues struct {
	MeasuredAt      string       `json:"measuredAt"`
	Groups          []IssueGroup `json:"groups"`
	TotalGroups     int64        `json:"totalGroups"`
	RemainingGroups int64        `json:"remainingGroups"`
	Truncated       bool         `json:"truncated"`
	CountScope      string       `json:"countScope"`
}

func (s *Service) Issues(ctx context.Context) (Issues, error) {
	if s == nil || s.cat == nil {
		return Issues{}, fmt.Errorf("catalog is not available")
	}
	snapshot, err := s.cat.AdminIssues(ctx)
	if err != nil {
		return Issues{}, fmt.Errorf("admin issue snapshot: %w", err)
	}
	groups := make([]IssueGroup, len(snapshot.Groups))
	for i, group := range snapshot.Groups {
		groups[i] = IssueGroup{
			Stage: group.Stage, ErrorCode: group.ErrorCode, FailureCount: group.FailureCount, CountUnit: group.CountUnit,
		}
	}
	remaining := snapshot.TotalGroups - int64(len(groups))
	if remaining < 0 {
		remaining = 0
	}
	return Issues{
		MeasuredAt: time.Now().UTC().Format(time.RFC3339Nano), Groups: groups, TotalGroups: snapshot.TotalGroups,
		RemainingGroups: remaining, Truncated: remaining > 0,
		CountScope: "failureCount counts the explicit countUnit: unique blobs for encoding/embedding, albums for archive failures, and failed entries for extraction-entry groups. Groups are independent and may overlap; extraction entries use each album's latest recorded run only.",
	}, nil
}

type ExtractionFailureOptions struct {
	ReasonCode string
	Limit      int
	Cursor     string
}

type ExtractionFailurePage struct {
	ReasonCode string                           `json:"reasonCode"`
	Failures   []catalog.AdminExtractionFailure `json:"failures"`
	NextCursor *string                          `json:"nextCursor"`
}

type extractionFailureCursor struct {
	Version int    `json:"v"`
	Kind    string `json:"kind"`
	Reason  string `json:"reason"`
	Limit   int    `json:"limit"`
	AlbumID string `json:"albumId"`
	Ordinal int    `json:"ordinal"`
}

func (s *Service) LatestExtractionFailures(ctx context.Context, options ExtractionFailureOptions) (ExtractionFailurePage, error) {
	if s == nil || s.cat == nil {
		return ExtractionFailurePage{}, fmt.Errorf("catalog is not available")
	}
	if options.Limit < 1 || options.Limit > MaxBrowseLimit {
		return ExtractionFailurePage{}, fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalidBrowseRequest, MaxBrowseLimit)
	}
	if !validSafeCode(options.ReasonCode) {
		return ExtractionFailurePage{}, fmt.Errorf("%w: reasonCode must be a lowercase error code", ErrInvalidBrowseRequest)
	}
	afterAlbum, afterOrdinal, err := decodeExtractionFailureCursor(options.Cursor, options.ReasonCode, options.Limit)
	if err != nil {
		return ExtractionFailurePage{}, err
	}
	rows, err := s.cat.AdminLatestExtractionFailures(ctx, options.ReasonCode, afterAlbum, afterOrdinal, options.Limit+1)
	if err != nil {
		return ExtractionFailurePage{}, err
	}
	page := ExtractionFailurePage{ReasonCode: options.ReasonCode, Failures: rows}
	if len(rows) > options.Limit {
		page.Failures = rows[:options.Limit]
		last := page.Failures[len(page.Failures)-1]
		page.NextCursor = encodeExtractionFailureCursor(extractionFailureCursor{
			Version: 1, Kind: "extractionFailures", Reason: options.ReasonCode, Limit: options.Limit,
			AlbumID: last.AlbumID, Ordinal: last.Ordinal,
		})
	}
	return page, nil
}

func encodeExtractionFailureCursor(cursor extractionFailureCursor) *string {
	data, _ := json.Marshal(cursor)
	token := base64.RawURLEncoding.EncodeToString(data)
	return &token
}

func decodeExtractionFailureCursor(token, reason string, limit int) (string, int, error) {
	if token == "" {
		return "", -1, nil
	}
	if len(token) > maxBrowseCursor {
		return "", 0, invalidCursor()
	}
	data, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", 0, invalidCursor()
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var cursor extractionFailureCursor
	if decoder.Decode(&cursor) != nil || cursor.Version != 1 || cursor.Kind != "extractionFailures" ||
		cursor.Reason != reason || cursor.Limit != limit || cursor.AlbumID == "" || cursor.Ordinal < 0 {
		return "", 0, invalidCursor()
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return "", 0, invalidCursor()
	}
	return cursor.AlbumID, cursor.Ordinal, nil
}
