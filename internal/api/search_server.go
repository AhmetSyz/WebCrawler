package api

import (
	"bufio"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Posting mirrors the repo's on-disk inverted-index line format:
// token,url,origin_url,depth,count
//
// This is what currently exists under data/storage/*.data (including p.data).
type Posting struct {
	Token  string
	URL    string
	Count  int
	Origin string
	Depth  int
}

type SearchResult struct {
	URL       string `json:"url"`
	Relevance int    `json:"relevance"`
}

// Server provides GET /search on localhost:3600 backed by an in-memory cache.
//
// For RAM efficiency it does NOT load all shards on startup. Instead it lazily
// loads and caches the shard for the first character of the queried token.
//
// data/storage/<shard>.data where shard is:
// - a..z for alphabetic tokens
// - _ for anything else (digits, punctuation, etc.)
type Server struct {
	baseDir string

	mu      sync.RWMutex
	byShard map[string]map[string]map[string]int // shard -> token -> url -> summed count
	loadErr map[string]error                     // shard -> last load error (if any)
}

// NewServer creates a Search API server that reads from data/storage/*.data.
//
// baseDir should typically be the project root.
func NewServer(baseDir string) (*Server, error) {
	if baseDir == "" {
		baseDir = "."
	}
	return &Server{
		baseDir: baseDir,
		byShard: make(map[string]map[string]map[string]int, 32),
		loadErr: make(map[string]error, 32),
	}, nil
}

// NewServerFromPData is kept for compatibility; it now behaves like NewServer.
func NewServerFromPData(baseDir string) (*Server, error) { return NewServer(baseDir) }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/search", s.handleSearch)
	return mux
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}

	q := strings.TrimSpace(r.URL.Query().Get("query"))
	if q == "" {
		http.Error(w, "missing query parameter 'query'", http.StatusBadRequest)
		return
	}
	sortBy := strings.TrimSpace(r.URL.Query().Get("sortBy"))

	// The query must match the token exactly.
	token := strings.ToLower(q)
	shard := shardForToken(token)

	byToken, err := s.ensureShardLoaded(shard)
	if err != nil {
		http.Error(w, "failed to load shard: "+err.Error(), http.StatusInternalServerError)
		return
	}

	urlCounts := byToken[token]
	w.Header().Set("Content-Type", "application/json")
	if len(urlCounts) == 0 {
		_, _ = w.Write([]byte("[]"))
		return
	}

	results := make([]SearchResult, 0, len(urlCounts))
	for url, c := range urlCounts {
		results = append(results, SearchResult{URL: url, Relevance: c})
	}

	if sortBy == "relevance" {
		sort.SliceStable(results, func(i, j int) bool {
			if results[i].Relevance == results[j].Relevance {
				return results[i].URL < results[j].URL
			}
			return results[i].Relevance > results[j].Relevance
		})
	}

	_ = json.NewEncoder(w).Encode(results)
}

func (s *Server) ensureShardLoaded(shard string) (map[string]map[string]int, error) {
	// Fast path
	s.mu.RLock()
	m := s.byShard[shard]
	err := s.loadErr[shard]
	s.mu.RUnlock()
	if m != nil {
		return m, nil
	}
	if err != nil {
		return nil, err
	}

	// Slow path
	s.mu.Lock()
	defer s.mu.Unlock()
	if m = s.byShard[shard]; m != nil {
		return m, nil
	}
	if err = s.loadErr[shard]; err != nil {
		return nil, err
	}

	path := filepath.Join(s.baseDir, "data", "storage", shard+".data")
	byToken, loadErr := loadIndexShard(path)
	if loadErr != nil {
		s.loadErr[shard] = loadErr
		return nil, loadErr
	}
	// Cache it.
	s.byShard[shard] = byToken
	return byToken, nil
}

func shardForToken(token string) string {
	if token == "" {
		return "_"
	}
	b := token[0]
	if b >= 'a' && b <= 'z' {
		return string([]byte{b})
	}
	if b >= 'A' && b <= 'Z' {
		return string([]byte{b + ('a' - 'A')})
	}
	return "_"
}

func loadIndexShard(path string) (map[string]map[string]int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	// allow long lines
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 10*1024*1024)

	byToken := make(map[string]map[string]int, 4096)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		// token,url,origin_url,depth,count
		parts := strings.SplitN(line, ",", 5)
		if len(parts) != 5 {
			return nil, corruptedLine(path, lineNo)
		}

		token := strings.ToLower(strings.TrimSpace(parts[0]))
		url := strings.TrimSpace(parts[1])
		if token == "" || url == "" {
			return nil, corruptedLine(path, lineNo)
		}

		cnt := atoi(strings.TrimSpace(parts[4]))
		if cnt <= 0 {
			continue
		}

		m := byToken[token]
		if m == nil {
			m = make(map[string]int, 8)
			byToken[token] = m
		}
		m[url] += cnt
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return byToken, nil
}

func corruptedLine(path string, lineNo int) error {
	return errors.New("data file corrupted: " + path + " (line " + itoa(lineNo) + ")")
}

func atoi(s string) int {
	// small helper to avoid importing strconv
	neg := false
	if s == "" {
		return 0
	}
	if s[0] == '-' {
		neg = true
		s = s[1:]
	}
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	if neg {
		return -n
	}
	return n
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	var b [32]byte
	n := 0
	for i > 0 {
		b[n] = byte('0' + (i % 10))
		n++
		i /= 10
	}
	if neg {
		b[n] = '-'
		n++
	}
	for l, r := 0, n-1; l < r; l, r = l+1, r-1 {
		b[l], b[r] = b[r], b[l]
	}
	return string(b[:n])
}
