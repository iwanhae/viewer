package encoding

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"viewer/internal/catalog"
	"viewer/internal/storage"
)

// A 1x1 opaque WebP encoded with cwebp -q 85 -alpha_q 100 -metadata all.
const tinyWebP = "UklGRkAAAABXRUJQVlA4IDQAAADwAQCdASoBAAEAAQAcJaACdLoB+AAETAAA/vW4f/6aR40jxpHxcP/ugT90CfugT/3NoAAA"

type memoryStore struct {
	objects                     map[string][]byte
	types                       map[string]string
	copyReturnsErrorBeforeWrite bool
	copyReturnsErrorAfterWrite  bool
	putStarted                  chan struct{}
	putRelease                  chan struct{}
	putFinished                 chan struct{}
	putCalls                    int
}

func newMemoryStore() *memoryStore {
	return &memoryStore{objects: map[string][]byte{}, types: map[string]string{}}
}
func (m *memoryStore) GetObject(_ context.Context, key string) (io.ReadCloser, string, error) {
	data, ok := m.objects[key]
	if !ok {
		return nil, "", storage.ErrObjectNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), m.types[key], nil
}
func (m *memoryStore) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	return "memory://" + key, nil
}
func (m *memoryStore) PresignPut(_ context.Context, key string, _ time.Duration) (string, error) {
	return "memory://" + key, nil
}
func (m *memoryStore) PutObject(_ context.Context, key string, body io.Reader, contentType string) error {
	m.putCalls++
	if m.putStarted != nil {
		close(m.putStarted)
	}
	if m.putRelease != nil {
		<-m.putRelease
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	m.objects[key] = data
	m.types[key] = contentType
	if m.putFinished != nil {
		close(m.putFinished)
	}
	return nil
}
func (m *memoryStore) StatObject(_ context.Context, key string) (storage.Object, bool, error) {
	data, ok := m.objects[key]
	if !ok {
		return storage.Object{}, false, nil
	}
	sum := sha256.Sum256(data)
	return storage.Object{Key: key, Size: int64(len(data)), ETag: hex.EncodeToString(sum[:]), LastModified: time.Now()}, true, nil
}
func (m *memoryStore) CopyObjectIfMatch(ctx context.Context, from, to, etag, ct string) error {
	info, ok, _ := m.StatObject(ctx, from)
	if !ok || info.ETag != etag {
		return ErrLostLease
	}
	if m.copyReturnsErrorBeforeWrite {
		return errors.New("copy request failed before object replacement")
	}
	m.objects[to] = bytes.Clone(m.objects[from])
	m.types[to] = ct
	if m.copyReturnsErrorAfterWrite {
		return errors.New("copy response lost after object replacement")
	}
	return nil
}
func (m *memoryStore) DeleteObjects(_ context.Context, keys []string) error {
	for _, key := range keys {
		delete(m.objects, key)
	}
	return nil
}
func (m *memoryStore) ListObjects(_ context.Context, prefix string) ([]storage.Object, error) {
	var out []storage.Object
	for key := range m.objects {
		if strings.HasPrefix(key, prefix) {
			info, _, _ := m.StatObject(context.Background(), key)
			out = append(out, info)
		}
	}
	return out, nil
}

func sourcePNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.NRGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	// Include an ancillary chunk to make the source larger than the WebP.
	payload := append([]byte("Comment\x00"), bytes.Repeat([]byte{'a'}, 200)...)
	chunk := make([]byte, 12+len(payload))
	binary.BigEndian.PutUint32(chunk[:4], uint32(len(payload)))
	copy(chunk[4:8], "tEXt")
	copy(chunk[8:], payload)
	binary.BigEndian.PutUint32(chunk[len(chunk)-4:], crc32.ChecksumIEEE(chunk[4:len(chunk)-4]))
	return append(append(bytes.Clone(data[:len(data)-12]), chunk...), data[len(data)-12:]...)
}

func waitForEncodingStatus(t *testing.T, cat *catalog.Store, hash, want string) catalog.EncodingJob {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		job, err := cat.GetEncodingJob(context.Background(), hash)
		if err != nil {
			t.Fatal(err)
		}
		if job.Status == want {
			return job
		}
		select {
		case <-deadline.C:
			t.Fatalf("encoding %s status=%q, want %q", hash, job.Status, want)
		case <-ticker.C:
		}
	}
}

func TestReceivedOutputIsValidatedAndPutAsynchronously(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	source := sourcePNG(t)
	sum := sha256.Sum256(source)
	hash := hex.EncodeToString(sum[:])
	if err := cat.UpsertBlob(ctx, catalog.Blob{Hash: hash, SizeBytes: int64(len(source)), ContentType: "image/png", EncodingGate: true}); err != nil {
		t.Fatal(err)
	}
	encoded, err := base64.StdEncoding.DecodeString(tinyWebP)
	if err != nil {
		t.Fatal(err)
	}
	store := newMemoryStore()
	store.objects["blobs/"+hash] = source
	store.types["blobs/"+hash] = "image/png"
	store.putStarted = make(chan struct{})
	store.putRelease = make(chan struct{})
	store.putFinished = make(chan struct{})
	svc := New(cat, store)
	svc.spoolDir = t.TempDir()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	claimed, err := svc.Claim(ctx, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: jobs=%+v err=%v", claimed, err)
	}
	job := claimed[0]
	accepted := make(chan error, 1)
	go func() { accepted <- svc.ReceiveOutput(ctx, hash, job.Token, bytes.NewReader(encoded)) }()

	select {
	case <-store.putStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("background processor did not reach S3 PutObject")
	}
	// The API-facing receive call must return while the S3 write is still
	// blocked, proving it did not perform validation/upload synchronously.
	select {
	case err := <-accepted:
		if err != nil {
			t.Fatalf("receive output: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ReceiveOutput waited for the background S3 write")
	}
	committing, err := cat.GetEncodingJob(ctx, hash)
	if err != nil || committing.Status != "committing" {
		t.Fatalf("job before releasing S3 write: %+v err=%v", committing, err)
	}
	if !bytes.Equal(store.objects["blobs/"+hash], source) {
		t.Fatal("original blob changed before the S3 PutObject completed")
	}
	close(store.putRelease)
	select {
	case <-store.putFinished:
	case <-time.After(5 * time.Second):
		t.Fatal("background S3 PutObject did not finish")
	}
	done := waitForEncodingStatus(t, cat, hash, "done")
	if done.Type != "image/webp" || done.SizeBytes != int64(len(encoded)) {
		t.Fatalf("finished job metadata: %+v", done)
	}
	if store.putCalls != 1 || !bytes.Equal(store.objects["blobs/"+hash], encoded) || store.types["blobs/"+hash] != "image/webp" {
		t.Fatal("validated WebP was not put at the original blob key")
	}
	if _, err := os.Stat(svc.spoolPath(hash, job.Token)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("spool file still exists after completion: %v", err)
	}
}

func TestInvalidReceivedOutputResetsWithoutReplacingBlob(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	source := sourcePNG(t)
	sum := sha256.Sum256(source)
	hash := hex.EncodeToString(sum[:])
	if err := cat.UpsertBlob(ctx, catalog.Blob{Hash: hash, SizeBytes: int64(len(source)), ContentType: "image/png"}); err != nil {
		t.Fatal(err)
	}
	store := newMemoryStore()
	store.objects["blobs/"+hash] = source
	store.types["blobs/"+hash] = "image/png"
	svc := New(cat, store)
	svc.spoolDir = t.TempDir()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	claimed, err := svc.Claim(ctx, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: jobs=%+v err=%v", claimed, err)
	}
	job := claimed[0]
	if err := svc.ReceiveOutput(ctx, hash, job.Token, bytes.NewReader([]byte("not a webp"))); err != nil {
		t.Fatalf("receive invalid-but-bounded output: %v", err)
	}
	waitForEncodingStatus(t, cat, hash, "pending")
	if store.putCalls != 0 || !bytes.Equal(store.objects["blobs/"+hash], source) {
		t.Fatal("invalid output reached S3 or replaced the original blob")
	}
	if _, err := os.Stat(svc.spoolPath(hash, job.Token)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid spool file still exists: %v", err)
	}
}

func TestStartResetsReceivedOutputWhenTempFileWasLost(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	if err := cat.UpsertBlob(ctx, catalog.Blob{Hash: strings.Repeat("a", 64), SizeBytes: 100, ContentType: "image/png"}); err != nil {
		t.Fatal(err)
	}
	claimed, err := cat.ClaimEncoding(ctx, 1, LeaseTTL)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: jobs=%+v err=%v", claimed, err)
	}
	if ok, err := cat.MarkEncodingReceived(ctx, claimed[0].Hash, claimed[0].Token, LeaseTTL); err != nil || !ok {
		t.Fatalf("mark received: ok=%v err=%v", ok, err)
	}
	svc := New(cat, newMemoryStore())
	svc.spoolDir = t.TempDir() // models a fresh/cleared OS temp directory
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	job, err := cat.GetEncodingJob(ctx, claimed[0].Hash)
	if err != nil || job.Status != "pending" {
		t.Fatalf("missing /tmp result was not returned to pending: job=%+v err=%v", job, err)
	}
}

func TestEncodingCommitAndDuplicateUpload(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	source := sourcePNG(t)
	sum := sha256.Sum256(source)
	hash := hex.EncodeToString(sum[:])
	encoded, err := base64.StdEncoding.DecodeString(tinyWebP)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) >= len(source) {
		t.Fatal("fixture must save bytes")
	}
	if err := cat.UpsertBlob(ctx, catalog.Blob{Hash: hash, SizeBytes: int64(len(source)), ContentType: "image/png", EncodingGate: true}); err != nil {
		t.Fatal(err)
	}
	store := newMemoryStore()
	key := "blobs/" + hash
	store.objects[key] = source
	store.types[key] = "image/png"
	if pending, err := cat.ClaimPendingEmbeddings(ctx, 1, time.Now().Add(time.Minute)); err != nil || len(pending) != 0 {
		t.Fatalf("new image embedded before encoding: %+v %v", pending, err)
	}
	svc := New(cat, store)
	claimed, err := svc.Claim(ctx, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %v %+v", err, claimed)
	}
	job, err := cat.GetEncodingJob(ctx, hash)
	if err != nil {
		t.Fatal(err)
	}
	store.objects[job.StageKey] = encoded
	status, err := svc.Complete(ctx, hash, job.Token, "uploaded", "")
	if err != nil || status != "done" {
		t.Fatalf("complete: %q %v", status, err)
	}
	if !bytes.Equal(store.objects[key], encoded) || store.types[key] != "image/webp" {
		t.Fatal("original key was not replaced with WebP")
	}
	if status, err := svc.Complete(ctx, hash, job.Token, "uploaded", ""); err != nil || status != "done" {
		t.Fatalf("duplicate complete: %q %v", status, err)
	}
	if err := cat.UpsertBlob(ctx, catalog.Blob{Hash: hash, SizeBytes: int64(len(source)), ContentType: "image/png"}); err != nil {
		t.Fatal(err)
	}
	counts, err := cat.EncodingCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Converted != 1 || counts.BytesSaved != int64(len(source)-len(encoded)) {
		t.Fatalf("encoding counts after duplicate upload=%+v want savings %d", counts, len(source)-len(encoded))
	}
	blob, err := cat.GetBlob(ctx, hash)
	if err != nil {
		t.Fatal(err)
	}
	if blob.ContentType != "image/webp" || blob.SizeBytes != int64(len(encoded)) {
		t.Fatalf("duplicate upload clobbered metadata: %+v", blob)
	}
	if pending, err := cat.ClaimPendingEmbeddings(ctx, 1, time.Now().Add(time.Minute)); err != nil || len(pending) != 1 {
		t.Fatalf("encoded image not released to embedder: %+v %v", pending, err)
	}
}

func TestEncodingReconcilesAmbiguousCopyError(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	source := sourcePNG(t)
	sum := sha256.Sum256(source)
	hash := hex.EncodeToString(sum[:])
	encoded, err := base64.StdEncoding.DecodeString(tinyWebP)
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.UpsertBlob(ctx, catalog.Blob{Hash: hash, SizeBytes: int64(len(source)), ContentType: "image/png"}); err != nil {
		t.Fatal(err)
	}
	store := newMemoryStore()
	key := "blobs/" + hash
	store.objects[key] = source
	store.types[key] = "image/png"
	svc := New(cat, store)
	claimed, err := svc.Claim(ctx, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: err=%v claimed=%+v", err, claimed)
	}
	job, err := cat.GetEncodingJob(ctx, hash)
	if err != nil {
		t.Fatal(err)
	}
	store.objects[job.StageKey] = encoded
	store.copyReturnsErrorAfterWrite = true
	status, err := svc.Complete(ctx, hash, job.Token, "uploaded", "")
	if err != nil || status != "done" {
		t.Fatalf("complete after ambiguous copy: status=%q err=%v", status, err)
	}
	if !bytes.Equal(store.objects[key], encoded) || store.types[key] != "image/webp" {
		t.Fatal("ambiguous copy did not leave the WebP object at the original key")
	}
	counts, err := cat.EncodingCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Converted != 1 || counts.BytesSaved != int64(len(source)-len(encoded)) {
		t.Fatalf("encoding counts after ambiguous copy=%+v want savings %d", counts, len(source)-len(encoded))
	}
}

func TestEncodingKeepsCommitLeasedWhenCopyIsNotYetVisible(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	source := sourcePNG(t)
	sum := sha256.Sum256(source)
	hash := hex.EncodeToString(sum[:])
	encoded, err := base64.StdEncoding.DecodeString(tinyWebP)
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.UpsertBlob(ctx, catalog.Blob{Hash: hash, SizeBytes: int64(len(source)), ContentType: "image/png"}); err != nil {
		t.Fatal(err)
	}
	store := newMemoryStore()
	key := "blobs/" + hash
	store.objects[key] = source
	store.types[key] = "image/png"
	store.copyReturnsErrorBeforeWrite = true
	svc := New(cat, store)
	claimed, err := svc.Claim(ctx, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: err=%v claimed=%+v", err, claimed)
	}
	job, err := cat.GetEncodingJob(ctx, hash)
	if err != nil {
		t.Fatal(err)
	}
	store.objects[job.StageKey] = encoded
	if status, err := svc.Complete(ctx, hash, job.Token, "uploaded", ""); err == nil || status != "" {
		t.Fatalf("complete before delayed copy: status=%q err=%v want copy error", status, err)
	}
	job, err = cat.GetEncodingJob(ctx, hash)
	if err != nil || job.Status != "committing" {
		t.Fatalf("job after ambiguous error: %+v err=%v want committing", job, err)
	}
	if err := svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	job, err = cat.GetEncodingJob(ctx, hash)
	if err != nil || job.Status != "committing" {
		t.Fatalf("recovery before grace expiry: %+v err=%v want committing", job, err)
	}

	// Model the object-store request becoming visible after its client timed out.
	store.objects[key] = encoded
	if err := svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	counts, err := cat.EncodingCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Processing != 1 || counts.Converted != 0 {
		t.Fatalf("counts while commit lease is active=%+v want one processing, uncounted commit", counts)
	}
}

func TestAlreadyWebPClaimIsExcludedFromConversionSavings(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	webp, err := base64.StdEncoding.DecodeString(tinyWebP)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(webp)
	hash := hex.EncodeToString(sum[:])
	if err := cat.UpsertBlob(ctx, catalog.Blob{Hash: hash, SizeBytes: int64(len(webp)), ContentType: "image/png"}); err != nil {
		t.Fatal(err)
	}
	store := newMemoryStore()
	store.objects["blobs/"+hash] = webp
	store.types["blobs/"+hash] = "image/webp"
	claimed, err := New(cat, store).Claim(ctx, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: err=%v claimed=%+v", err, claimed)
	}
	status, err := New(cat, store).Complete(ctx, hash, claimed[0].Token, "uploaded", "")
	if err != nil || status != "skipped" {
		t.Fatalf("complete already-WebP object: status=%q err=%v", status, err)
	}
	counts, err := cat.EncodingCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Candidates != 0 || counts.AlreadyWebP != 1 || counts.Converted != 0 || counts.BytesSaved != 0 {
		t.Fatalf("already-WebP stats=%+v want excluded from candidates and savings", counts)
	}
}

func TestEncodingLargestFirstAndStaleToken(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	for _, item := range []struct {
		hash string
		size int64
	}{{"small", 10}, {"large", 100}, {"middle", 50}} {
		if err := cat.UpsertBlob(ctx, catalog.Blob{Hash: item.hash, SizeBytes: item.size, ContentType: "image/png"}); err != nil {
			t.Fatal(err)
		}
	}
	jobs, err := cat.ClaimEncoding(ctx, 2, -time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if jobs[0].Hash != "large" || jobs[1].Hash != "middle" {
		t.Fatalf("wrong order: %+v", jobs)
	}
	newJobs, err := cat.ClaimEncoding(ctx, 1, LeaseTTL)
	if err != nil {
		t.Fatal(err)
	}
	if newJobs[0].Hash != "large" || newJobs[0].Token == jobs[0].Token {
		t.Fatalf("expected reclaimed large job: %+v", newJobs)
	}
	if ok, err := cat.BeginEncodingCommit(ctx, "large", jobs[0].Token, jobs[0].SizeBytes, LeaseTTL); err != nil || ok {
		t.Fatalf("stale token accepted: %v %v", ok, err)
	}
}

func TestRecoverCompletedCopy(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	source := sourcePNG(t)
	sum := sha256.Sum256(source)
	hash := hex.EncodeToString(sum[:])
	encoded, _ := base64.StdEncoding.DecodeString(tinyWebP)
	if err := cat.UpsertBlob(ctx, catalog.Blob{Hash: hash, SizeBytes: int64(len(source)), ContentType: "image/png"}); err != nil {
		t.Fatal(err)
	}
	jobs, err := cat.ClaimEncoding(ctx, 1, LeaseTTL)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := cat.BeginEncodingCommit(ctx, hash, jobs[0].Token, int64(len(source)), 0); err != nil || !ok {
		t.Fatalf("begin: %v %v", ok, err)
	}
	store := newMemoryStore()
	store.objects["blobs/"+hash] = encoded
	if err := New(cat, store).Recover(ctx); err != nil {
		t.Fatal(err)
	}
	job, err := cat.GetEncodingJob(ctx, hash)
	if err != nil || job.Status != "done" || job.Type != "image/webp" {
		t.Fatalf("recovery: %+v %v", job, err)
	}
	counts, err := cat.EncodingCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Converted != 1 || counts.BytesSaved != int64(len(source)-len(encoded)) {
		t.Fatalf("recovered encoding counts=%+v want one conversion saving %d bytes", counts, len(source)-len(encoded))
	}
}
