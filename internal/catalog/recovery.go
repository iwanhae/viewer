package catalog

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	RecoveryRetryFailedEncoding  = "retry_failed_encoding"
	RecoveryRetryFailedEmbedding = "retry_failed_embedding"
	RecoveryReleaseEncoding      = "release_expired_encoding"
	RecoveryReleaseEmbedding     = "release_expired_embedding"
	RecoveryTargetLimit          = 10000
	RecoveryPreviewTTL           = 10 * time.Minute
	RecoveryPageLimit            = 100
	recoveryTargetMaxAttempts    = 5
)

func recoveryTimestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000000000Z")
}

var (
	ErrInvalidRecoverySelection = errors.New("invalid recovery selection")
	ErrRecoveryConflict         = errors.New("recovery preview conflict")
	ErrRecoveryNotFound         = errors.New("recovery resource not found")
)

// RecoverySelection is a frozen input to one narrow action. Exactly one scope
// is populated. Filters use the same exact blob filters as GET /admin/api/blobs.
type RecoverySelection struct {
	Hashes []string          `json:"hashes,omitempty"`
	Photos []RecoveryPhoto   `json:"photos,omitempty"`
	Albums []string          `json:"albumIds,omitempty"`
	Filter *AdminBlobFilters `json:"filter,omitempty"`
}

type RecoveryPhoto struct {
	AlbumID string `json:"albumId"`
	Index   int    `json:"idx"`
}

type RecoveryPreview struct {
	ID                string           `json:"previewId"`
	Actor             string           `json:"actor"`
	ActorNote         string           `json:"actorNote"`
	Action            string           `json:"action"`
	CreatedAt         string           `json:"createdAt"`
	ExpiresAt         string           `json:"expiresAt"`
	TargetCount       int64            `json:"targetCount"`
	EligibleCount     int64            `json:"eligibleCount"`
	SkippedCount      int64            `json:"skippedCount"`
	SkippedReasons    map[string]int64 `json:"skippedReasons"`
	SelectedPhotos    int64            `json:"selectedPhotos"`
	AffectedPhotos    int64            `json:"affectedPhotos"`
	AffectedAlbums    int64            `json:"affectedAlbums"`
	SharedBlobWarning bool             `json:"sharedBlobWarning"`
}

type RecoveryOperation struct {
	ID                string `json:"id"`
	PreviewID         string `json:"previewId"`
	Actor             string `json:"actor"`
	ActorNote         string `json:"actorNote"`
	Action            string `json:"action"`
	Status            string `json:"status"`
	CreatedAt         string `json:"createdAt"`
	UpdatedAt         string `json:"updatedAt"`
	Total             int64  `json:"total"`
	Eligible          int64  `json:"eligible"`
	Succeeded         int64  `json:"succeeded"`
	Conflicts         int64  `json:"conflicts"`
	Skipped           int64  `json:"skipped"`
	Failed            int64  `json:"failed"`
	SelectedPhotos    int64  `json:"selectedPhotos"`
	AffectedPhotos    int64  `json:"affectedPhotos"`
	AffectedAlbums    int64  `json:"affectedAlbums"`
	SharedBlobWarning bool   `json:"sharedBlobWarning"`
}

type RecoveryResult struct {
	Ordinal   int64  `json:"ordinal"`
	Hash      string `json:"hash"`
	Status    string `json:"status"`
	Reason    string `json:"reason,omitempty"`
	UpdatedAt string `json:"updatedAt"`
}

type RecoveryOperationPage struct {
	Operations []RecoveryOperation `json:"operations"`
	NextCursor *string             `json:"nextCursor"`
}

type RecoveryResultPage struct {
	Operation  RecoveryOperation `json:"operation"`
	Results    []RecoveryResult  `json:"results"`
	NextCursor *string           `json:"nextCursor"`
}

type recoveryTargetState struct {
	EncodingStatus      string
	EmbeddingStatus     string
	ContentType         string
	EncodingLease       int64
	EmbeddingLease      int64
	EmbeddingFinalizing bool
	TokenHash           string
	TokenValue          string
	ObsStatus           string
	ObsPhase            string
	StartedAt           string
	FinishedAt          string
	StatusChangedAt     string
	Attempts            int64
	ErrorCode           string
	Retryability        string
}

func validRecoveryAction(action string) bool {
	switch action {
	case RecoveryRetryFailedEncoding, RecoveryRetryFailedEmbedding, RecoveryReleaseEncoding, RecoveryReleaseEmbedding:
		return true
	default:
		return false
	}
}

func recoveryStage(action string) string {
	if action == RecoveryRetryFailedEncoding || action == RecoveryReleaseEncoding {
		return "encoding"
	}
	return "embedding"
}

// CreateRecoveryPreview reads the exact target set and stores it in one SQLite
// writer transaction. No pipeline rows are modified by a preview.
func (s *Store) CreateRecoveryPreview(ctx context.Context, actor, action string, selection RecoverySelection, now time.Time) (RecoveryPreview, error) {
	if !validRecoveryAction(action) {
		return RecoveryPreview{}, fmt.Errorf("%w: unsupported action", ErrInvalidRecoverySelection)
	}
	if err := validateRecoverySelection(selection); err != nil {
		return RecoveryPreview{}, err
	}
	if now.IsZero() {
		now = time.Now()
	}
	actor = recoveryActor(actor)
	encodedSelection, err := json.Marshal(selection)
	if err != nil {
		return RecoveryPreview{}, fmt.Errorf("encode recovery selection: %w", err)
	}
	previewID, err := newRecoveryID()
	if err != nil {
		return RecoveryPreview{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RecoveryPreview{}, err
	}
	defer tx.Rollback()
	if err := lockCatalogWriter(ctx, tx); err != nil {
		return RecoveryPreview{}, fmt.Errorf("lock recovery preview snapshot: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM admin_recovery_previews WHERE id IN
		(SELECT id FROM admin_recovery_previews WHERE consumed_by IS NULL AND expires_at < ? ORDER BY expires_at LIMIT 500)`, observationTime(now)); err != nil {
		return RecoveryPreview{}, fmt.Errorf("clean up expired recovery previews: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO admin_recovery_previews
		(id, actor, action, selection_json, created_at, expires_at, target_count, eligible_count, skipped_count,
		 selected_photos, affected_photos, affected_albums)
		VALUES (?, ?, ?, ?, ?, ?, 0, 0, 0, 0, 0, 0)`, previewID, actor, action, string(encodedSelection),
		observationTime(now), observationTime(now.Add(RecoveryPreviewTTL))); err != nil {
		return RecoveryPreview{}, fmt.Errorf("store recovery preview audit: %w", err)
	}
	hashes, err := recoverySelectionHashes(ctx, tx, selection)
	if err != nil {
		return RecoveryPreview{}, err
	}
	if len(hashes) > RecoveryTargetLimit {
		return RecoveryPreview{}, fmt.Errorf("%w: selection exceeds %d distinct blobs", ErrInvalidRecoverySelection, RecoveryTargetLimit)
	}
	preview := RecoveryPreview{
		ID: previewID, Actor: actor, ActorNote: "Basic-auth username label; token holder is not individually verified",
		Action: action, CreatedAt: observationTime(now), ExpiresAt: observationTime(now.Add(RecoveryPreviewTTL)),
		TargetCount: int64(len(hashes)), SkippedReasons: make(map[string]int64),
	}
	for ordinal, hash := range hashes {
		state, err := readRecoveryTargetState(ctx, tx, hash, recoveryStage(action))
		if err != nil {
			return RecoveryPreview{}, err
		}
		eligible, reason := recoveryEligibility(action, state, now)
		generation := recoveryGeneration(action, state)
		if eligible {
			preview.EligibleCount++
		} else {
			preview.SkippedCount++
			preview.SkippedReasons[reason]++
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO admin_recovery_targets
			(preview_id, ordinal, blob_hash, eligible, skip_reason, generation, selected_photos, affected_photos, affected_albums)
			VALUES (?, ?, ?, ?, ?, ?, 0, 0, 0)`, preview.ID, ordinal, hash, eligible, reason, generation); err != nil {
			return RecoveryPreview{}, fmt.Errorf("store recovery target snapshot: %w", err)
		}
	}
	if err := recoveryRelationCounts(ctx, tx, preview.ID, selection, &preview); err != nil {
		return RecoveryPreview{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE admin_recovery_previews SET target_count=?, eligible_count=?, skipped_count=?,
		selected_photos=?, affected_photos=?, affected_albums=? WHERE id=?`, preview.TargetCount, preview.EligibleCount,
		preview.SkippedCount, preview.SelectedPhotos, preview.AffectedPhotos, preview.AffectedAlbums, preview.ID); err != nil {
		return RecoveryPreview{}, fmt.Errorf("finalize recovery preview audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return RecoveryPreview{}, fmt.Errorf("commit recovery preview: %w", err)
	}
	preview.SharedBlobWarning = preview.AffectedPhotos > preview.SelectedPhotos
	return preview, nil
}

func validateRecoverySelection(selection RecoverySelection) error {
	populated := 0
	if len(selection.Hashes) > 0 {
		populated++
	}
	if len(selection.Photos) > 0 {
		populated++
	}
	if len(selection.Albums) > 0 {
		populated++
	}
	if selection.Filter != nil {
		populated++
	}
	if populated != 1 {
		return fmt.Errorf("%w: provide exactly one of hashes, photos, albumIds, or filter", ErrInvalidRecoverySelection)
	}
	count := len(selection.Hashes) + len(selection.Photos) + len(selection.Albums)
	if count > RecoveryTargetLimit {
		return fmt.Errorf("%w: explicit selection exceeds %d items", ErrInvalidRecoverySelection, RecoveryTargetLimit)
	}
	for _, hash := range selection.Hashes {
		if strings.TrimSpace(hash) == "" || len(hash) > 256 {
			return fmt.Errorf("%w: invalid blob hash", ErrInvalidRecoverySelection)
		}
	}
	for _, photo := range selection.Photos {
		if strings.TrimSpace(photo.AlbumID) == "" || photo.Index < 0 {
			return fmt.Errorf("%w: invalid photo identity", ErrInvalidRecoverySelection)
		}
	}
	for _, id := range selection.Albums {
		if strings.TrimSpace(id) == "" || len(id) > 256 {
			return fmt.Errorf("%w: invalid album id", ErrInvalidRecoverySelection)
		}
	}
	return nil
}

func recoverySelectionHashes(ctx context.Context, tx *sql.Tx, selection RecoverySelection) ([]string, error) {
	var query string
	var args []any
	switch {
	case len(selection.Hashes) > 0:
		values := uniqueStrings(selection.Hashes)
		if len(values) > RecoveryTargetLimit {
			return nil, fmt.Errorf("%w: too many hashes", ErrInvalidRecoverySelection)
		}
		query, args = `SELECT hash FROM blobs WHERE hash IN (`+placeholders(len(values))+`) ORDER BY hash`, stringArgs(values)
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		hashes, readErr := scanHashes(rows, RecoveryTargetLimit+1)
		if readErr != nil {
			return nil, readErr
		}
		if len(hashes) != len(values) {
			return nil, fmt.Errorf("%w: one or more hashes do not exist", ErrInvalidRecoverySelection)
		}
		return hashes, nil
	case len(selection.Photos) > 0:
		if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS admin_recovery_selected_photos (album_id TEXT NOT NULL, idx INTEGER NOT NULL, PRIMARY KEY(album_id, idx)) WITHOUT ROWID`); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM admin_recovery_selected_photos`); err != nil {
			return nil, err
		}
		for _, photo := range uniquePhotos(selection.Photos) {
			if _, err := tx.ExecContext(ctx, `INSERT INTO admin_recovery_selected_photos(album_id,idx) VALUES (?,?)`, photo.AlbumID, photo.Index); err != nil {
				return nil, err
			}
		}
		query = `SELECT DISTINCT b.hash FROM admin_recovery_selected_photos s JOIN photos p ON p.album_id=s.album_id AND p.idx=s.idx JOIN blobs b ON b.hash=p.hash ORDER BY b.hash LIMIT ?`
		args = []any{RecoveryTargetLimit + 1}
	case len(selection.Albums) > 0:
		ids := uniqueStrings(selection.Albums)
		if len(ids) > RecoveryTargetLimit {
			return nil, fmt.Errorf("%w: too many albums", ErrInvalidRecoverySelection)
		}
		var found int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM albums WHERE id IN (`+placeholders(len(ids))+`)`, stringArgs(ids)...).Scan(&found); err != nil {
			return nil, err
		}
		if found != len(ids) {
			return nil, fmt.Errorf("%w: one or more albums do not exist", ErrInvalidRecoverySelection)
		}
		query = `SELECT DISTINCT b.hash FROM photos p JOIN blobs b ON b.hash=p.hash WHERE p.album_id IN (` + placeholders(len(ids)) + `) ORDER BY b.hash LIMIT ?`
		args = append(stringArgs(ids), RecoveryTargetLimit+1)
	case selection.Filter != nil:
		where, filterArgs := adminBlobWhere(*selection.Filter, "")
		query = `SELECT b.hash FROM blobs b LEFT JOIN blob_stage_observations eo ON eo.blob_hash=b.hash AND eo.stage='encoding' LEFT JOIN blob_stage_observations bo ON bo.blob_hash=b.hash AND bo.stage='embedding'` + adminWhere(where) + ` ORDER BY b.hash LIMIT ?`
		args = append(filterArgs, RecoveryTargetLimit+1)
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read recovery selection: %w", err)
	}
	hashes, err := scanHashes(rows, RecoveryTargetLimit+1)
	if err != nil {
		return nil, err
	}
	if len(hashes) > RecoveryTargetLimit {
		return nil, fmt.Errorf("%w: selection exceeds %d distinct blobs", ErrInvalidRecoverySelection, RecoveryTargetLimit)
	}
	if len(selection.Photos) > 0 {
		var selected int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM admin_recovery_selected_photos s JOIN photos p ON p.album_id=s.album_id AND p.idx=s.idx JOIN blobs b ON b.hash=p.hash`).Scan(&selected); err != nil {
			return nil, err
		}
		if selected != len(uniquePhotos(selection.Photos)) {
			return nil, fmt.Errorf("%w: one or more photos do not exist or have no blob", ErrInvalidRecoverySelection)
		}
	}
	return hashes, nil
}

func recoveryRelationCounts(ctx context.Context, tx *sql.Tx, previewID string, selection RecoverySelection, preview *RecoveryPreview) error {
	var query string
	var args []any
	query = `UPDATE admin_recovery_targets SET
		affected_photos=(SELECT COUNT(*) FROM photos p WHERE p.hash=admin_recovery_targets.blob_hash),
		affected_albums=(SELECT COUNT(DISTINCT p.album_id) FROM photos p WHERE p.hash=admin_recovery_targets.blob_hash),
		selected_photos=(SELECT COUNT(*) FROM photos p WHERE p.hash=admin_recovery_targets.blob_hash`
	if len(selection.Albums) > 0 {
		ids := uniqueStrings(selection.Albums)
		query += ` AND p.album_id IN (` + placeholders(len(ids)) + `)`
		args = append(args, stringArgs(ids)...)
	} else if len(selection.Photos) > 0 {
		query += ` AND EXISTS (SELECT 1 FROM admin_recovery_selected_photos s WHERE s.album_id=p.album_id AND s.idx=p.idx)`
	} else if selection.Filter != nil && selection.Filter.AlbumID != "" {
		query += ` AND p.album_id=?`
		args = append(args, selection.Filter.AlbumID)
	}
	query += `) WHERE preview_id=?`
	args = append(args, previewID)
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("count scoped recovery impact: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(selected_photos),0), COALESCE(SUM(affected_photos),0) FROM admin_recovery_targets WHERE preview_id=?`, previewID).
		Scan(&preview.SelectedPhotos, &preview.AffectedPhotos); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT p.album_id) FROM photos p JOIN admin_recovery_targets t ON t.blob_hash=p.hash WHERE t.preview_id=?`, previewID).
		Scan(&preview.AffectedAlbums); err != nil {
		return err
	}
	return nil
}

func readRecoveryTargetState(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, hash, stage string) (recoveryTargetState, error) {
	var state recoveryTargetState
	var token string
	var started, finished, changed, phase sql.NullString
	err := q.QueryRowContext(ctx, `SELECT b.encoding_status, b.embedding_status, b.content_type,
		b.encoding_lease_until, b.embedding_lease_until, CASE WHEN ?='embedding' THEN b.embedding_token ELSE b.encoding_token END,
		b.embedding_finalizing!='',
		COALESCE(o.status,''), o.phase, o.started_at, o.finished_at, o.status_changed_at, COALESCE(o.attempts,0),
		COALESCE(o.error_code,''), COALESCE(o.retryability,'')
		FROM blobs b LEFT JOIN blob_stage_observations o ON o.blob_hash=b.hash AND o.stage=? WHERE b.hash=?`, stage, stage, hash).Scan(
		&state.EncodingStatus, &state.EmbeddingStatus, &state.ContentType, &state.EncodingLease, &state.EmbeddingLease, &token,
		&state.EmbeddingFinalizing,
		&state.ObsStatus, &phase, &started, &finished, &changed, &state.Attempts, &state.ErrorCode, &state.Retryability)
	if err != nil {
		return state, fmt.Errorf("read recovery generation: %w", err)
	}
	if phase.Valid {
		state.ObsPhase = phase.String
	}
	if token != "" {
		digest := sha256.Sum256([]byte(token))
		state.TokenHash = hex.EncodeToString(digest[:])
		state.TokenValue = token
	}
	if started.Valid {
		state.StartedAt = started.String
	}
	if finished.Valid {
		state.FinishedAt = finished.String
	}
	if changed.Valid {
		state.StatusChangedAt = changed.String
	}
	return state, nil
}

func recoveryEligibility(action string, state recoveryTargetState, now time.Time) (bool, string) {
	if state.ObsStatus == "" {
		return false, "observation_missing"
	}
	if state.ObsPhase == "reconciling" {
		return false, "reconciling"
	}
	switch action {
	case RecoveryRetryFailedEncoding:
		if state.EncodingStatus == "skipped" {
			if state.ContentType == "image/webp" {
				return false, "already_webp"
			}
			return false, "not_smaller"
		}
		if state.ContentType == "image/webp" {
			return false, "already_webp"
		}
		if state.EncodingStatus != "failed" {
			return false, "not_failed"
		}
	case RecoveryRetryFailedEmbedding:
		if state.EmbeddingStatus != "failed" {
			return false, "not_failed"
		}
	case RecoveryReleaseEncoding:
		if state.EncodingStatus == "received" || state.EncodingStatus == "committing" {
			return false, "unsafe_phase"
		}
		if state.EncodingStatus != "leased" {
			return false, "not_leased"
		}
		if state.EncodingLease >= now.UnixMilli() {
			return false, "lease_live"
		}
	case RecoveryReleaseEmbedding:
		if state.EmbeddingFinalizing {
			return false, "finalizing"
		}
		if state.EmbeddingStatus != "processing" {
			return false, "not_leased"
		}
		if state.EmbeddingLease >= now.UnixMilli() {
			return false, "lease_live"
		}
	}
	return true, ""
}

func recoveryGeneration(action string, state recoveryTargetState) string {
	stage := recoveryStage(action)
	value := struct {
		Action, EncodingStatus, EmbeddingStatus, ContentType                   string
		EncodingLease, EmbeddingLease                                          int64
		TokenHash, ObsStatus, ObsPhase, StartedAt, FinishedAt, StatusChangedAt string
		Attempts                                                               int64
		ErrorCode, Retryability                                                string
	}{Action: action, EncodingStatus: state.EncodingStatus, EmbeddingStatus: state.EmbeddingStatus,
		ContentType: state.ContentType, ObsStatus: state.ObsStatus, ObsPhase: state.ObsPhase,
		StartedAt: state.StartedAt, FinishedAt: state.FinishedAt, StatusChangedAt: state.StatusChangedAt,
		Attempts: state.Attempts, ErrorCode: state.ErrorCode, Retryability: state.Retryability}
	if stage == "encoding" {
		value.EncodingLease = state.EncodingLease
		if state.EncodingStatus == "leased" {
			value.TokenHash = state.TokenHash
		}
	} else {
		value.EmbeddingLease = state.EmbeddingLease
		if state.EmbeddingStatus == "processing" {
			value.TokenHash = state.TokenHash
		}
	}
	encoded, _ := json.Marshal(value)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func recoveryActor(actor string) string {
	actor = strings.TrimSpace(actor)
	actor = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, actor)
	if actor == "" {
		return "unknown"
	}
	if len(actor) > 100 {
		actor = actor[:100]
	}
	return actor
}

func newRecoveryID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("create recovery id: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func scanHashes(rows *sql.Rows, limit int) ([]string, error) {
	defer rows.Close()
	items := make([]string, 0)
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return nil, err
		}
		items = append(items, hash)
		if len(items) >= limit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

func placeholders(count int) string {
	if count <= 0 {
		return "NULL"
	}
	return strings.TrimRight(strings.Repeat("?,", count), ",")
}

func stringArgs(items []string) []any {
	args := make([]any, len(items))
	for i := range items {
		args[i] = items[i]
	}
	return args
}

func uniqueStrings(items []string) []string {
	seen := make(map[string]struct{}, len(items))
	result := make([]string, 0, len(items))
	for _, item := range items {
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		result = append(result, item)
	}
	sort.Strings(result)
	return result
}

func uniquePhotos(items []RecoveryPhoto) []RecoveryPhoto {
	seen := make(map[string]struct{}, len(items))
	result := make([]RecoveryPhoto, 0, len(items))
	for _, item := range items {
		key := fmt.Sprintf("%s\x00%d", item.AlbumID, item.Index)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, item)
	}
	return result
}

// CreateRecoveryOperation consumes one unexpired preview. Idempotency is scoped
// to the Basic username label and binds the key to the exact preview and action.
func (s *Store) CreateRecoveryOperation(ctx context.Context, actor, previewID, action, key, payloadHash string, now time.Time) (RecoveryOperation, bool, error) {
	if now.IsZero() {
		now = time.Now()
	}
	actor = recoveryActor(actor)
	key = strings.TrimSpace(key)
	if key == "" || len(key) > 200 || !printableIdempotencyKey(key) {
		return RecoveryOperation{}, false, fmt.Errorf("%w: invalid idempotency key", ErrInvalidRecoverySelection)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RecoveryOperation{}, false, err
	}
	defer tx.Rollback()
	if err := lockCatalogWriter(ctx, tx); err != nil {
		return RecoveryOperation{}, false, err
	}
	var existingID, existingPreview, existingAction, existingPayload string
	err = tx.QueryRowContext(ctx, `SELECT id, preview_id, action, payload_hash FROM admin_recovery_operations WHERE actor=? AND idempotency_key=?`, actor, key).
		Scan(&existingID, &existingPreview, &existingAction, &existingPayload)
	if err == nil {
		if existingPreview != previewID || existingAction != action || existingPayload != payloadHash {
			return RecoveryOperation{}, false, ErrRecoveryConflict
		}
		operation, err := readRecoveryOperation(ctx, tx, existingID)
		if err != nil {
			return RecoveryOperation{}, false, err
		}
		if err := tx.Commit(); err != nil {
			return RecoveryOperation{}, false, err
		}
		return operation, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return RecoveryOperation{}, false, err
	}
	var previewActor, previewAction, expires string
	var consumed sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT actor, action, expires_at, consumed_by FROM admin_recovery_previews WHERE id=?`, previewID).
		Scan(&previewActor, &previewAction, &expires, &consumed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RecoveryOperation{}, false, ErrRecoveryNotFound
		}
		return RecoveryOperation{}, false, err
	}
	if previewActor != actor || previewAction != action || !validRecoveryAction(action) {
		return RecoveryOperation{}, false, ErrRecoveryConflict
	}
	expiry, err := time.Parse(time.RFC3339Nano, expires)
	if err != nil || !now.Before(expiry) {
		return RecoveryOperation{}, false, ErrRecoveryConflict
	}
	if consumed.Valid {
		return RecoveryOperation{}, false, ErrRecoveryConflict
	}
	operationID, err := newRecoveryID()
	if err != nil {
		return RecoveryOperation{}, false, err
	}
	stamp := recoveryTimestamp(now)
	if _, err := tx.ExecContext(ctx, `INSERT INTO admin_recovery_operations
		(id, preview_id, actor, action, idempotency_key, payload_hash, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 'queued', ?, ?)`, operationID, previewID, actor, action, key, payloadHash, stamp, stamp); err != nil {
		return RecoveryOperation{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO admin_recovery_results(operation_id, ordinal, blob_hash, status, reason_code, updated_at)
		SELECT ?, ordinal, blob_hash, CASE WHEN eligible=1 THEN 'queued' ELSE 'skipped' END,
		CASE WHEN eligible=1 THEN '' ELSE skip_reason END, ?
		FROM admin_recovery_targets WHERE preview_id=? ORDER BY ordinal`, operationID, stamp, previewID); err != nil {
		return RecoveryOperation{}, false, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE admin_recovery_previews SET consumed_by=? WHERE id=? AND consumed_by IS NULL`, operationID, previewID)
	if err != nil {
		return RecoveryOperation{}, false, err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		return RecoveryOperation{}, false, ErrRecoveryConflict
	}
	operation, err := readRecoveryOperation(ctx, tx, operationID)
	if err != nil {
		return RecoveryOperation{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return RecoveryOperation{}, false, err
	}
	return operation, false, nil
}

func printableIdempotencyKey(key string) bool {
	for i := 0; i < len(key); i++ {
		if key[i] < 0x21 || key[i] > 0x7e {
			return false
		}
	}
	return true
}

const recoveryOperationSelect = `SELECT o.id, o.preview_id, o.actor, o.action, o.status, o.created_at, o.updated_at,
	(SELECT COUNT(*) FROM admin_recovery_results r WHERE r.operation_id=o.id),
	(SELECT COUNT(*) FROM admin_recovery_results r WHERE r.operation_id=o.id AND r.status!='skipped'),
	(SELECT COUNT(*) FROM admin_recovery_results r WHERE r.operation_id=o.id AND r.status='succeeded'),
	(SELECT COUNT(*) FROM admin_recovery_results r WHERE r.operation_id=o.id AND r.status='conflict'),
	(SELECT COUNT(*) FROM admin_recovery_results r WHERE r.operation_id=o.id AND r.status='skipped'),
	(SELECT COUNT(*) FROM admin_recovery_results r WHERE r.operation_id=o.id AND r.status='failed'),
	p.selected_photos, p.affected_photos, p.affected_albums
	FROM admin_recovery_operations o JOIN admin_recovery_previews p ON p.id=o.preview_id`

func readRecoveryOperation(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id string) (RecoveryOperation, error) {
	var operation RecoveryOperation
	err := q.QueryRowContext(ctx, recoveryOperationSelect+` WHERE o.id=?`, id).Scan(
		&operation.ID, &operation.PreviewID, &operation.Actor, &operation.Action, &operation.Status,
		&operation.CreatedAt, &operation.UpdatedAt, &operation.Total, &operation.Eligible,
		&operation.Succeeded, &operation.Conflicts, &operation.Skipped, &operation.Failed,
		&operation.SelectedPhotos, &operation.AffectedPhotos, &operation.AffectedAlbums)
	if errors.Is(err, sql.ErrNoRows) {
		return RecoveryOperation{}, ErrRecoveryNotFound
	}
	if err != nil {
		return RecoveryOperation{}, err
	}
	operation.ActorNote = "Basic-auth username label; token holder is not individually verified"
	operation.SharedBlobWarning = operation.AffectedPhotos > operation.SelectedPhotos
	return operation, nil
}

func (s *Store) RecoveryOperation(ctx context.Context, id string) (RecoveryOperation, error) {
	return readRecoveryOperation(ctx, s.db, id)
}

func recoveryResultCursor(value int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%d", value)))
}

func parseRecoveryResultCursor(value string) (int64, error) {
	if value == "" {
		return -1, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return 0, ErrInvalidRecoverySelection
	}
	cursor, err := strconv.ParseInt(string(data), 10, 64)
	if err != nil || cursor < -1 {
		return 0, ErrInvalidRecoverySelection
	}
	return cursor, nil
}

func (s *Store) RecoveryResults(ctx context.Context, id string, limit int, cursor string) (RecoveryResultPage, error) {
	if limit < 1 || limit > RecoveryPageLimit {
		return RecoveryResultPage{}, fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalidRecoverySelection, RecoveryPageLimit)
	}
	operation, err := s.RecoveryOperation(ctx, id)
	if err != nil {
		return RecoveryResultPage{}, err
	}
	after, err := parseRecoveryResultCursor(cursor)
	if err != nil {
		return RecoveryResultPage{}, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT ordinal, blob_hash, status, reason_code, updated_at
		FROM admin_recovery_results WHERE operation_id=? AND ordinal>? ORDER BY ordinal LIMIT ?`, id, after, limit+1)
	if err != nil {
		return RecoveryResultPage{}, err
	}
	defer rows.Close()
	page := RecoveryResultPage{Operation: operation, Results: make([]RecoveryResult, 0, limit)}
	for rows.Next() {
		var item RecoveryResult
		if err := rows.Scan(&item.Ordinal, &item.Hash, &item.Status, &item.Reason, &item.UpdatedAt); err != nil {
			return RecoveryResultPage{}, err
		}
		page.Results = append(page.Results, item)
	}
	if err := rows.Err(); err != nil {
		return RecoveryResultPage{}, err
	}
	if len(page.Results) > limit {
		page.Results = page.Results[:limit]
		page.NextCursor = ptrString(recoveryResultCursor(page.Results[len(page.Results)-1].Ordinal))
	}
	return page, nil
}

func ptrString(value string) *string { return &value }

type RecoveryOperationListOptions struct {
	Limit        int
	AfterCreated string
	AfterID      string
}

func (s *Store) RecoveryOperations(ctx context.Context, options RecoveryOperationListOptions) (RecoveryOperationPage, error) {
	if options.Limit < 1 || options.Limit > RecoveryPageLimit {
		return RecoveryOperationPage{}, fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalidRecoverySelection, RecoveryPageLimit)
	}
	query := recoveryOperationSelect
	args := make([]any, 0, 4)
	if options.AfterCreated != "" {
		query += ` WHERE (o.created_at < ? OR (o.created_at = ? AND o.id < ?))`
		args = append(args, options.AfterCreated, options.AfterCreated, options.AfterID)
	}
	query += ` ORDER BY o.created_at DESC, o.id DESC LIMIT ?`
	args = append(args, options.Limit+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return RecoveryOperationPage{}, err
	}
	defer rows.Close()
	page := RecoveryOperationPage{Operations: make([]RecoveryOperation, 0, options.Limit)}
	for rows.Next() {
		var operation RecoveryOperation
		if err := rows.Scan(&operation.ID, &operation.PreviewID, &operation.Actor, &operation.Action, &operation.Status,
			&operation.CreatedAt, &operation.UpdatedAt, &operation.Total, &operation.Eligible,
			&operation.Succeeded, &operation.Conflicts, &operation.Skipped, &operation.Failed,
			&operation.SelectedPhotos, &operation.AffectedPhotos, &operation.AffectedAlbums); err != nil {
			return RecoveryOperationPage{}, err
		}
		operation.ActorNote = "Basic-auth username label; token holder is not individually verified"
		operation.SharedBlobWarning = operation.AffectedPhotos > operation.SelectedPhotos
		page.Operations = append(page.Operations, operation)
	}
	if err := rows.Err(); err != nil {
		return RecoveryOperationPage{}, err
	}
	if len(page.Operations) > options.Limit {
		page.Operations = page.Operations[:options.Limit]
		last := page.Operations[len(page.Operations)-1]
		cursor, _ := json.Marshal([]string{last.CreatedAt, last.ID})
		page.NextCursor = ptrString(base64.RawURLEncoding.EncodeToString(cursor))
	}
	return page, nil
}

// ClaimRecoveryOperation moves one durable queued operation to running. Open
// returns abandoned running operations to queued before any worker starts.
func (s *Store) ClaimRecoveryOperation(ctx context.Context) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if err := lockCatalogWriter(ctx, tx); err != nil {
		return "", err
	}
	var id string
	err = tx.QueryRowContext(ctx, `SELECT id FROM admin_recovery_operations WHERE status='queued' ORDER BY created_at, id LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", tx.Commit()
	}
	if err != nil {
		return "", err
	}
	result, err := tx.ExecContext(ctx, `UPDATE admin_recovery_operations SET status='running', updated_at=? WHERE id=? AND status='queued'`, observationTime(time.Now()), id)
	if err != nil {
		return "", err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
}

// ProcessRecoveryTarget performs one previewed target transition and records
// its result in the same transaction as the catalog mutation and observation.
func (s *Store) ProcessRecoveryTarget(ctx context.Context, operationID string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err := lockCatalogWriter(ctx, tx); err != nil {
		return false, err
	}
	var action string
	if err := tx.QueryRowContext(ctx, `SELECT action FROM admin_recovery_operations WHERE id=? AND status='running'`, operationID).Scan(&action); err != nil {
		return false, err
	}
	var ordinal int64
	var hash, expected string
	if err := tx.QueryRowContext(ctx, `SELECT r.ordinal, r.blob_hash, t.generation FROM admin_recovery_results r
		JOIN admin_recovery_targets t ON t.preview_id=(SELECT preview_id FROM admin_recovery_operations WHERE id=r.operation_id) AND t.ordinal=r.ordinal
		WHERE r.operation_id=? AND r.status='queued' ORDER BY r.ordinal LIMIT 1`, operationID).Scan(&ordinal, &hash, &expected); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, tx.Commit()
		}
		return false, err
	}
	now := time.Now()
	state, stateErr := readRecoveryTargetState(ctx, tx, hash, recoveryStage(action))
	if errors.Is(stateErr, sql.ErrNoRows) {
		if err := setRecoveryResult(ctx, tx, operationID, ordinal, "conflict", "target_missing", now); err != nil {
			return false, err
		}
		if err := tx.Commit(); err != nil {
			return false, err
		}
		return true, nil
	}
	if stateErr != nil {
		return false, stateErr
	}
	if recoveryGeneration(action, state) != expected {
		if err := setRecoveryResult(ctx, tx, operationID, ordinal, "conflict", "state_changed", now); err != nil {
			return false, err
		}
	} else if eligible, _ := recoveryEligibility(action, state, now); !eligible {
		if err := setRecoveryResult(ctx, tx, operationID, ordinal, "conflict", "state_changed", now); err != nil {
			return false, err
		}
	} else {
		changed, err := applyRecoveryTransition(ctx, tx, action, hash, state, now)
		if err != nil {
			return false, err
		}
		if !changed {
			if err := setRecoveryResult(ctx, tx, operationID, ordinal, "conflict", "state_changed", now); err != nil {
				return false, err
			}
		} else if err := setRecoveryResult(ctx, tx, operationID, ordinal, "succeeded", "", now); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// NoteRecoveryExecutionFailure durably bounds retries for an unexpected
// per-target catalog failure. The raw error remains in process logs only; the
// result exposes a short safe code after the fifth unsuccessful attempt.
func (s *Store) NoteRecoveryExecutionFailure(ctx context.Context, operationID string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err := lockCatalogWriter(ctx, tx); err != nil {
		return false, err
	}
	var ordinal int64
	var attempts int
	if err := tx.QueryRowContext(ctx, `SELECT ordinal, execution_attempts FROM admin_recovery_results
		WHERE operation_id=? AND status='queued' ORDER BY ordinal LIMIT 1`, operationID).Scan(&ordinal, &attempts); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, tx.Commit()
		}
		return false, err
	}
	attempts++
	if attempts >= recoveryTargetMaxAttempts {
		if err := setRecoveryResult(ctx, tx, operationID, ordinal, "failed", "execution_error", time.Now()); err != nil {
			return false, err
		}
	} else if _, err := tx.ExecContext(ctx, `UPDATE admin_recovery_results SET execution_attempts=?, updated_at=?
		WHERE operation_id=? AND ordinal=? AND status='queued'`, attempts, observationTime(time.Now()), operationID, ordinal); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return attempts >= recoveryTargetMaxAttempts, nil
}

func applyRecoveryTransition(ctx context.Context, tx *sql.Tx, action, hash string, state recoveryTargetState, now time.Time) (bool, error) {
	var query string
	var args []any
	var event, status, stage string
	switch action {
	case RecoveryRetryFailedEncoding:
		stage, event, status = "encoding", "admin_retry", "pending"
		query = `UPDATE blobs SET encoding_status='pending', encoding_token='', encoding_stage_key='', encoding_lease_until=0,
			encoding_source_size_bytes=0, encoding_error='' WHERE hash=? AND encoding_status='failed' AND content_type!='image/webp'`
		args = []any{hash}
	case RecoveryRetryFailedEmbedding:
		stage, event, status = "embedding", "admin_retry", "pending"
		query = `UPDATE blobs SET embedding_status='pending', embedding_error='', embedding_lease_until=0 WHERE hash=? AND embedding_status='failed'`
		args = []any{hash}
	case RecoveryReleaseEncoding:
		stage, event, status = "encoding", "lease_expired", "pending"
		query = `UPDATE blobs SET encoding_status='pending', encoding_token='', encoding_stage_key='', encoding_lease_until=0
			WHERE hash=? AND encoding_status='leased' AND encoding_lease_until=? AND encoding_token=?`
		args = []any{hash, state.EncodingLease, state.TokenValue}
	case RecoveryReleaseEmbedding:
		stage, event, status = "embedding", "lease_expired", "pending"
		query = `UPDATE blobs SET embedding_status='pending', embedding_error='', embedding_lease_until=0, embedding_owner='external', embedding_token=''
			WHERE hash=? AND embedding_status='processing' AND embedding_lease_until=? AND embedding_token=? AND embedding_finalizing=''`
		args = []any{hash, state.EmbeddingLease, state.TokenValue}
	default:
		return false, fmt.Errorf("unsupported recovery action")
	}
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		return false, err
	}
	if _, err := observeStage(ctx, tx, hash, stage, event, status, "", now, false, false, false, "", "unknown"); err != nil {
		return false, err
	}
	return true, nil
}

func setRecoveryResult(ctx context.Context, tx *sql.Tx, operationID string, ordinal int64, status, reason string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE admin_recovery_results SET status=?, reason_code=?, updated_at=?
		WHERE operation_id=? AND ordinal=? AND status='queued'`, status, reason, observationTime(now), operationID, ordinal)
	return err
}

func (s *Store) FinishRecoveryOperation(ctx context.Context, id string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err := lockCatalogWriter(ctx, tx); err != nil {
		return false, err
	}
	var queued int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM admin_recovery_results WHERE operation_id=? AND status='queued'`, id).Scan(&queued); err != nil {
		return false, err
	}
	if queued > 0 {
		return false, tx.Commit()
	}
	var succeeded, bad, eligible int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(status='succeeded'),0), COALESCE(SUM(status IN ('conflict','failed')),0), COALESCE(SUM(status!='skipped'),0)
		FROM admin_recovery_results WHERE operation_id=?`, id).Scan(&succeeded, &bad, &eligible); err != nil {
		return false, err
	}
	if succeeded < 0 {
		succeeded = 0
	}
	if bad < 0 {
		bad = 0
	}
	if eligible < 0 {
		eligible = 0
	}
	status := "completed"
	if bad > 0 {
		if succeeded > 0 {
			status = "partially_failed"
		} else if eligible > 0 {
			status = "failed"
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE admin_recovery_operations SET status=?, updated_at=? WHERE id=? AND status='running'`, status, observationTime(time.Now()), id); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) ResumeRecoveryOperations(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE admin_recovery_operations SET status='queued', updated_at=? WHERE status='running'`, observationTime(time.Now()))
	return err
}
