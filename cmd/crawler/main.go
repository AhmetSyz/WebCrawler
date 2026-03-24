package main

import (
	"encoding/json"
	"fmt"
	"google-in-a-day/internal/indexer"
	"google-in-a-day/internal/search"
	"html/template"
	"net/http"
	"path/filepath"
	"sync"
	"time"
)

// CrawlManager tracks the current crawl status
type CrawlManager struct {
	mu              sync.RWMutex
	IsRunning       bool
	PagesCrawled    int
	LastURL         string
	MaxURLs         int
	RecentURLs      []string // Last 5 URLs
	StartTime       time.Time
	CurrentProgress float64 // 0 to 100
}

// CrawlConfig holds the parameters for starting a crawl
type CrawlConfig struct {
	StartURL       string `json:"startURL"`
	MaxDepth       int    `json:"maxDepth"`
	MaxConcurrency int    `json:"maxConcurrency"`
	QueueCapacity  int    `json:"queueCapacity"`
	MaxURLs        int    `json:"maxURLs"`
}

// StatusResponse is the JSON response for /api/status
type StatusResponse struct {
	IsRunning      bool     `json:"isRunning"`
	PagesCrawled   int      `json:"pagesCrawled"`
	LastURL        string   `json:"lastURL"`
	MaxURLs        int      `json:"maxURLs"`
	Progress       float64  `json:"progress"`
	RecentURLs     []string `json:"recentURLs"`
	ElapsedSeconds float64  `json:"elapsedSeconds"`
}

// Global state
var (
	manager  *CrawlManager
	invIndex *indexer.InvertedIndex
	docStore *indexer.DocumentStore
	fetcher  *indexer.Fetcher
	store    indexer.Store
)

func init() {
	manager = &CrawlManager{
		RecentURLs: make([]string, 0, 5),
	}
	invIndex = indexer.NewInvertedIndex(32)
	docStore = indexer.NewDocumentStore()
	fetcher = indexer.NewFetcher(5*time.Second, 2*1024*1024)
	store = indexer.NewInMemoryStore()
}

func (m *CrawlManager) SetRunning(running bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.IsRunning = running
	if running {
		m.StartTime = time.Now()
		m.PagesCrawled = 0
		m.RecentURLs = make([]string, 0, 5)
	}
}

func (m *CrawlManager) AddCrawledURL(url string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.LastURL = url
	m.PagesCrawled++

	// Add to recent URLs (keep only last 5)
	m.RecentURLs = append(m.RecentURLs, url)
	if len(m.RecentURLs) > 5 {
		m.RecentURLs = m.RecentURLs[len(m.RecentURLs)-5:]
	}

	// Calculate progress
	if m.MaxURLs > 0 {
		m.CurrentProgress = (float64(m.PagesCrawled) / float64(m.MaxURLs)) * 100
		if m.CurrentProgress > 100 {
			m.CurrentProgress = 100
		}
	}
}

func (m *CrawlManager) GetStatus() StatusResponse {
	m.mu.RLock()
	defer m.mu.RUnlock()

	elapsed := 0.0
	if m.IsRunning {
		elapsed = time.Since(m.StartTime).Seconds()
	}

	// Make a copy of recent URLs
	recentCopy := make([]string, len(m.RecentURLs))
	copy(recentCopy, m.RecentURLs)

	return StatusResponse{
		IsRunning:      m.IsRunning,
		PagesCrawled:   m.PagesCrawled,
		LastURL:        m.LastURL,
		MaxURLs:        m.MaxURLs,
		Progress:       m.CurrentProgress,
		RecentURLs:     recentCopy,
		ElapsedSeconds: elapsed,
	}
}

func (m *CrawlManager) SetMaxURLs(max int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.MaxURLs = max
}

func main() {
	logFile, err := indexer.SetupLogger(".", false)
	if err != nil {
		panic(err)
	}
	defer logFile.Close()

	fmt.Println("🎸 Google in a Day: Dynamic Dashboard Edition 🎸")
	fmt.Println("--------------------------------------------------")
	fmt.Println("✅ Crawler initialized. Waiting for API commands...")
	fmt.Println("👉 Visit http://localhost:8080/ to access the dashboard")

	// Serve static files from ui folder
	http.Handle("/ui/", http.StripPrefix("/ui/", http.FileServer(http.Dir(filepath.Join(".", "ui")))))

	// Main dashboard page
	templatesDir := filepath.Join(".", "ui")
	tmplIndex := template.Must(template.ParseFiles(filepath.Join(templatesDir, "index.html")))
	tmplResults := template.Must(template.ParseFiles(filepath.Join(templatesDir, "results.html")))

	// Root handler
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		tmplIndex.Execute(w, nil)
	})

	// Search page handler
	http.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query().Get("q")
		if query == "" {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}

		results := search.Search(query, invIndex, docStore)

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		data := struct {
			Query   string
			Results []search.SearchResult
		}{
			Query:   query,
			Results: results,
		}

		if err := tmplResults.Execute(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})

	// API: Start crawl
	http.HandleFunc("/api/crawl", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error": "POST required"}`, http.StatusMethodNotAllowed)
			return
		}

		var config CrawlConfig
		if err := json.NewDecoder(r.Body).Decode(&config); err != nil {
			http.Error(w, `{"error": "Invalid JSON"}`, http.StatusBadRequest)
			return
		}

		// Validate inputs
		if config.StartURL == "" {
			http.Error(w, `{"error": "startURL is required"}`, http.StatusBadRequest)
			return
		}
		if config.MaxDepth < 0 {
			http.Error(w, `{"error": "maxDepth must be >= 0"}`, http.StatusBadRequest)
			return
		}
		if config.MaxConcurrency <= 0 {
			config.MaxConcurrency = 10
		}
		if config.QueueCapacity <= 0 {
			config.QueueCapacity = 200
		}
		if config.MaxURLs <= 0 {
			config.MaxURLs = 10000
		}

		// Check if already running
		if manager.IsRunning {
			http.Error(w, `{"error": "Crawler is already running"}`, http.StatusConflict)
			return
		}

		// Start crawl in a goroutine
		go func() {
			manager.SetRunning(true)
			manager.SetMaxURLs(config.MaxURLs)

			// Reset index + docstore per crawl so search sees the same instances.
			invIndex = indexer.NewInvertedIndex(32)
			docStore = indexer.NewDocumentStore()
			store = indexer.NewInMemoryStore()

			// Create progress callback
			onProgress := func(url string, count int) {
				manager.mu.Lock()
				manager.LastURL = url
				manager.PagesCrawled = count
				// maintain recent urls
				manager.RecentURLs = append(manager.RecentURLs, url)
				if len(manager.RecentURLs) > 5 {
					manager.RecentURLs = manager.RecentURLs[len(manager.RecentURLs)-5:]
				}
				if manager.MaxURLs > 0 {
					manager.CurrentProgress = (float64(manager.PagesCrawled) / float64(manager.MaxURLs)) * 100
					if manager.CurrentProgress > 100 {
						manager.CurrentProgress = 100
					}
				}
				manager.mu.Unlock()
			}

			fmt.Printf("🔍 Starting crawl: %s (depth=%d, concurrency=%d)\n",
				config.StartURL, config.MaxDepth, config.MaxConcurrency)

			crawlDocStore, err := indexer.Crawl(
				config.StartURL,
				config.StartURL,
				fetcher,
				store,
				config.MaxDepth,
				config.QueueCapacity,
				config.MaxConcurrency,
				config.MaxURLs,
				invIndex,
				onProgress,
			)

			if err != nil {
				fmt.Printf("❌ Crawl error: %v\n", err)
			} else {
				// CRITICAL: ensure search handlers see the exact docStore instance built by Crawl.
				docStore = crawlDocStore
				if flushErr := indexer.FlushToDisk(invIndex, docStore); flushErr != nil {
					fmt.Printf("❌ FlushToDisk error: %v\n", flushErr)
				}

				// Export visited URLs as required output artifact.
				if path, vErr := indexer.WriteVisitedURLsData(store, filepath.Join("data")); vErr != nil {
					fmt.Printf("❌ visited_urls.data export error: %v\n", vErr)
				} else {
					fmt.Printf("✅ Visited URLs exported: %s\n", path)
				}

				fmt.Println("✅ Crawl completed successfully!")
			}

			manager.SetRunning(false)
		}()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"message": "Crawl started in background",
		})
	})

	// API: Get status
	http.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, `{"error": "GET required"}`, http.StatusMethodNotAllowed)
			return
		}

		status := manager.GetStatus()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(status)
	})

	// API: Search
	http.HandleFunc("/api/search", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query().Get("q")
		if query == "" {
			http.Error(w, `{"error": "Missing 'q' parameter"}`, http.StatusBadRequest)
			return
		}

		results := search.Search(query, invIndex, docStore)

		w.Header().Set("Content-Type", "application/json")
		if results == nil {
			w.Write([]byte(`[]`))
			return
		}

		if err := json.NewEncoder(w).Encode(results); err != nil {
			http.Error(w, `{"error": "Failed to encode results"}`, http.StatusInternalServerError)
		}
	})

	fmt.Println("🌐 Server starting on http://localhost:8080/")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		fmt.Printf("❌ Server error: %v\n", err)
	}
}
