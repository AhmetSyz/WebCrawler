package indexer

import (
	"fmt"
	"hash/fnv"
	"strings"
	"sync"
)

// IndexShard holds token -> docID -> count for a subset of tokens.
// The shard is protected by a RWMutex to allow concurrent readers later.
type IndexShard struct {
	mu       sync.RWMutex
	postings map[string]map[uint64]uint32
}

// InvertedIndex is a sharded inverted index to reduce lock contention.
type InvertedIndex struct {
	shards []IndexShard
}

// NewInvertedIndex creates an inverted index with the given number of shards.
// If shardCount <= 0, a default of 32 is used.
func NewInvertedIndex(shardCount int) *InvertedIndex {
	if shardCount <= 0 {
		shardCount = 32
	}
	shards := make([]IndexShard, shardCount)
	for i := range shards {
		shards[i] = IndexShard{
			postings: make(map[string]map[uint64]uint32),
		}
	}
	return &InvertedIndex{shards: shards}
}

func (idx *InvertedIndex) shardForToken(token string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(token))
	return int(h.Sum32() % uint32(len(idx.shards)))
}

// AddDocument indexes the given document tokens, updating token counts for docID.
// It is concurrency-safe: updates are protected per-shard.
func (idx *InvertedIndex) AddDocument(docID uint64, tokens []string) {
	if idx == nil || len(idx.shards) == 0 || len(tokens) == 0 {
		return
	}

	// Aggregate counts per token for this document to reduce lock acquisitions.
	docTokenCounts := make(map[string]uint32, len(tokens))
	for _, t := range tokens {
		if t == "" {
			continue
		}
		docTokenCounts[t]++
	}

	for token, count := range docTokenCounts {
		shardIdx := idx.shardForToken(token)
		shard := &idx.shards[shardIdx]

		if strings.Contains(token, "trooper") {
			fmt.Printf("🎯 [Indexer] MATCH! 'trooper' eklendi -> DocID: %d, Shard: %d, Count: %d\n", docID, shardIdx, count)
		}

		shard.mu.Lock()
		postingForToken, ok := shard.postings[token]
		if !ok {
			postingForToken = make(map[uint64]uint32)
			shard.postings[token] = postingForToken
		}
		postingForToken[docID] += count
		shard.mu.Unlock()
	}
}

// GetPostings retrieves the docIDs and their term counts for a given token.
// It is concurrency-safe for reading.
func (idx *InvertedIndex) GetPostings(token string) map[uint64]uint32 {
	if idx == nil || len(idx.shards) == 0 {
		return nil
	}

	shardIdx := idx.shardForToken(token)
	shard := &idx.shards[shardIdx]

	shard.mu.RLock()
	defer shard.mu.RUnlock()

	postings, ok := shard.postings[token]
	if !ok {
		return nil
	}

	// Create a copy to safely use outside the lock
	result := make(map[uint64]uint32, len(postings))
	for docID, count := range postings {
		result[docID] = count
	}

	return result
}
