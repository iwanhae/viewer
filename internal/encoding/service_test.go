package encoding

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"io"
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
	objects map[string][]byte
	types   map[string]string
}

func newMemoryStore() *memoryStore { return &memoryStore{map[string][]byte{}, map[string]string{}} }
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
	m.objects[to] = bytes.Clone(m.objects[from])
	m.types[to] = ct
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
	if ok, err := cat.BeginEncodingCommit(ctx, "large", jobs[0].Token); err != nil || ok {
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
	if ok, err := cat.BeginEncodingCommit(ctx, hash, jobs[0].Token); err != nil || !ok {
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
}
