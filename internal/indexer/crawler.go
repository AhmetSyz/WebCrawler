package indexer

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/html" // Bu, Go ekibinin standart HTML ayrıştırıcısıdır.
)

// PRD Gereksinimi 6.1: Tarama işi yapısı
type CrawlJob struct {
	URL       string
	OriginURL string
	Depth     int
}

// PRD Gereksinimi 6.2: Çekilen sayfa verisi
type FetchedPage struct {
	URL         string
	OriginURL   string
	Depth       int
	Body        string
	StatusCode  int
	ContentType string
}

// DocMeta is the document metadata we store per indexed docID.
type DocMeta struct {
	DocURL    string
	OriginURL string
	Depth     int
}

// URLDocMap assigns a stable uint64 ID to each unique URL.
// It is safe for concurrent use by multiple crawl workers.
type URLDocMap struct {
	mu      sync.RWMutex
	idByURL map[string]uint64
	nextID  uint64
}

func NewURLDocMap() *URLDocMap {
	return &URLDocMap{
		idByURL: make(map[string]uint64),
	}
}

// GetOrCreate returns the docID for url, creating a new one if needed.
func (m *URLDocMap) GetOrCreate(url string) uint64 {
	m.mu.RLock()
	if id, ok := m.idByURL[url]; ok {
		m.mu.RUnlock()
		return id
	}
	m.mu.RUnlock()

	m.mu.Lock()
	defer m.mu.Unlock()
	// Re-check after acquiring the write lock.
	if id, ok := m.idByURL[url]; ok {
		return id
	}
	m.nextID++
	m.idByURL[url] = m.nextID
	return m.nextID
}

// DocumentStore holds DocMeta per docID.
type DocumentStore struct {
	mu      sync.RWMutex
	docByID map[uint64]DocMeta
}

func NewDocumentStore() *DocumentStore {
	return &DocumentStore{
		docByID: make(map[uint64]DocMeta),
	}
}

func (s *DocumentStore) Add(docID uint64, meta DocMeta) {
	s.mu.Lock()
	s.docByID[docID] = meta
	s.mu.Unlock()
}

func (s *DocumentStore) Get(docID uint64) (DocMeta, bool) {
	s.mu.RLock()
	meta, ok := s.docByID[docID]
	s.mu.RUnlock()
	return meta, ok
}

// Fetcher: Sayfayı internetten indirir
type Fetcher struct {
	client  *http.Client
	maxSize int64 // Bellek güvenliği için
}

func NewFetcher(timeout time.Duration, maxSize int64) *Fetcher {
	return &Fetcher{
		client: &http.Client{
			Timeout: timeout, // Zaman aşımı kontrolü
		},
		maxSize: maxSize,
	}
}

// Fetch metodu sayfayı çeker ve PRD'ye uygun şekilde döner
func (f *Fetcher) Fetch(job CrawlJob) (*FetchedPage, error) {
	req, err := http.NewRequest("GET", job.URL, nil)
	if err != nil {
		return nil, err
	}

	// Wikipedia and other sites require a User-Agent header to not serve a 403 Forbidden
	req.Header.Set("User-Agent", "Google-in-a-Day/1.0 Crawler (Educational Project)")

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status code: %d", resp.StatusCode)
	}

	// Sadece HTML sayfalarını tara
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if !strings.Contains(contentType, "text/html") {
		return nil, fmt.Errorf("non-html content: %s", contentType)
	}

	// Bellek şişmesini önlemek için okuma sınırla
	bodyReader := io.LimitReader(resp.Body, f.maxSize)
	bodyBytes, err := io.ReadAll(bodyReader)
	if err != nil {
		return nil, err
	}

	return &FetchedPage{
		URL:         job.URL,
		OriginURL:   job.OriginURL,
		Depth:       job.Depth,
		Body:        string(bodyBytes),
		StatusCode:  resp.StatusCode,
		ContentType: contentType,
	}, nil
}

// ExtractLinks reads HTML and extracts links resolved against base.
// It filters to links that match allowedHost (by hostname only).
func ExtractLinks(body io.Reader, base *url.URL, allowedHost string) ([]string, error) {
	var links []string
	z := html.NewTokenizer(body) // İşte golang.org/x/net/html burada kullanılıyor!

	binaryExts := []string{
		".zip",
		".gz",
		".pkg",
		".pdf",
		".exe",
		".dmg",
		".tar",
		".tgz",
		".tar.gz",
		".msi",
		".bz2",
		".7z",
		".rar",
		".iso",
		".img",
		".wim",
		".cab",
		//".arj",
		//".lzh",
		".lzx",
		".lzma",
		".tlz",
		".txz",
		".tgz",
		".tbz2",
		".tzst",
	}

	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			if z.Err() == io.EOF {
				return links, nil // Sayfa bitti
			}
			return nil, z.Err()

		case html.StartTagToken, html.SelfClosingTagToken:
			t := z.Token()
			if t.Data == "a" { // <a> etiketi bulduk
				for _, a := range t.Attr {
					if a.Key == "href" {
						// Linki ana URL'e göre temizle/çözümle
						u, err := url.Parse(a.Val) // İşte net/url burada kullanılıyor!
						if err != nil {
							continue
						}
						// Göreceli linkleri tam URL'e çevirir (/about -> https://google.com/about)
						resolved := base.ResolveReference(u)
						if allowedHost != "" && resolved.Hostname() != allowedHost {
							continue
						}

						// Skip common binary file types (by URL path suffix).
						pathLower := strings.ToLower(resolved.Path)
						skip := false
						for _, ext := range binaryExts {
							if strings.HasSuffix(pathLower, ext) {
								skip = true
								break
							}
						}
						if skip {
							continue
						}

						links = append(links, resolved.String())
					}
				}
			}
		}
	}
}

// unboundedJobQueue is a simple in-memory queue that never blocks producers.
// Back-pressure is enforced only at the bounded `frontier` channel boundary.
type unboundedJobQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	items  []CrawlJob
	closed bool
}

func newUnboundedJobQueue() *unboundedJobQueue {
	q := &unboundedJobQueue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *unboundedJobQueue) push(job CrawlJob) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.closed {
		return
	}

	q.items = append(q.items, job)
	q.cond.Signal()
}

func (q *unboundedJobQueue) pop() (CrawlJob, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	for len(q.items) == 0 && !q.closed {
		q.cond.Wait()
	}

	if len(q.items) == 0 && q.closed {
		return CrawlJob{}, false
	}

	job := q.items[0]
	// Avoid memory retention.
	q.items[0] = CrawlJob{}
	q.items = q.items[1:]
	return job, true
}

func (q *unboundedJobQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.closed {
		return
	}
	q.closed = true
	q.cond.Broadcast()
}

// Crawl orchestrates fetching and link expansion using a bounded frontier and a worker pool.
func Crawl(
	startURL string,
	originURL string,
	fetcher *Fetcher,
	store Store,
	maxDepthK int,
	frontierQueueCapacity int,
	maxConcurrency int,
	maxURLs int,
	invertedIndex *InvertedIndex,
	onProgress func(url string, pagesCrawled int),
) (*DocumentStore, error) {
	if fetcher == nil {
		return nil, fmt.Errorf("fetcher is nil")
	}
	if store == nil {
		return nil, fmt.Errorf("store is nil")
	}
	if startURL == "" {
		return nil, fmt.Errorf("startURL is empty")
	}
	if maxDepthK < 0 {
		return nil, fmt.Errorf("maxDepthK must be >= 0")
	}
	if frontierQueueCapacity <= 0 {
		return nil, fmt.Errorf("frontierQueueCapacity must be > 0")
	}
	if maxConcurrency <= 0 {
		return nil, fmt.Errorf("maxConcurrency must be > 0")
	}
	if maxURLs <= 0 {
		maxURLs = int(^uint(0) >> 1) // effectively unlimited
	}
	if originURL == "" {
		originURL = startURL
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	originParsed, parseErr := url.Parse(originURL)
	if parseErr != nil {
		return nil, fmt.Errorf("invalid originURL: %w", parseErr)
	}
	allowedHost := originParsed.Hostname()

	if invertedIndex == nil {
		return nil, fmt.Errorf("invertedIndex is nil")
	}

	// frontier is bounded to provide back-pressure at the fetch scheduling boundary.
	frontier := make(chan CrawlJob, frontierQueueCapacity)

	// discoveredQueue is an internal buffer between worker link discovery and the bounded frontier.
	// This is what prevents workers from blocking when frontier is full.
	discoveredQueue := newUnboundedJobQueue()

	var (
		inFlightMu sync.Mutex
		inFlight   int // jobs either queued for discovery, sitting in frontier, or being processed
		closeOnce  sync.Once
		workerWG   sync.WaitGroup
		dispatchWG sync.WaitGroup
	)

	var pagesCrawled uint64

	stop := func() {
		closeOnce.Do(func() {
			cancel()
			discoveredQueue.close()
			close(frontier)
		})
	}

	inc := func() {
		inFlightMu.Lock()
		inFlight++
		inFlightMu.Unlock()
	}

	dec := func() {
		inFlightMu.Lock()
		inFlight--
		shouldStop := inFlight == 0
		inFlightMu.Unlock()

		if shouldStop {
			stop()
		}
	}

	// Track the initial job as in-flight.
	inFlight = 1
	frontier <- CrawlJob{URL: startURL, OriginURL: originURL, Depth: 0}

	urlDocMap := NewURLDocMap()
	docStore := NewDocumentStore()

	// Dispatcher forwards discovered jobs into the bounded frontier.
	dispatchWG.Add(1)
	go func() {
		defer dispatchWG.Done()
		for {
			job, ok := discoveredQueue.pop()
			if !ok {
				return
			}
			select {
			case <-ctx.Done():
				return
			case frontier <- job:
			}
		}
	}()

	workerWG.Add(maxConcurrency)
	for i := 0; i < maxConcurrency; i++ {
		go func() {
			defer workerWG.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case job, ok := <-frontier:
					if !ok {
						return
					}

					if allowedHost != "" {
						u, err := url.Parse(job.URL)
						if err != nil || u.Hostname() != allowedHost {
							dec()
							continue
						}
					}

					// Strict maxURLs enforcement: reserve a slot BEFORE fetching.
					reserved := atomic.AddUint64(&pagesCrawled, 1)
					if int(reserved) > maxURLs {
						stop()
						dec()
						return
					}

					// Dedup happens right before fetching.
					if store.AddVisited(job.URL) {
						page, err := fetcher.Fetch(job)
						if err == nil {
							// Tokenize and index the fetched page.
							tokens := Tokenize(page.Body)

							// Extract links before dropping the body.
							var extractedLinks []string
							if job.Depth < maxDepthK {
								base, err := url.Parse(job.URL)
								if err == nil && base != nil {
									links, err := ExtractLinks(strings.NewReader(page.Body), base, allowedHost)
									if err == nil {
										extractedLinks = links
									}
								}
							}

							// Drop the HTML body completely to save memory
							page.Body = ""

							docID := urlDocMap.GetOrCreate(page.URL)
							docStore.Add(docID, DocMeta{
								DocURL:    page.URL,
								OriginURL: job.OriginURL,
								Depth:     job.Depth,
							})

							invertedIndex.AddDocument(docID, tokens)

							// Progress callback + GC every 100 pages
							if onProgress != nil {
								onProgress(page.URL, int(reserved))
							}
							if reserved%100 == 0 {
								_ = FlushAndClearToDisk(invertedIndex, docStore)
								runtime.GC()
							}

							// Stop discovering new links if we already reached the limit.
							if int(reserved) >= maxURLs {
								stop()
								dec()
								return
							}

							// Enqueue extracted links
						linkLoop:
							for _, link := range extractedLinks {
								select {
								case <-ctx.Done():
									break linkLoop
								default:
								}
								// Extra safety: only enqueue links from the same host.
								if allowedHost != "" {
									lu, err := url.Parse(link)
									if err != nil || lu.Hostname() != allowedHost {
										continue
									}
								}
								inc()
								discoveredQueue.push(CrawlJob{
									URL:       link,
									OriginURL: job.OriginURL,
									Depth:     job.Depth + 1,
								})
							}
						}
					}

					dec()
				}
			}
		}()
	}

	workerWG.Wait()
	dispatchWG.Wait()
	saveErr := store.SaveState()
	if saveErr != nil {
		return docStore, saveErr
	}
	return docStore, nil
}
