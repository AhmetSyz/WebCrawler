package search

import (
	"bufio"
	"fmt"
	"google-in-a-day/internal/indexer"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SearchResult represents a single document matching the search query.
type SearchResult struct {
	URL   string
	Score uint32
	Title string // Placeholder, as we don't index titles currently
}

// Search takes a query string, tokenizes it, and queries the given index.
// It returns a slice of SearchResult sorted by Score descending.
func Search(query string, index *indexer.InvertedIndex, docStore *indexer.DocumentStore) []SearchResult {
	if query == "" {
		return nil
	}

	tokens := indexer.Tokenize(query)
	if len(tokens) == 0 {
		return nil
	}

	docScores := make(map[string]uint32) // URL -> score

	// Prefer in-memory index if available.
	if index != nil {
		for _, token := range tokens {
			postings := index.GetPostings(token)
			for docID, count := range postings {
				if docStore == nil {
					continue
				}
				meta, ok := docStore.Get(docID)
				if !ok {
					fmt.Printf("[Searcher] Metadata not found for docID: %d\n", docID)
					continue
				}
				docScores[meta.DocURL] += count
			}
		}
	} else {
		// Disk-based search: read only the letter-partitioned file for each token.
		for _, token := range tokens {
			letter := diskFirstLetter(token)
			fileName := filepath.Join("storage", fmt.Sprintf("%s.data", letter))
			f, err := os.Open(fileName)
			if err != nil {
				continue
			}
			scanner := bufio.NewScanner(f)
			for scanner.Scan() {
				line := scanner.Text()
				// token,url,origin_url,depth,count
				parts := strings.SplitN(line, ",", 5)
				if len(parts) != 5 {
					continue
				}
				if unescapeCSV(parts[0]) != token {
					continue
				}
				url := unescapeCSV(parts[1])
				count := parseUint32(parts[4])
				docScores[url] += count
			}
			_ = f.Close()
		}
	}

	if len(docScores) == 0 {
		return nil
	}

	results := make([]SearchResult, 0, len(docScores))
	for url, score := range docScores {
		results = append(results, SearchResult{URL: url, Score: score, Title: url})
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})
	return results
}

func diskFirstLetter(token string) string {
	if token == "" {
		return "_"
	}
	r := token[0]
	if r >= 'A' && r <= 'Z' {
		r = r - 'A' + 'a'
	}
	if r >= 'a' && r <= 'z' {
		return string(r)
	}
	return "_"
}

func unescapeCSV(s string) string {
	s = strings.ReplaceAll(s, "%2C", ",")
	return s
}

func parseUint32(s string) uint32 {
	var v uint32
	_, _ = fmt.Sscanf(strings.TrimSpace(s), "%d", &v)
	return v
}
