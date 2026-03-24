package main

import (
	"context"
	"encoding/json"
	"fmt"
	"google-in-a-day/internal/api"
	"google-in-a-day/internal/indexer"
	"google-in-a-day/internal/search"
	"html/template"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// CrawlManager tracks the current crawl status
// (duplicated from cmd/crawler/main.go to keep this entrypoint self-contained).
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
// (same JSON contract as the dashboard expects).
type CrawlConfig struct {
	StartURL       string `json:"startURL"`
	MaxDepth       int    `json:"maxDepth"`
	MaxConcurrency int    `json:"maxConcurrency"`
	QueueCapacity  int    `json:"queueCapacity"`
	MaxURLs        int    `json:"maxURLs"`
}

// StatusResponse is the JSON response for /api/status.
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
	manager  = &CrawlManager{RecentURLs: make([]string, 0, 5)}
	invIndex = indexer.NewInvertedIndex(32)
	docStore = indexer.NewDocumentStore()
	fetcher  = indexer.NewFetcher(5*time.Second, 2*1024*1024)
	store    = indexer.NewInMemoryStore()
)

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

func (m *CrawlManager) SetMaxURLs(max int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.MaxURLs = max
}

func (m *CrawlManager) GetStatus() StatusResponse {
	m.mu.RLock()
	defer m.mu.RUnlock()

	elapsed := 0.0
	if m.IsRunning {
		elapsed = time.Since(m.StartTime).Seconds()
	}

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

func main() {
	logFile, err := indexer.SetupLogger(".", false)
	if err != nil {
		panic(err)
	}
	defer logFile.Close()

	fmt.Println("All-in-one: Dashboard(:8080) + Search API(:3600)")

	// --- Search API server (:3600)
	searchAPI, err := api.NewServer(".")
	if err != nil {
		panic(err)
	}
	searchSrv := &http.Server{
		Addr:              ":3600",
		Handler:           searchAPI.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// --- Dashboard server (:8080)
	dashMux := http.NewServeMux()

	// Serve static files from ui folder
	dashMux.Handle("/ui/", http.StripPrefix("/ui/", http.FileServer(http.Dir(filepath.Join(".", "ui")))))

	templatesDir := filepath.Join(".", "ui")
	tmplIndex := template.Must(template.ParseFiles(filepath.Join(templatesDir, "index.html")))
	tmplResults := template.Must(template.ParseFiles(filepath.Join(templatesDir, "results.html")))

	dashMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		_ = tmplIndex.Execute(w, nil)
	})

	dashMux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
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

	// JSON endpoints
	dashMux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, `{"error": "GET required"}`, http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(manager.GetStatus())
	})

	dashMux.HandleFunc("/api/search", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query().Get("q")
		if query == "" {
			http.Error(w, `{"error": "Missing 'q' parameter"}`, http.StatusBadRequest)
			return
		}

		results := search.Search(query, invIndex, docStore)
		w.Header().Set("Content-Type", "application/json")
		if results == nil {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		if err := json.NewEncoder(w).Encode(results); err != nil {
			http.Error(w, `{"error": "Failed to encode results"}`, http.StatusInternalServerError)
		}
	})

	// /api/crawl is longer; keep it identical behavior-wise, but in this file.
	dashMux.HandleFunc("/api/crawl", func(w http.ResponseWriter, r *http.Request) {
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
		manager.mu.RLock()
		running := manager.IsRunning
		manager.mu.RUnlock()
		if running {
			http.Error(w, `{"error": "Crawler is already running"}`, http.StatusConflict)
			return
		}

		go func() {
			manager.SetRunning(true)
			manager.SetMaxURLs(config.MaxURLs)

			// Reset per crawl
			invIndex = indexer.NewInvertedIndex(32)
			docStore = indexer.NewDocumentStore()
			store = indexer.NewInMemoryStore()

			onProgress := func(url string, count int) {
				manager.mu.Lock()
				manager.LastURL = url
				manager.PagesCrawled = count
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

			if err == nil {
				docStore = crawlDocStore
				_ = indexer.FlushToDisk(invIndex, docStore)
				_, _ = indexer.WriteVisitedURLsData(store, filepath.Join("data"))
			}
			manager.SetRunning(false)
		}()

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"message":"Crawl started in background"}`))
	})

	dashSrv := &http.Server{
		Addr:              ":8080",
		Handler:           dashMux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// --- run both with graceful shutdown
	errCh := make(chan error, 2)
	go func() { errCh <- dashSrv.ListenAndServe() }()
	go func() { errCh <- searchSrv.ListenAndServe() }()

	fmt.Println("Dashboard:  http://localhost:8080/")
	fmt.Println("Search API: http://localhost:3600/search?query=block&sortBy=relevance")

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-stop:
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = dashSrv.Shutdown(ctx)
		_ = searchSrv.Shutdown(ctx)
	case err := <-errCh:
		// If one server fails, shut down the other.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = dashSrv.Shutdown(ctx)
		_ = searchSrv.Shutdown(ctx)
		if err != nil && err != http.ErrServerClosed {
			panic(err)
		}
	}
}
