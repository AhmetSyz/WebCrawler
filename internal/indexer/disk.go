package indexer

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Keep all persisted artifacts under ./data
const defaultStorageDir = "data/storage"

// FlushToDisk writes the in-memory inverted index to letter-partitioned .data files.
//
// Format per line:
// token,url,origin_url,depth,count
//
// Files are overwritten per call (so the on-disk index represents the latest snapshot).
func FlushToDisk(index *InvertedIndex, docStore *DocumentStore) error {
	if index == nil || docStore == nil {
		return fmt.Errorf("index or docStore is nil")
	}

	if err := os.MkdirAll(defaultStorageDir, 0o755); err != nil {
		return fmt.Errorf("mkdir storage: %w", err)
	}

	// Collect token -> docID -> count in a stable order.
	tokenPostings := make(map[string]map[uint64]uint32)

	for si := range index.shards {
		sh := &index.shards[si]
		sh.mu.RLock()
		for token, postings := range sh.postings {
			// Copy postings (avoid holding shard lock during disk I/O)
			cp := make(map[uint64]uint32, len(postings))
			for docID, c := range postings {
				cp[docID] = c
			}
			tokenPostings[token] = cp
		}
		sh.mu.RUnlock()
	}

	// Group lines per first letter file.
	linesByFile := make(map[string][]string, 26)

	for token, postings := range tokenPostings {
		letter := firstLetter(token)
		fileName := filepath.Join(defaultStorageDir, fmt.Sprintf("%s.data", letter))

		for docID, count := range postings {
			meta, ok := docStore.Get(docID)
			if !ok {
				continue
			}
			line := fmt.Sprintf("%s,%s,%s,%d,%d", escapeCSV(token), escapeCSV(meta.DocURL), escapeCSV(meta.OriginURL), meta.Depth, count)
			linesByFile[fileName] = append(linesByFile[fileName], line)
		}
	}

	// Overwrite files (truncate) for a consistent snapshot.
	for fileName, lines := range linesByFile {
		sort.Strings(lines)
		f, err := os.Create(fileName)
		if err != nil {
			return fmt.Errorf("create %s: %w", fileName, err)
		}
		bw := bufio.NewWriterSize(f, 1<<20)
		for _, ln := range lines {
			_, _ = bw.WriteString(ln)
			_ = bw.WriteByte('\n')
		}
		if err := bw.Flush(); err != nil {
			_ = f.Close()
			return fmt.Errorf("flush %s: %w", fileName, err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("close %s: %w", fileName, err)
		}
	}

	return nil
}

// FlushAndClearToDisk writes the current in-memory index to disk and deletes
// the flushed token postings from RAM immediately.
//
// This is intended to be called periodically during crawling to keep RAM bounded.
func FlushAndClearToDisk(index *InvertedIndex, docStore *DocumentStore) error {
	if index == nil || docStore == nil {
		return fmt.Errorf("index or docStore is nil")
	}
	if err := os.MkdirAll(defaultStorageDir, 0o755); err != nil {
		return fmt.Errorf("mkdir storage: %w", err)
	}

	// Open writers per file once.
	writers := make(map[string]*bufio.Writer)
	files := make(map[string]*os.File)
	defer func() {
		for _, w := range writers {
			_ = w.Flush()
		}
		for _, f := range files {
			_ = f.Close()
		}
	}()

	getWriter := func(letter string) (*bufio.Writer, error) {
		fileName := filepath.Join(defaultStorageDir, fmt.Sprintf("%s.data", letter))
		if w, ok := writers[fileName]; ok {
			return w, nil
		}
		f, err := os.OpenFile(fileName, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, err
		}
		files[fileName] = f
		w := bufio.NewWriterSize(f, 1<<20)
		writers[fileName] = w
		return w, nil
	}

	for si := range index.shards {
		sh := &index.shards[si]

		// Lock shard exclusively so we can delete tokens after flushing.
		sh.mu.Lock()
		for token, postings := range sh.postings {
			letter := firstLetter(token)
			w, err := getWriter(letter)
			if err != nil {
				sh.mu.Unlock()
				return fmt.Errorf("writer for %s: %w", letter, err)
			}

			for docID, count := range postings {
				meta, ok := docStore.Get(docID)
				if !ok {
					continue
				}
				line := fmt.Sprintf("%s,%s,%s,%d,%d\n", escapeCSV(token), escapeCSV(meta.DocURL), escapeCSV(meta.OriginURL), meta.Depth, count)
				_, _ = w.WriteString(line)
			}

			// Delete token from RAM immediately after flushing.
			delete(sh.postings, token)
		}
		sh.mu.Unlock()
	}

	return nil
}

func firstLetter(token string) string {
	if token == "" {
		return "_"
	}
	r := token[0]
	// Normalize to a-z buckets; everything else goes to '_'.
	if r >= 'A' && r <= 'Z' {
		r = r - 'A' + 'a'
	}
	if r >= 'a' && r <= 'z' {
		return string(r)
	}
	return "_"
}

// escapeCSV is a minimal escape for commas/newlines by replacing them.
// This keeps the format simple for the assignment.
func escapeCSV(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, ",", "%2C")
	return s
}
