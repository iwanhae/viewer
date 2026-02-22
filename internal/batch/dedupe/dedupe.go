package dedupe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"viewer/internal/storage"
)

const (
	PlanVersion = 2
	HashPolicy  = "size_sampled_then_sha256_v1"

	sampleWindowBytes int64 = 64 * 1024
)

type Store interface {
	ListAlbumSources(ctx context.Context) ([]storage.AlbumSourceObject, error)
	ListObjectsByPrefix(ctx context.Context, prefix string) ([]string, error)
	DeleteObject(ctx context.Context, key string) error
	GetObject(ctx context.Context, key string) (io.ReadCloser, string, error)
	GetObjectRange(ctx context.Context, key string, start int64, end int64) (io.ReadCloser, string, error)
}

type Planner struct {
	store  Store
	bucket string
}

type PlanOptions struct {
	Limit int
}

type PlanSummary struct {
	ScannedSources  int `json:"scannedSources"`
	DuplicateGroups int `json:"duplicateGroups"`
	KeptAlbums      int `json:"keptAlbums"`
	DeletedAlbums   int `json:"deletedAlbums"`
	DeleteKeys      int `json:"deleteKeys"`
}

type GroupMember struct {
	AlbumID              string `json:"albumId"`
	Key                  string `json:"key"`
	Size                 int64  `json:"size"`
	LastModifiedUnixNano int64  `json:"lastModifiedUnixNano"`
}

type DuplicateGroup struct {
	ContentHash    string        `json:"contentHash"`
	KeepAlbumID    string        `json:"keepAlbumId"`
	DeleteAlbumIDs []string      `json:"deleteAlbumIds"`
	Members        []GroupMember `json:"members"`
}

type SourceSnapshot struct {
	AlbumID string `json:"albumId"`
	Key     string `json:"key"`
	Size    int64  `json:"size"`
}

type Plan struct {
	Version         int              `json:"version"`
	GeneratedAt     string           `json:"generatedAt"`
	Bucket          string           `json:"bucket"`
	HashPolicy      string           `json:"hashPolicy"`
	Groups          []DuplicateGroup `json:"groups"`
	KeepAlbums      []string         `json:"keepAlbums"`
	DeleteAlbums    []string         `json:"deleteAlbums"`
	DeleteKeys      []string         `json:"deleteKeys"`
	SourceSnapshot  []SourceSnapshot `json:"sourceSnapshot"`
	PlanFingerprint string           `json:"planFingerprint"`
}

type DeleteFailure struct {
	Key   string `json:"key"`
	Error string `json:"error"`
}

type ApplyResult struct {
	Attempted int             `json:"attempted"`
	Deleted   int             `json:"deleted"`
	Failed    []DeleteFailure `json:"failed"`
}

func NewPlanner(store Store, bucket string) *Planner {
	return &Planner{
		store:  store,
		bucket: strings.TrimSpace(bucket),
	}
}

func (p *Planner) BuildPlan(ctx context.Context, opts PlanOptions) (Plan, PlanSummary, error) {
	if p == nil || p.store == nil {
		return Plan{}, PlanSummary{}, fmt.Errorf("planner store is required")
	}
	if p.bucket == "" {
		return Plan{}, PlanSummary{}, fmt.Errorf("bucket is required")
	}

	sources, err := p.store.ListAlbumSources(ctx)
	if err != nil {
		return Plan{}, PlanSummary{}, err
	}
	sort.Slice(sources, func(i, j int) bool {
		return sources[i].Key < sources[j].Key
	})
	if opts.Limit > 0 && len(sources) > opts.Limit {
		sources = sources[:opts.Limit]
	}

	groupsBySize := make(map[int64][]storage.AlbumSourceObject)
	for _, source := range sources {
		groupsBySize[source.Size] = append(groupsBySize[source.Size], source)
	}

	sizes := make([]int64, 0, len(groupsBySize))
	for size := range groupsBySize {
		sizes = append(sizes, size)
	}
	sort.Slice(sizes, func(i, j int) bool {
		return sizes[i] < sizes[j]
	})

	groupsByHash := make(map[string][]storage.AlbumSourceObject)
	for _, size := range sizes {
		candidates := groupsBySize[size]
		if len(candidates) < 2 {
			continue
		}

		sampleGroups := make(map[string][]storage.AlbumSourceObject)
		sampleFailed := false
		for _, source := range candidates {
			sampleHash, err := p.resolveSampleHash(ctx, source)
			if err != nil {
				sampleFailed = true
				break
			}
			sampleGroups[sampleHash] = append(sampleGroups[sampleHash], source)
		}

		if sampleFailed {
			for _, source := range candidates {
				fullHash, err := p.resolveFullSHA256(ctx, source)
				if err != nil {
					return Plan{}, PlanSummary{}, fmt.Errorf("hash source %s: %w", source.Key, err)
				}
				contentHash := "sha256:" + fullHash
				groupsByHash[contentHash] = append(groupsByHash[contentHash], source)
			}
			continue
		}

		sampleHashes := make([]string, 0, len(sampleGroups))
		for sampleHash := range sampleGroups {
			sampleHashes = append(sampleHashes, sampleHash)
		}
		sort.Strings(sampleHashes)

		for _, sampleHash := range sampleHashes {
			sampleCandidates := sampleGroups[sampleHash]
			if len(sampleCandidates) < 2 {
				continue
			}
			for _, source := range sampleCandidates {
				fullHash, err := p.resolveFullSHA256(ctx, source)
				if err != nil {
					return Plan{}, PlanSummary{}, fmt.Errorf("hash source %s: %w", source.Key, err)
				}
				contentHash := "sha256:" + fullHash
				groupsByHash[contentHash] = append(groupsByHash[contentHash], source)
			}
		}
	}

	hashes := make([]string, 0, len(groupsByHash))
	for hash := range groupsByHash {
		hashes = append(hashes, hash)
	}
	sort.Strings(hashes)

	duplicateGroups := make([]DuplicateGroup, 0)
	keepAlbumSet := make(map[string]struct{})
	deleteAlbumSet := make(map[string]struct{})
	snapshotByKey := make(map[string]SourceSnapshot)

	for _, hash := range hashes {
		sourcesWithHash := groupsByHash[hash]
		if len(sourcesWithHash) < 2 {
			continue
		}

		sort.Slice(sourcesWithHash, func(i, j int) bool {
			left := sourcesWithHash[i]
			right := sourcesWithHash[j]
			if !left.LastModified.Equal(right.LastModified) {
				return left.LastModified.After(right.LastModified)
			}
			if left.AlbumID != right.AlbumID {
				return left.AlbumID < right.AlbumID
			}
			return left.Key < right.Key
		})

		members := make([]GroupMember, 0, len(sourcesWithHash))
		deleteIDs := make([]string, 0, len(sourcesWithHash)-1)
		for idx, source := range sourcesWithHash {
			members = append(members, GroupMember{
				AlbumID:              source.AlbumID,
				Key:                  source.Key,
				Size:                 source.Size,
				LastModifiedUnixNano: source.LastModified.UTC().UnixNano(),
			})

			snapshotByKey[source.Key] = SourceSnapshot{
				AlbumID: source.AlbumID,
				Key:     source.Key,
				Size:    source.Size,
			}

			if idx == 0 {
				keepAlbumSet[source.AlbumID] = struct{}{}
				continue
			}
			deleteAlbumSet[source.AlbumID] = struct{}{}
			deleteIDs = append(deleteIDs, source.AlbumID)
		}
		sort.Strings(deleteIDs)

		duplicateGroups = append(duplicateGroups, DuplicateGroup{
			ContentHash:    hash,
			KeepAlbumID:    sourcesWithHash[0].AlbumID,
			DeleteAlbumIDs: deleteIDs,
			Members:        members,
		})
	}

	sort.Slice(duplicateGroups, func(i, j int) bool {
		return duplicateGroups[i].ContentHash < duplicateGroups[j].ContentHash
	})

	deleteAlbums := sortedSetKeys(deleteAlbumSet)
	keepAlbums := sortedSetKeys(keepAlbumSet)

	deleteKeySet := make(map[string]struct{})
	for _, albumID := range deleteAlbums {
		prefix := fmt.Sprintf("albums/%s/", albumID)
		keys, err := p.store.ListObjectsByPrefix(ctx, prefix)
		if err != nil {
			return Plan{}, PlanSummary{}, fmt.Errorf("list delete keys for %s: %w", albumID, err)
		}
		for _, key := range keys {
			deleteKeySet[key] = struct{}{}
		}
	}
	deleteKeys := sortedSetKeys(deleteKeySet)

	snapshot := make([]SourceSnapshot, 0, len(snapshotByKey))
	snapshotKeys := make([]string, 0, len(snapshotByKey))
	for key := range snapshotByKey {
		snapshotKeys = append(snapshotKeys, key)
	}
	sort.Strings(snapshotKeys)
	for _, key := range snapshotKeys {
		snapshot = append(snapshot, snapshotByKey[key])
	}

	plan := Plan{
		Version:        PlanVersion,
		GeneratedAt:    time.Now().UTC().Format(time.RFC3339Nano),
		Bucket:         p.bucket,
		HashPolicy:     HashPolicy,
		Groups:         duplicateGroups,
		KeepAlbums:     keepAlbums,
		DeleteAlbums:   deleteAlbums,
		DeleteKeys:     deleteKeys,
		SourceSnapshot: snapshot,
	}
	if err := plan.Seal(); err != nil {
		return Plan{}, PlanSummary{}, err
	}

	summary := PlanSummary{
		ScannedSources:  len(sources),
		DuplicateGroups: len(duplicateGroups),
		KeptAlbums:      len(keepAlbums),
		DeletedAlbums:   len(deleteAlbums),
		DeleteKeys:      len(deleteKeys),
	}
	return plan, summary, nil
}

type byteRange struct {
	start int64
	end   int64
}

func sampleRanges(size int64) []byteRange {
	window := sampleWindowBytes
	if size <= 0 {
		return nil
	}
	if size < window {
		window = size
	}
	maxStart := size - window
	middleStart := (size / 2) - (window / 2)
	if middleStart < 0 {
		middleStart = 0
	}
	if middleStart > maxStart {
		middleStart = maxStart
	}

	starts := []int64{0, middleStart, maxStart}
	seenStarts := make(map[int64]struct{}, len(starts))
	ranges := make([]byteRange, 0, len(starts))
	for _, start := range starts {
		if start < 0 {
			start = 0
		}
		if start > maxStart {
			start = maxStart
		}
		if _, exists := seenStarts[start]; exists {
			continue
		}
		seenStarts[start] = struct{}{}
		ranges = append(ranges, byteRange{
			start: start,
			end:   start + window - 1,
		})
	}
	sort.Slice(ranges, func(i, j int) bool {
		if ranges[i].start != ranges[j].start {
			return ranges[i].start < ranges[j].start
		}
		return ranges[i].end < ranges[j].end
	})
	return ranges
}

func (p *Planner) resolveSampleHash(ctx context.Context, source storage.AlbumSourceObject) (string, error) {
	ranges := sampleRanges(source.Size)
	if len(ranges) == 0 {
		return "", fmt.Errorf("invalid source size: %d", source.Size)
	}

	digest := sha256.New()
	if _, err := fmt.Fprintf(digest, "size=%d;", source.Size); err != nil {
		return "", fmt.Errorf("write sample hash header: %w", err)
	}

	for _, sample := range ranges {
		body, _, err := p.store.GetObjectRange(ctx, source.Key, sample.start, sample.end)
		if err != nil {
			return "", fmt.Errorf("read sample range %d-%d: %w", sample.start, sample.end, err)
		}
		if _, err := fmt.Fprintf(digest, "range=%d-%d:", sample.start, sample.end); err != nil {
			body.Close()
			return "", fmt.Errorf("write sample range header: %w", err)
		}
		if _, err := io.Copy(digest, body); err != nil {
			body.Close()
			return "", fmt.Errorf("read sample bytes: %w", err)
		}
		if err := body.Close(); err != nil {
			return "", fmt.Errorf("close sample reader: %w", err)
		}
		if _, err := io.WriteString(digest, ";"); err != nil {
			return "", fmt.Errorf("write sample delimiter: %w", err)
		}
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func (p *Planner) resolveFullSHA256(ctx context.Context, source storage.AlbumSourceObject) (string, error) {
	body, _, err := p.store.GetObject(ctx, source.Key)
	if err != nil {
		return "", err
	}
	defer body.Close()

	digest := sha256.New()
	if _, err := io.Copy(digest, body); err != nil {
		return "", fmt.Errorf("read object for sha256: %w", err)
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func ApplyPlan(ctx context.Context, store Store, plan Plan) (ApplyResult, error) {
	if store == nil {
		return ApplyResult{}, fmt.Errorf("store is required")
	}
	if err := ValidatePlan(plan); err != nil {
		return ApplyResult{}, err
	}
	if err := validateSourceSnapshot(ctx, store, plan.SourceSnapshot); err != nil {
		return ApplyResult{}, err
	}

	result := ApplyResult{
		Attempted: len(plan.DeleteKeys),
		Deleted:   0,
		Failed:    make([]DeleteFailure, 0),
	}
	for _, key := range plan.DeleteKeys {
		if err := store.DeleteObject(ctx, key); err != nil {
			result.Failed = append(result.Failed, DeleteFailure{
				Key:   key,
				Error: err.Error(),
			})
			continue
		}
		result.Deleted++
	}

	if len(result.Failed) > 0 {
		return result, fmt.Errorf("delete failed for %d keys", len(result.Failed))
	}
	return result, nil
}

func ValidatePlan(plan Plan) error {
	if plan.Version != PlanVersion {
		return fmt.Errorf("unsupported plan version: %d", plan.Version)
	}
	if strings.TrimSpace(plan.Bucket) == "" {
		return fmt.Errorf("plan bucket is required")
	}
	if strings.TrimSpace(plan.HashPolicy) != HashPolicy {
		return fmt.Errorf("unsupported hash policy: %s", plan.HashPolicy)
	}
	if err := plan.ValidateFingerprint(); err != nil {
		return err
	}
	return nil
}

func validateSourceSnapshot(ctx context.Context, store Store, snapshots []SourceSnapshot) error {
	if len(snapshots) == 0 {
		return nil
	}
	currentSources, err := store.ListAlbumSources(ctx)
	if err != nil {
		return fmt.Errorf("list current album sources: %w", err)
	}

	currentByKey := make(map[string]storage.AlbumSourceObject, len(currentSources))
	for _, source := range currentSources {
		currentByKey[source.Key] = source
	}

	for _, snapshot := range snapshots {
		source, ok := currentByKey[snapshot.Key]
		if !ok {
			return fmt.Errorf("snapshot mismatch: source missing %s", snapshot.Key)
		}
		if source.Size != snapshot.Size {
			return fmt.Errorf("snapshot mismatch: size changed for %s (have=%d want=%d)", snapshot.Key, source.Size, snapshot.Size)
		}
	}
	return nil
}

func WritePlanFile(path string, plan Plan) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("plan output path is required")
	}
	if plan.PlanFingerprint == "" {
		if err := plan.Seal(); err != nil {
			return err
		}
	} else if err := plan.ValidateFingerprint(); err != nil {
		return err
	}

	body, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal plan: %w", err)
	}
	body = append(body, '\n')

	if err := os.WriteFile(path, body, 0644); err != nil {
		return fmt.Errorf("write plan %s: %w", path, err)
	}
	return nil
}

func ReadPlanFile(path string) (Plan, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return Plan{}, fmt.Errorf("plan path is required")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return Plan{}, fmt.Errorf("read plan %s: %w", path, err)
	}
	var plan Plan
	if err := json.Unmarshal(body, &plan); err != nil {
		return Plan{}, fmt.Errorf("decode plan %s: %w", path, err)
	}
	return plan, nil
}

func ComputePlanFingerprint(plan Plan) (string, error) {
	payload := plan
	payload.PlanFingerprint = ""

	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal fingerprint payload: %w", err)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

func (p *Plan) Seal() error {
	if p == nil {
		return fmt.Errorf("plan is nil")
	}
	fingerprint, err := ComputePlanFingerprint(*p)
	if err != nil {
		return err
	}
	p.PlanFingerprint = fingerprint
	return nil
}

func (p Plan) ValidateFingerprint() error {
	actual := strings.ToLower(strings.TrimSpace(p.PlanFingerprint))
	if actual == "" {
		return fmt.Errorf("plan fingerprint is required")
	}
	expected, err := ComputePlanFingerprint(p)
	if err != nil {
		return err
	}
	if actual != expected {
		return fmt.Errorf("plan fingerprint mismatch")
	}
	return nil
}

func sortedSetKeys[T any](set map[string]T) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
