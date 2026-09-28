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
	putReturnsErrorAfterWrite   bool
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
func (m *memoryStore) PutObjectIfMatch(ctx context.Context, key string, body io.Reader, contentType, etag string) error {
	m.putCalls++
	if m.putStarted != nil {
		close(m.putStarted)
	}
	if m.putRelease != nil {
		<-m.putRelease
	}
	info, exists, err := m.StatObject(ctx, key)
	if err != nil {
		return err
	}
	if !exists || info.ETag != etag {
		return ErrLostLease
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	m.objects[key] = data
	m.types[key] = contentType
	if m.putReturnsErrorAfterWrite {
		return errors.New("put response lost after object replacement")
	}
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
	svc := New(cat, store, t.TempDir())
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
	svc := New(cat, store, t.TempDir())
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

func TestStartResetsReceivedOutputWhenSpoolFileWasLost(t *testing.T) {
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
	if ok, err := cat.MarkEncodingReceived(ctx, claimed[0].Hash, claimed[0].Token); err != nil || !ok {
		t.Fatalf("mark received: ok=%v err=%v", ok, err)
	}
	svc := New(cat, newMemoryStore(), t.TempDir()) // models a fresh/cleared spool directory
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	job, err := cat.GetEncodingJob(ctx, claimed[0].Hash)
	if err != nil || job.Status != "pending" {
		t.Fatalf("missing spool result was not returned to pending: job=%+v err=%v", job, err)
	}
}

func TestStartRejectsSymlinkedSpool(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "spool")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	svc := New(cat, newMemoryStore(), link)
	if err := svc.Start(context.Background()); err == nil {
		t.Fatal("symlinked spool was accepted")
	}
	info, err := os.Stat(target)
	if err != nil || info.Mode().Perm() != before.Mode().Perm() {
		t.Fatalf("symlink target was modified: info=%v err=%v", info, err)
	}
}

func TestStartProcessesReceivedOutputFromPersistentSpool(t *testing.T) {
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
	claimed, err := cat.ClaimEncoding(ctx, 1, LeaseTTL)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: jobs=%+v err=%v", claimed, err)
	}
	job := claimed[0]
	encoded, err := base64.StdEncoding.DecodeString(tinyWebP)
	if err != nil {
		t.Fatal(err)
	}
	spoolDir := filepath.Join(t.TempDir(), "encoding-spool")
	if err := os.MkdirAll(spoolDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spoolDir, hash+"_"+job.Token+".webp"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, err := cat.MarkEncodingReceived(ctx, hash, job.Token); err != nil || !ok {
		t.Fatalf("mark received: ok=%v err=%v", ok, err)
	}
	store := newMemoryStore()
	store.objects["blobs/"+hash] = source
	store.types["blobs/"+hash] = "image/png"
	svc := New(cat, store, spoolDir) // a new process reopening the same volume
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitForEncodingStatus(t, cat, hash, "done")
	if !bytes.Equal(store.objects["blobs/"+hash], encoded) {
		t.Fatal("restart did not commit the accepted spool file")
	}
}

func TestAmbiguousConditionalPutFinishesOnlyMatchingOutput(t *testing.T) {
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
	encoded, err := base64.StdEncoding.DecodeString(tinyWebP)
	if err != nil {
		t.Fatal(err)
	}
	store := newMemoryStore()
	store.objects["blobs/"+hash] = source
	store.types["blobs/"+hash] = "image/png"
	store.putReturnsErrorAfterWrite = true
	svc := New(cat, store, t.TempDir())
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	claimed, err := svc.ClaimForAPI(ctx, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: jobs=%+v err=%v", claimed, err)
	}
	if err := svc.ReceiveOutput(ctx, hash, claimed[0].Token, bytes.NewReader(encoded)); err != nil {
		t.Fatal(err)
	}
	waitForEncodingStatus(t, cat, hash, "done")
	if store.putCalls != 1 || !bytes.Equal(store.objects["blobs/"+hash], encoded) {
		t.Fatalf("ambiguous PUT did not finalize the observed output: calls=%d", store.putCalls)
	}
}

func TestConditionalPutDoesNotOverwriteRestoredSource(t *testing.T) {
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
	encoded, err := base64.StdEncoding.DecodeString(tinyWebP)
	if err != nil {
		t.Fatal(err)
	}
	store := newMemoryStore()
	key := "blobs/" + hash
	store.objects[key] = source
	store.types[key] = "image/png"
	store.putStarted = make(chan struct{})
	store.putRelease = make(chan struct{})
	svc := New(cat, store, t.TempDir())
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	claimed, err := svc.ClaimForAPI(ctx, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: jobs=%+v err=%v", claimed, err)
	}
	job := claimed[0]
	if err := svc.ReceiveOutput(ctx, hash, job.Token, bytes.NewReader(encoded)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-store.putStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("background PUT did not start")
	}
	restored := append(bytes.Clone(source), []byte("new source version")...)
	store.objects[key] = restored
	if err := cat.UpsertBlob(ctx, catalog.Blob{Hash: hash, SizeBytes: int64(len(restored)), ContentType: "image/png", SourceRestored: true}); err != nil {
		t.Fatal(err)
	}
	close(store.putRelease)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(svc.spoolPath(hash, job.Token)); errors.Is(err, os.ErrNotExist) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stale upload was not discarded")
		}
		time.Sleep(5 * time.Millisecond)
	}
	current := waitForEncodingStatus(t, cat, hash, "pending")
	if current.Token != "" || !bytes.Equal(store.objects[key], restored) || store.putCalls != 1 {
		t.Fatalf("stale output replaced restored source: job=%+v putCalls=%d", current, store.putCalls)
	}
}

func TestRecoveryRejectsDifferentWebPWhenSpoolExists(t *testing.T) {
	ctx := context.Background()
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
	claimed, err := cat.ClaimEncoding(ctx, 1, LeaseTTL)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: jobs=%+v err=%v", claimed, err)
	}
	job := claimed[0]
	if ok, err := cat.MarkEncodingReceived(ctx, hash, job.Token); err != nil || !ok {
		t.Fatalf("mark received: ok=%v err=%v", ok, err)
	}
	if ok, err := cat.BeginEncodingCommit(ctx, hash, job.Token, int64(len(source)), 0); err != nil || !ok {
		t.Fatalf("begin commit: ok=%v err=%v", ok, err)
	}
	encoded, err := base64.StdEncoding.DecodeString(tinyWebP)
	if err != nil {
		t.Fatal(err)
	}
	spoolDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(spoolDir, hash+"_"+job.Token+".webp"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	store := newMemoryStore()
	otherWebP := append(bytes.Clone(encoded), 0)
	store.objects["blobs/"+hash] = otherWebP
	store.types["blobs/"+hash] = "image/webp"
	svc := New(cat, store, spoolDir)
	if err := svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	current := waitForEncodingStatus(t, cat, hash, "pending")
	if current.Token != "" || !bytes.Equal(store.objects["blobs/"+hash], otherWebP) {
		t.Fatalf("mismatched WebP was counted as our output: %+v", current)
	}
	// If the local spool is lost entirely, the empty API stage key still
	// distinguishes this from a legacy copy and prevents a false conversion.
	claimed, err = cat.ClaimEncoding(ctx, 1, LeaseTTL)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("reclaim: jobs=%+v err=%v", claimed, err)
	}
	job = claimed[0]
	if ok, err := cat.MarkEncodingReceived(ctx, hash, job.Token); err != nil || !ok {
		t.Fatalf("mark received again: ok=%v err=%v", ok, err)
	}
	if ok, err := cat.BeginEncodingCommit(ctx, hash, job.Token, int64(len(source)), 0); err != nil || !ok {
		t.Fatalf("begin commit again: ok=%v err=%v", ok, err)
	}
	if err := svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	waitForEncodingStatus(t, cat, hash, "pending")
}

func TestPeriodicRecoveryDoesNotInterruptActiveCommit(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	hash := strings.Repeat("a", 64)
	if err := cat.UpsertBlob(ctx, catalog.Blob{Hash: hash, SizeBytes: 200, ContentType: "image/png"}); err != nil {
		t.Fatal(err)
	}
	claimed, err := cat.ClaimEncoding(ctx, 1, LeaseTTL)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: jobs=%+v err=%v", claimed, err)
	}
	job := claimed[0]
	if ok, err := cat.MarkEncodingReceived(ctx, hash, job.Token); err != nil || !ok {
		t.Fatalf("mark received: ok=%v err=%v", ok, err)
	}
	if ok, err := cat.BeginEncodingCommit(ctx, hash, job.Token, 200, 0); err != nil || !ok {
		t.Fatalf("begin commit: ok=%v err=%v", ok, err)
	}
	store := newMemoryStore()
	store.objects["blobs/"+hash] = []byte("unreplaced original")
	svc := New(cat, store, t.TempDir())
	svc.queued[jobKey(hash, job.Token)] = struct{}{}
	if err := svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if current, err := cat.GetEncodingJob(ctx, hash); err != nil || current.Status != "committing" {
		t.Fatalf("active commit was reset: job=%+v err=%v", current, err)
	}
	delete(svc.queued, jobKey(hash, job.Token))
	if err := svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	waitForEncodingStatus(t, cat, hash, "pending")
}

func TestRecoveredCommittingFilesCountTowardSpoolCapacity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	for _, char := range []string{"a", "b", "c"} {
		if err := cat.UpsertBlob(ctx, catalog.Blob{Hash: strings.Repeat(char, 64), SizeBytes: 200, ContentType: "image/png"}); err != nil {
			t.Fatal(err)
		}
	}
	jobs, err := cat.ClaimEncoding(ctx, 3, LeaseTTL)
	if err != nil || len(jobs) != 3 {
		t.Fatalf("claim: jobs=%+v err=%v", jobs, err)
	}
	spoolDir := t.TempDir()
	for _, job := range jobs[:2] {
		if ok, err := cat.MarkEncodingReceived(ctx, job.Hash, job.Token); err != nil || !ok {
			t.Fatalf("mark received: ok=%v err=%v", ok, err)
		}
		if ok, err := cat.BeginEncodingCommit(ctx, job.Hash, job.Token, 200, time.Hour); err != nil || !ok {
			t.Fatalf("begin commit: ok=%v err=%v", ok, err)
		}
		if err := os.WriteFile(filepath.Join(spoolDir, job.Hash+"_"+job.Token+".webp"), []byte("spooled"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	svc := New(cat, newMemoryStore(), spoolDir)
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if len(svc.spoolSlots) != outputProcessors {
		t.Fatalf("recovered committing files reserved %d of %d slots", len(svc.spoolSlots), outputProcessors)
	}
	if err := svc.ReceiveOutput(ctx, jobs[2].Hash, jobs[2].Token, strings.NewReader("new result")); !errors.Is(err, ErrSpoolBusy) {
		t.Fatalf("new upload with a full recovered spool: %v, want ErrSpoolBusy", err)
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
	svc := New(cat, store, t.TempDir())
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
	svc := New(cat, store, t.TempDir())
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
	svc := New(cat, store, t.TempDir())
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
	claimed, err := New(cat, store, t.TempDir()).Claim(ctx, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: err=%v claimed=%+v", err, claimed)
	}
	status, err := New(cat, store, t.TempDir()).Complete(ctx, hash, claimed[0].Token, "uploaded", "")
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
	if err := New(cat, store, t.TempDir()).Recover(ctx); err != nil {
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
