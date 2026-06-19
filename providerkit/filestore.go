package providerkit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// FileStore is a simple, dependency-free, durable Store: it persists the
// whole Snapshot as JSON and writes it atomically (write to a temp file in
// the same directory, fsync, rename over the target). A crash can never
// leave a torn file — Load sees either the prior snapshot or the new one.
//
// It is the YAGNI-simplest choice that satisfies the durability contract.
// The full-snapshot write is O(N) per mutation, which is fine up to the
// conformance threshold (~10k machines/shard). Providers beyond that should
// implement a delta-oriented Store over an embedded KV store; the [Store]
// interface is small enough to swap.
type FileStore struct {
	path string
	mu   sync.Mutex
}

// NewFileStore returns a Store backed by the JSON file at path, creating its
// parent directory if needed. The file itself is created on first Save.
func NewFileStore(path string) (*FileStore, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("providerkit: create store dir: %w", err)
	}
	return &FileStore{path: path}, nil
}

func (f *FileStore) Load() (Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	data, err := os.ReadFile(f.path)
	if errors.Is(err, fs.ErrNotExist) {
		return Snapshot{}, nil // first boot
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("providerkit: read store %s: %w", f.path, err)
	}
	if len(data) == 0 {
		return Snapshot{}, nil
	}
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return Snapshot{}, fmt.Errorf("providerkit: decode store %s: %w", f.path, err)
	}
	return snap, nil
}

func (f *FileStore) Save(s Snapshot) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("providerkit: encode store: %w", err)
	}

	// Atomic replace: temp file in the same dir, fsync, rename.
	tmp, err := os.CreateTemp(filepath.Dir(f.path), ".store-*.tmp")
	if err != nil {
		return fmt.Errorf("providerkit: create temp store: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once the rename succeeds

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("providerkit: write temp store: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("providerkit: fsync temp store: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("providerkit: close temp store: %w", err)
	}
	if err := os.Rename(tmpName, f.path); err != nil {
		return fmt.Errorf("providerkit: rename store into place: %w", err)
	}
	// fsync the parent directory so the rename (the new directory entry) is
	// itself durable across a hard crash — otherwise the very first write of a
	// brand-new store file can be lost. Best-effort: a missing dir-sync never
	// corrupts an existing snapshot.
	if dir, err := os.Open(filepath.Dir(f.path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func (f *FileStore) Close() error { return nil }

// Compile-time check.
var _ Store = (*FileStore)(nil)
