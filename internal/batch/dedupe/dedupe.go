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
	PlanVersion = 1
	HashPolicy  = "etag_or_sha256"
)

type Store interface {
	ListAlbumSources(ctx context.Context) ([]storage.AlbumSourceObject, error)
	ListObjectsByPrefix(ctx context.Context, prefix string) ([]string, error)
	DeleteObject(ctx context.Context, key string) error
	GetObject(ctx context.Context, key string) (io.ReadCloser, string, error)
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
	ETag                 string `json:"etag,omitempty"`
	LastModifiedUnixNano int64  `json:"lastModifiedUnixNano"`
}

type DuplicateGroup struct {
	ContentHash    string        `json:"contentHash"`
	KeepAlbumID    string        `json:"keepAlbumId"`
	DeleteAlbumIDs []string      `json:"deleteAlbumIds"`
	Members        []GroupMember `json:"members"`
}

type SourceSnapshot struct {
	AlbumID              string `json:"albumId"`
	Key                  string `json:"key"`
	Size                 int64  `json:"size"`
	ETag                 string `json:"etag,omitempty"`
	LastModifiedUnixNano int64  `json:"lastModifiedUnixNano"`
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

	type sourceCandidate struct {
		source      storage.AlbumSourceObject
		contentHash string
	}

	groupsByHash := make(map[string][]sourceCandidate)
	for _, source := range sources {
		contentHash, err := p.resolveContentHash(ctx, source)
		if err != nil {
			return Plan{}, PlanSummary{}, fmt.Errorf("hash source %s: %w", source.Key, err)
		}
		groupsByHash[contentHash] = append(groupsByHash[contentHash], sourceCandidate{
			source:      source,
			contentHash: contentHash,
		})
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
		candidates := groupsByHash[hash]
		if len(candidates) < 2 {
			continue
		}

		sort.Slice(candidates, func(i, j int) bool {
			left := candidates[i].source
			right := candidates[j].source
			if !left.LastModified.Equal(right.LastModified) {
				return left.LastModified.After(right.LastModified)
			}
			if left.AlbumID != right.AlbumID {
				return left.AlbumID < right.AlbumID
			}
			return left.Key < right.Key
		})

		members := make([]GroupMember, 0, len(candidates))
		deleteIDs := make([]string, 0, len(candidates)-1)
		for idx, candidate := range candidates {
			source := candidate.source
			members = append(members, GroupMember{
				AlbumID:              source.AlbumID,
				Key:                  source.Key,
				Size:                 source.Size,
				ETag:                 NormalizeETag(source.ETag),
				LastModifiedUnixNano: source.LastModified.UTC().UnixNano(),
			})

			snapshotByKey[source.Key] = SourceSnapshot{
				AlbumID:              source.AlbumID,
				Key:                  source.Key,
				Size:                 source.Size,
				ETag:                 NormalizeETag(source.ETag),
				LastModifiedUnixNano: source.LastModified.UTC().UnixNano(),
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
			KeepAlbumID:    candidates[0].source.AlbumID,
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

func (p *Planner) resolveContentHash(ctx context.Context, source storage.AlbumSourceObject) (string, error) {
	if hash, ok := SimpleETagHash(source.ETag); ok {
		return "etag:" + hash, nil
	}

	body, _, err := p.store.GetObject(ctx, source.Key)
	if err != nil {
		return "", err
	}
	defer body.Close()

	digest := sha256.New()
	if _, err := io.Copy(digest, body); err != nil {
		return "", fmt.Errorf("read object for sha256: %w", err)
	}
	return "sha256:" + hex.EncodeToString(digest.Sum(nil)), nil
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
		if NormalizeETag(source.ETag) != NormalizeETag(snapshot.ETag) {
			return fmt.Errorf("snapshot mismatch: etag changed for %s", snapshot.Key)
		}
		if source.LastModified.UTC().UnixNano() != snapshot.LastModifiedUnixNano {
			return fmt.Errorf("snapshot mismatch: lastModified changed for %s", snapshot.Key)
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

func NormalizeETag(etag string) string {
	etag = strings.TrimSpace(strings.ToLower(etag))
	etag = strings.Trim(etag, "\"")
	return etag
}

func SimpleETagHash(etag string) (string, bool) {
	etag = NormalizeETag(etag)
	if etag == "" || strings.Contains(etag, "-") || len(etag) != 32 {
		return "", false
	}
	for _, ch := range etag {
		if !isLowerHex(ch) {
			return "", false
		}
	}
	return etag, true
}

func isLowerHex(ch rune) bool {
	return (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f')
}

func sortedSetKeys[T any](set map[string]T) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
