package landing

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// JSONStore persists batches in one JSON file for command-line operation.
type JSONStore struct {
	Path string
	mu   sync.Mutex
}

// Save writes the complete batch set atomically.
func (s *JSONStore) Save(_ context.Context, batch Batch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	batches, err := s.read()
	if err != nil {
		return err
	}
	batches[batch.ID] = cloneBatch(batch)
	return s.write(batches)
}

// Load reads one stored batch.
func (s *JSONStore) Load(_ context.Context, id string) (Batch, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	batches, err := s.read()
	if err != nil {
		return Batch{}, false, err
	}
	batch, found := batches[id]
	return cloneBatch(batch), found, nil
}

// ListActive reads every non-terminal stored batch.
func (s *JSONStore) ListActive(_ context.Context) ([]Batch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	batches, err := s.read()
	if err != nil {
		return nil, err
	}
	out := make([]Batch, 0, len(batches))
	for _, batch := range batches {
		if batch.State != Failed && batch.State != Landed {
			out = append(out, cloneBatch(batch))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// List returns every stored batch, including terminal batches.
func (s *JSONStore) List(_ context.Context) ([]Batch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	batches, err := s.read()
	if err != nil {
		return nil, err
	}
	out := make([]Batch, 0, len(batches))
	for _, batch := range batches {
		out = append(out, cloneBatch(batch))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *JSONStore) read() (map[string]Batch, error) {
	if s.Path == "" {
		return nil, fmt.Errorf("batch store path is empty")
	}
	body, err := os.ReadFile(s.Path)
	if os.IsNotExist(err) {
		return make(map[string]Batch), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read batch store: %w", err)
	}
	batches := make(map[string]Batch)
	if err := json.Unmarshal(body, &batches); err != nil {
		return nil, fmt.Errorf("decode batch store: %w", err)
	}
	return batches, nil
}

func (s *JSONStore) write(batches map[string]Batch) error {
	body, err := json.MarshalIndent(batches, "", "  ")
	if err != nil {
		return fmt.Errorf("encode batch store: %w", err)
	}
	directory := filepath.Dir(s.Path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("create batch store directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".batch-*.json")
	if err != nil {
		return fmt.Errorf("create batch store temporary file: %w", err)
	}
	name := temporary.Name()
	if _, err := temporary.Write(body); err != nil {
		temporary.Close()
		os.Remove(name)
		return fmt.Errorf("write batch store: %w", err)
	}
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		os.Remove(name)
		return fmt.Errorf("protect batch store: %w", err)
	}
	if err := temporary.Close(); err != nil {
		os.Remove(name)
		return fmt.Errorf("close batch store: %w", err)
	}
	if err := os.Rename(name, s.Path); err != nil {
		os.Remove(name)
		return fmt.Errorf("replace batch store: %w", err)
	}
	return nil
}
