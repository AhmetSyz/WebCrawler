package search

import (
	"fmt"
	"google-in-a-day/internal/indexer"
	"sort"
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
	if index == nil || docStore == nil || query == "" {
		return nil
	}

	fmt.Printf("[Searcher DEBUG] Index Address: %p | Query: %s\n", index, query)

	tokens := indexer.Tokenize(query)
	if len(tokens) == 0 {
		return nil
	}

	fmt.Printf("[Searcher] Query tokens: %v\n", tokens)

	// Calculate document scores across all query tokens.
	docScores := make(map[uint64]uint32)

	for _, token := range tokens {
		postings := index.GetPostings(token)

		fmt.Printf("🔎 [Searcher] Token: '%s' | Aranan Shard: (GetPostings içinde) | Bulunan Kayıt: %d\n", token, len(postings))

		if len(postings) > 0 {
			fmt.Printf("[Searcher] Found %d matches for token '%s'\n", len(postings), token)
		} else {
			fmt.Printf("[Searcher] No matches found for token '%s'\n", token)
		}
		for docID, count := range postings {
			docScores[docID] += count
		}
	}

	if len(docScores) == 0 {
		return nil
	}

	var results []SearchResult
	for docID, score := range docScores {
		meta, ok := docStore.Get(docID)
		if !ok {
			continue
		}

		results = append(results, SearchResult{
			URL:   meta.DocURL,
			Score: score,
			Title: meta.DocURL, // Using URL as a placeholder title for now
		})
	}

	// Sort results by score in descending order
	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	return results
}
