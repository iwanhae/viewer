package encoding

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"path/filepath"
	"testing"
	"time"

	"viewer/internal/catalog"
)

func TestCloseJoinsAcceptedOutputBeforeCatalogShutdown(t *testing.T) {
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	source := sourcePNG(t)
	sum := sha256.Sum256(source)
	hash := hex.EncodeToString(sum[:])
	if err := cat.UpsertBlob(context.Background(), catalog.Blob{Hash: hash, SizeBytes: int64(len(source)), ContentType: "image/png"}); err != nil {
		t.Fatal(err)
	}
	encoded, err := base64.StdEncoding.DecodeString(tinyWebP)
	if err != nil {
		t.Fatal(err)
	}
	store := newMemoryStore()
	store.objects["blobs/"+hash] = source
	store.types["blobs/"+hash] = "image/png"
	store.putStarted, store.putRelease, store.putFinished = make(chan struct{}), make(chan struct{}), make(chan struct{})
	service := New(cat, store, t.TempDir())
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	jobs, err := service.ClaimForAPI(context.Background(), 1)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("claim=%+v %v", jobs, err)
	}
	if err := service.ReceiveOutput(context.Background(), hash, jobs[0].Token, bytes.NewReader(encoded)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-store.putStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("output processor did not start")
	}
	stopping := make(chan struct{})
	service.startMu.Lock()
	stop := service.stop
	service.stop = func() { stop(); close(stopping) }
	service.startMu.Unlock()
	closed := make(chan struct{})
	go func() { service.Close(); close(closed) }()
	select {
	case <-stopping:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not stop processors")
	}
	select {
	case <-closed:
		t.Fatal("Close returned during accepted-output commit")
	default:
	}
	close(store.putRelease)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not join processors")
	}
	select {
	case <-store.putFinished:
	default:
		t.Fatal("catalog could close before output write returned")
	}
}
