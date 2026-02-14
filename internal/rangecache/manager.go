package rangecache

import (
	"container/list"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
)

const (
	defaultChunkSize = int64(1 << 20) // 1 MiB
	defaultMaxBytes  = int64(8 << 30) // 8 GiB
)

type FetchRangeFunc func(ctx context.Context, key string, start int64, end int64) (io.ReadCloser, error)

type Config struct {
	ChunkSize int64
	MaxBytes  int64
	Fetch     FetchRangeFunc
}

type Manager struct {
	dir       string
	chunkSize int64
	maxBytes  int64
	fetch     FetchRangeFunc

	entries map[string]*entry
	lru     *list.List

	mu          sync.Mutex
	loadedBytes atomic.Int64
	fetchReqs   atomic.Int64
	fetchBytes  atomic.Int64
	cacheHits   atomic.Int64
	cacheMisses atomic.Int64
	readErrors  atomic.Int64
}

type Stats struct {
	FetchRequests int64 `json:"fetchRequests"`
	FetchBytes    int64 `json:"fetchBytes"`
	CacheHits     int64 `json:"cacheHits"`
	CacheMisses   int64 `json:"cacheMisses"`
	ReadErrors    int64 `json:"readErrors"`
	LoadedBytes   int64 `json:"loadedBytes"`
}

type Handle struct {
	ctx     context.Context
	manager *Manager
	entry   *entry
	once    sync.Once
	closed  atomic.Bool
}

type entry struct {
	key       string
	path      string
	size      int64
	chunkSize int64

	file *os.File
	data []byte

	loaded     []bool
	loading    map[int]*chunkLoad
	loadedSize int64

	refs    int
	closed  bool
	lruElem *list.Element

	mu sync.Mutex
}

type chunkLoad struct {
	done chan struct{}
	err  error
}

func NewManager(cacheDir string, cfg Config) (*Manager, error) {
	if cfg.Fetch == nil {
		return nil, fmt.Errorf("fetch func is required")
	}
	if cfg.ChunkSize <= 0 {
		cfg.ChunkSize = defaultChunkSize
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = defaultMaxBytes
	}

	// Runtime-only cache state: always start from a clean directory.
	if err := os.RemoveAll(cacheDir); err != nil {
		return nil, fmt.Errorf("clear range cache dir: %w", err)
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("create range cache dir: %w", err)
	}

	return &Manager{
		dir:       cacheDir,
		chunkSize: cfg.ChunkSize,
		maxBytes:  cfg.MaxBytes,
		fetch:     cfg.Fetch,
		entries:   make(map[string]*entry),
		lru:       list.New(),
	}, nil
}

func (m *Manager) Open(ctx context.Context, key string, size int64) (*Handle, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if key == "" {
		return nil, fmt.Errorf("key is required")
	}
	if size <= 0 {
		return nil, fmt.Errorf("invalid object size: %d", size)
	}

	m.mu.Lock()
	if e, ok := m.entries[key]; ok {
		if e.size == size && !e.closed {
			e.refs++
			m.touchLocked(e)
			m.mu.Unlock()
			return &Handle{ctx: ctx, manager: m, entry: e}, nil
		}
		if e.refs > 0 {
			m.mu.Unlock()
			return nil, fmt.Errorf("object %s changed size while in use", key)
		}
		if _, err := m.destroyEntryLocked(e); err != nil {
			m.mu.Unlock()
			return nil, err
		}
	}
	m.mu.Unlock()

	created, err := m.createEntry(key, size)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if existing, ok := m.entries[key]; ok {
		if existing.size == size && !existing.closed {
			existing.refs++
			m.touchLocked(existing)
			_, _ = created.destroy()
			return &Handle{ctx: ctx, manager: m, entry: existing}, nil
		}
		if existing.refs > 0 {
			_, _ = created.destroy()
			return nil, fmt.Errorf("object %s changed size while in use", key)
		}
		if _, err := m.destroyEntryLocked(existing); err != nil {
			_, _ = created.destroy()
			return nil, err
		}
	}

	created.refs = 1
	created.lruElem = m.lru.PushFront(created)
	m.entries[key] = created
	return &Handle{ctx: ctx, manager: m, entry: created}, nil
}

func (h *Handle) ReadAt(p []byte, off int64) (int, error) {
	if h == nil || h.manager == nil || h.entry == nil {
		return 0, fmt.Errorf("invalid range cache handle")
	}
	if h.closed.Load() {
		return 0, fmt.Errorf("range cache handle is closed")
	}
	return h.manager.readAt(h.ctx, h.entry, p, off)
}

func (h *Handle) Close() error {
	if h == nil || h.manager == nil || h.entry == nil {
		return nil
	}
	h.once.Do(func() {
		h.closed.Store(true)
		h.manager.release(h.entry)
	})
	return nil
}

func (m *Manager) release(e *entry) {
	m.mu.Lock()
	if e.refs > 0 {
		e.refs--
	}
	if !e.closed {
		m.touchLocked(e)
	}
	m.mu.Unlock()

	if m.loadedBytes.Load() > m.maxBytes {
		m.evictIfNeeded()
	}
}

func (m *Manager) readAt(ctx context.Context, e *entry, p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("negative offset: %d", off)
	}
	if len(p) == 0 {
		return 0, nil
	}
	if off >= e.size {
		return 0, io.EOF
	}

	maxRead := int64(len(p))
	if off+maxRead > e.size {
		maxRead = e.size - off
	}

	startChunk := off / e.chunkSize
	endChunk := (off + maxRead - 1) / e.chunkSize
	for idx := startChunk; idx <= endChunk; idx++ {
		if err := m.ensureChunk(ctx, e, int(idx)); err != nil {
			m.readErrors.Add(1)
			return 0, err
		}
	}

	start := int(off)
	end := int(off + maxRead)
	copy(p[:int(maxRead)], e.data[start:end])

	m.mu.Lock()
	if !e.closed {
		m.touchLocked(e)
	}
	m.mu.Unlock()

	if m.loadedBytes.Load() > m.maxBytes {
		m.evictIfNeeded()
	}

	n := int(maxRead)
	if maxRead < int64(len(p)) {
		return n, io.EOF
	}
	return n, nil
}

func (m *Manager) ensureChunk(ctx context.Context, e *entry, idx int) error {
	for {
		e.mu.Lock()
		if e.closed {
			e.mu.Unlock()
			return fmt.Errorf("cache entry closed for key %s", e.key)
		}
		if idx < 0 || idx >= len(e.loaded) {
			e.mu.Unlock()
			return fmt.Errorf("chunk index out of range: %d", idx)
		}
		if e.loaded[idx] {
			m.cacheHits.Add(1)
			e.mu.Unlock()
			return nil
		}
		if st, ok := e.loading[idx]; ok {
			done := st.done
			e.mu.Unlock()
			<-done
			if st.err != nil {
				return st.err
			}
			continue
		}

		st := &chunkLoad{done: make(chan struct{})}
		e.loading[idx] = st
		m.cacheMisses.Add(1)
		e.mu.Unlock()

		err := m.fetchChunk(ctx, e, idx)

		e.mu.Lock()
		if err == nil && !e.loaded[idx] {
			e.loaded[idx] = true
			delta := e.chunkLen(idx)
			e.loadedSize += delta
			m.loadedBytes.Add(delta)
		}
		st.err = err
		delete(e.loading, idx)
		close(st.done)
		e.mu.Unlock()

		return err
	}
}

func (m *Manager) fetchChunk(ctx context.Context, e *entry, idx int) error {
	if ctx == nil {
		ctx = context.Background()
	}

	start, end := e.chunkRange(idx)
	expected := end - start + 1
	m.fetchReqs.Add(1)
	m.fetchBytes.Add(expected)
	body, err := m.fetch(ctx, e.key, start, end)
	if err != nil {
		return fmt.Errorf("fetch chunk key=%s range=%d-%d: %w", e.key, start, end, err)
	}
	defer body.Close()

	dst := e.data[int(start):int(end+1)]
	n, err := io.ReadFull(body, dst)
	if err != nil {
		return fmt.Errorf("read chunk body key=%s range=%d-%d: %w", e.key, start, end, err)
	}
	if int64(n) != expected {
		return fmt.Errorf("read chunk body key=%s range=%d-%d short read: got=%d want=%d", e.key, start, end, n, expected)
	}

	var extra [1]byte
	extraN, extraErr := body.Read(extra[:])
	if extraN > 0 {
		return fmt.Errorf("read chunk body key=%s range=%d-%d too long", e.key, start, end)
	}
	if extraErr != nil && !errors.Is(extraErr, io.EOF) {
		return fmt.Errorf("read chunk body key=%s range=%d-%d trailing read failed: %w", e.key, start, end, extraErr)
	}

	return nil
}

func (m *Manager) Stats() Stats {
	if m == nil {
		return Stats{}
	}
	return Stats{
		FetchRequests: m.fetchReqs.Load(),
		FetchBytes:    m.fetchBytes.Load(),
		CacheHits:     m.cacheHits.Load(),
		CacheMisses:   m.cacheMisses.Load(),
		ReadErrors:    m.readErrors.Load(),
		LoadedBytes:   m.loadedBytes.Load(),
	}
}

func (m *Manager) evictIfNeeded() {
	if m.loadedBytes.Load() <= m.maxBytes {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	for m.loadedBytes.Load() > m.maxBytes {
		var candidate *entry
		for node := m.lru.Back(); node != nil; node = node.Prev() {
			e := node.Value.(*entry)
			if e.refs == 0 {
				candidate = e
				break
			}
		}
		if candidate == nil {
			return
		}

		reclaimed, err := m.destroyEntryLocked(candidate)
		if err != nil {
			log.Printf("rangecache: evict key=%s failed: %v", candidate.key, err)
			return
		}
		log.Printf("rangecache: evicted key=%s reclaimed_bytes=%d", candidate.key, reclaimed)
	}
}

func (m *Manager) destroyEntryLocked(e *entry) (int64, error) {
	if cur, ok := m.entries[e.key]; ok && cur == e {
		delete(m.entries, e.key)
	}
	if e.lruElem != nil {
		m.lru.Remove(e.lruElem)
		e.lruElem = nil
	}

	reclaimed, err := e.destroy()
	if reclaimed > 0 {
		m.loadedBytes.Add(-reclaimed)
	}
	return reclaimed, err
}

func (m *Manager) touchLocked(e *entry) {
	if e.lruElem == nil {
		e.lruElem = m.lru.PushFront(e)
		return
	}
	m.lru.MoveToFront(e.lruElem)
}

func (m *Manager) createEntry(key string, size int64) (*entry, error) {
	if size > int64(math.MaxInt) {
		return nil, fmt.Errorf("object %s too large to map: %d", key, size)
	}

	path := filepath.Join(m.dir, fileNameForKey(key)+".bin")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open cache file for %s: %w", key, err)
	}
	if err := f.Truncate(size); err != nil {
		f.Close()
		return nil, fmt.Errorf("truncate cache file for %s: %w", key, err)
	}

	data, err := syscall.Mmap(int(f.Fd()), 0, int(size), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("mmap cache file for %s: %w", key, err)
	}

	chunkCount := int((size + m.chunkSize - 1) / m.chunkSize)
	return &entry{
		key:       key,
		path:      path,
		size:      size,
		chunkSize: m.chunkSize,
		file:      f,
		data:      data,
		loaded:    make([]bool, chunkCount),
		loading:   make(map[int]*chunkLoad),
	}, nil
}

func (e *entry) destroy() (int64, error) {
	e.mu.Lock()
	if e.closed {
		reclaimed := e.loadedSize
		e.loadedSize = 0
		e.mu.Unlock()
		return reclaimed, nil
	}

	wait := make([]chan struct{}, 0, len(e.loading))
	for _, st := range e.loading {
		wait = append(wait, st.done)
	}
	e.mu.Unlock()
	for _, done := range wait {
		<-done
	}

	e.mu.Lock()
	if e.closed {
		reclaimed := e.loadedSize
		e.loadedSize = 0
		e.mu.Unlock()
		return reclaimed, nil
	}
	e.closed = true
	data := e.data
	e.data = nil
	file := e.file
	e.file = nil
	path := e.path
	reclaimed := e.loadedSize
	e.loadedSize = 0
	e.mu.Unlock()

	var firstErr error
	if data != nil {
		if err := syscall.Munmap(data); err != nil {
			firstErr = fmt.Errorf("munmap %s: %w", path, err)
		}
	}
	if file != nil {
		if err := file.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("close %s: %w", path, err)
		}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) && firstErr == nil {
		firstErr = fmt.Errorf("remove %s: %w", path, err)
	}
	return reclaimed, firstErr
}

func (e *entry) chunkRange(idx int) (int64, int64) {
	start := int64(idx) * e.chunkSize
	end := start + e.chunkSize - 1
	if end >= e.size {
		end = e.size - 1
	}
	return start, end
}

func (e *entry) chunkLen(idx int) int64 {
	start, end := e.chunkRange(idx)
	return end - start + 1
}

func fileNameForKey(key string) string {
	sum := sha1.Sum([]byte(key))
	return hex.EncodeToString(sum[:])
}
