package indexer

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Store defines the behavior for tracking visited URLs and persisting state.
type Store interface {
	// AddVisited records the given URL as visited.
	// It returns true if the URL was not previously recorded, false otherwise.
	AddVisited(url string) bool

	// IsVisited reports whether the given URL has already been visited.
	IsVisited(url string) bool

	// VisitedSnapshot returns a point-in-time list of visited URLs.
	// This is used for persistence/export (e.g. `visited_urls.data`).
	VisitedSnapshot() []string

	// SaveState persists the current state of the store.
	// For in-memory implementations this can be a no-op.
	SaveState() error
}

// InMemoryStore is a simple in-memory implementation of Store.
type InMemoryStore struct {
	mu      sync.RWMutex        // RWMutex is a read-write mutex that allows multiple readers or a single writer
	visited map[string]struct{} // struct{} is a empty struct that is used to store a key-value pair in the map
}

// NewInMemoryStore creates a new in-memory store instance.
func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{
		visited: make(map[string]struct{}),
	}
}

// AddVisited records the URL as visited if it was not seen before.
// It returns true if the URL was newly added, or false if it was already present.
func (s *InMemoryStore) AddVisited(url string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.visited[url]; exists {
		return false
	}

	s.visited[url] = struct{}{}
	return true
}

// IsVisited checks if the URL has already been recorded as visited.
func (s *InMemoryStore) IsVisited(url string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	_, exists := s.visited[url] // := used for defining and assigning a value to a variable at the same time
	return exists
}

// VisitedSnapshot returns all visited URLs as a slice.
func (s *InMemoryStore) VisitedSnapshot() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]string, 0, len(s.visited))
	for u := range s.visited {
		out = append(out, u)
	}

	// Deterministic output is useful for evaluation/debugging.
	sort.Strings(out)
	return out
}

// SaveState is a no-op for the in-memory implementation, but satisfies the Store interface.
func (s *InMemoryStore) SaveState() error {
	return nil
}

// WriteVisitedURLsData exports the visited URLs to `visited_urls.data` under outDir.
// File format: one URL per line.
func WriteVisitedURLsData(store Store, outDir string) (string, error) {
	if outDir == "" {
		outDir = "."
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", err
	}

	path := filepath.Join(outDir, "visited_urls.data")
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	w := bufio.NewWriterSize(f, 1<<20)
	for _, u := range store.VisitedSnapshot() {
		if _, err := w.WriteString(u + "\n"); err != nil {
			return "", err
		}
	}
	if err := w.Flush(); err != nil {
		return "", err
	}
	return path, nil
}
