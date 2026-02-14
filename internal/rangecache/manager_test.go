package rangecache

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"testing"
)

func TestManagerReadAtUsesChunkCache(t *testing.T) {
	t.Parallel()

	backing := []byte("0123456789abcdef")
	var mu sync.Mutex
	calls := make(map[string]int)

	m, err := NewManager(
		filepath.Join(t.TempDir(), "range"),
		Config{
			ChunkSize: 4,
			MaxBytes:  64,
			Fetch: func(ctx context.Context, key string, start int64, end int64) (io.ReadCloser, error) {
				mu.Lock()
				calls[key+":"+rangeKey(start, end)]++
				mu.Unlock()
				return io.NopCloser(bytes.NewReader(backing[start : end+1])), nil
			},
		},
	)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	h, err := m.Open(context.Background(), "albums/a/source.zip", int64(len(backing)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer h.Close()

	buf := make([]byte, 5)
	n, err := h.ReadAt(buf, 2)
	if err != nil {
		t.Fatalf("ReadAt first: %v", err)
	}
	if n != 5 || string(buf) != "23456" {
		t.Fatalf("unexpected first read n=%d data=%q", n, string(buf))
	}

	n, err = h.ReadAt(buf[:2], 3)
	if err != nil {
		t.Fatalf("ReadAt second: %v", err)
	}
	if n != 2 || string(buf[:2]) != "34" {
		t.Fatalf("unexpected second read n=%d data=%q", n, string(buf[:2]))
	}

	mu.Lock()
	defer mu.Unlock()
	if got := calls["albums/a/source.zip:"+rangeKey(0, 3)]; got != 1 {
		t.Fatalf("chunk [0-3] fetch count=%d want=1", got)
	}
	if got := calls["albums/a/source.zip:"+rangeKey(4, 7)]; got != 1 {
		t.Fatalf("chunk [4-7] fetch count=%d want=1", got)
	}
}

func TestManagerEvictsLeastRecentlyUsedEntry(t *testing.T) {
	t.Parallel()

	backing := []byte("0123456789abcdef")
	var mu sync.Mutex
	calls := make(map[string]int)

	m, err := NewManager(
		filepath.Join(t.TempDir(), "range"),
		Config{
			ChunkSize: 4,
			MaxBytes:  4, // allow one loaded chunk total
			Fetch: func(ctx context.Context, key string, start int64, end int64) (io.ReadCloser, error) {
				mu.Lock()
				calls[key+":"+rangeKey(start, end)]++
				mu.Unlock()
				return io.NopCloser(bytes.NewReader(backing[start : end+1])), nil
			},
		},
	)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	h1, err := m.Open(context.Background(), "albums/a/source.zip", int64(len(backing)))
	if err != nil {
		t.Fatalf("Open h1: %v", err)
	}
	one := make([]byte, 1)
	if _, err := h1.ReadAt(one, 0); err != nil {
		t.Fatalf("ReadAt h1: %v", err)
	}
	_ = h1.Close()

	h2, err := m.Open(context.Background(), "albums/b/source.zip", int64(len(backing)))
	if err != nil {
		t.Fatalf("Open h2: %v", err)
	}
	if _, err := h2.ReadAt(one, 0); err != nil {
		t.Fatalf("ReadAt h2: %v", err)
	}
	_ = h2.Close()

	h1Again, err := m.Open(context.Background(), "albums/a/source.zip", int64(len(backing)))
	if err != nil {
		t.Fatalf("Open h1 again: %v", err)
	}
	if _, err := h1Again.ReadAt(one, 0); err != nil {
		t.Fatalf("ReadAt h1 again: %v", err)
	}
	_ = h1Again.Close()

	mu.Lock()
	defer mu.Unlock()
	if got := calls["albums/a/source.zip:"+rangeKey(0, 3)]; got != 2 {
		t.Fatalf("chunk [0-3] for album a fetch count=%d want=2", got)
	}
}

func TestManagerFailsWhenRangeBodyTooLong(t *testing.T) {
	t.Parallel()

	backing := []byte("0123456789abcdef")
	m, err := NewManager(
		filepath.Join(t.TempDir(), "range"),
		Config{
			ChunkSize: 4,
			MaxBytes:  64,
			Fetch: func(ctx context.Context, key string, start int64, end int64) (io.ReadCloser, error) {
				chunk := append([]byte(nil), backing[start:end+1]...)
				chunk = append(chunk, 'x')
				return io.NopCloser(bytes.NewReader(chunk)), nil
			},
		},
	)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	h, err := m.Open(context.Background(), "albums/a/source.zip", int64(len(backing)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer h.Close()

	buf := make([]byte, 2)
	if _, err := h.ReadAt(buf, 0); err == nil {
		t.Fatalf("expected ReadAt to fail for oversized range body")
	}

	stats := m.Stats()
	if got, want := stats.ReadErrors, int64(1); got != want {
		t.Fatalf("ReadErrors=%d want=%d", got, want)
	}
}

func TestManagerStatsCounters(t *testing.T) {
	t.Parallel()

	backing := []byte("0123456789abcdef")
	m, err := NewManager(
		filepath.Join(t.TempDir(), "range"),
		Config{
			ChunkSize: 4,
			MaxBytes:  64,
			Fetch: func(ctx context.Context, key string, start int64, end int64) (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(backing[start : end+1])), nil
			},
		},
	)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	h, err := m.Open(context.Background(), "albums/a/source.zip", int64(len(backing)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer h.Close()

	buf := make([]byte, 2)
	if _, err := h.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt first: %v", err)
	}
	if _, err := h.ReadAt(buf[:1], 1); err != nil {
		t.Fatalf("ReadAt second: %v", err)
	}

	stats := m.Stats()
	if got, want := stats.FetchRequests, int64(1); got != want {
		t.Fatalf("FetchRequests=%d want=%d", got, want)
	}
	if got, want := stats.FetchBytes, int64(4); got != want {
		t.Fatalf("FetchBytes=%d want=%d", got, want)
	}
	if got, want := stats.CacheMisses, int64(1); got != want {
		t.Fatalf("CacheMisses=%d want=%d", got, want)
	}
	if got := stats.CacheHits; got < 1 {
		t.Fatalf("CacheHits=%d want>=1", got)
	}
	if got, want := stats.ReadErrors, int64(0); got != want {
		t.Fatalf("ReadErrors=%d want=%d", got, want)
	}
}

func rangeKey(start int64, end int64) string {
	return fmt.Sprintf("%d-%d", start, end)
}
