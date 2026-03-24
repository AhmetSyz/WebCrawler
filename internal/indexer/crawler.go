package indexer

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
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
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				// Avoid redirect loops / huge chains.
				if len(via) >= 10 {
					return fmt.Errorf("stopped after 10 redirects")
				}
				// Keep headers across redirects.
				if len(via) > 0 {
					req.Header = via[len(via)-1].Header.Clone()
				}
				return nil
			},
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

	// User-Agent spoofing: use a realistic modern UA to avoid 403/light pages.
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/123.0.0.0 Safari/537.36")
	// Common accept headers reduce the chance of being served non-standard variants.
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Connection", "keep-alive")

	resp, err := f.client.Do(req)
	if err != nil {
		// Detailed error logging for timeouts / dial errors etc.
		var nerr net.Error
		if errors.As(err, &nerr) && nerr.Timeout() {
			log.Printf("[fetch][timeout] url=%s err=%v", job.URL, err)
		} else {
			log.Printf("[fetch][error] url=%s err=%v", job.URL, err)
		}
		return nil, err
	}
	defer resp.Body.Close()

	// Log non-OK statuses to diagnose blocking/rate limits.
	if resp.StatusCode != http.StatusOK {
		log.Printf("[fetch][status] url=%s status=%d", job.URL, resp.StatusCode)
		return nil, fmt.Errorf("status code: %d", resp.StatusCode)
	}

	// Sadece HTML sayfalarını tara
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if !strings.Contains(contentType, "text/html") {
		return nil, fmt.Errorf("non-html content: %s", contentType)
	}

	// Handle gzip if present (Accept-Encoding: gzip).
	var bodySrc io.Reader = resp.Body
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Encoding")), "gzip") {
		gz, gzErr := gzip.NewReader(resp.Body)
		if gzErr != nil {
			return nil, gzErr
		}
		defer gz.Close()
		bodySrc = gz
	}

	// Bellek şişmesini önlemek için okuma sınırla
	bodyReader := io.LimitReader(bodySrc, f.maxSize)
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

// normalizeAndFilterLink resolves u against base, enforces allowedHost, strips fragments,
// and rejects mailto/javascript and common non-web schemes.
func normalizeAndFilterLink(href string, base *url.URL, allowedHost string) (string, bool) {
	href = strings.TrimSpace(href)
	if href == "" {
		return "", false
	}
	lower := strings.ToLower(href)
	if strings.HasPrefix(lower, "mailto:") || strings.HasPrefix(lower, "javascript:") || strings.HasPrefix(lower, "tel:") {
		return "", false
	}

	u, err := url.Parse(href)
	if err != nil {
		return "", false
	}
	resolved := base.ResolveReference(u)

	// Only http(s)
	scheme := strings.ToLower(resolved.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", false
	}

	// Strip fragment (#section) to avoid over-aggressive duplicate skipping.
	resolved.Fragment = ""

	if allowedHost != "" && resolved.Hostname() != allowedHost {
		return "", false
	}
	return resolved.String(), true
}

// ExtractLinks reads HTML and extracts links resolved against base.
// It filters to links that match allowedHost (by hostname only).
func ExtractLinks(body io.Reader, base *url.URL, allowedHost string) ([]string, error) {
	var links []string
	z := html.NewTokenizer(body)

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
				return links, nil
			}
			return nil, z.Err()

		case html.StartTagToken, html.SelfClosingTagToken:
			t := z.Token()
			if t.Data == "a" {
				for _, a := range t.Attr {
					if a.Key != "href" {
						continue
					}

					resolvedStr, ok := normalizeAndFilterLink(a.Val, base, allowedHost)
					if !ok {
						continue
					}

					resolved, err := url.Parse(resolvedStr)
					if err != nil {
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

					links = append(links, resolvedStr)
				}
			}
		}
	}
}

// ExtractVisibleText converts HTML to visible text by walking tokens and:
// - skipping script/style/noscript/template/svg
// - inserting spaces between text chunks
func ExtractVisibleText(htmlStr string) string {
	z := html.NewTokenizer(strings.NewReader(htmlStr))
	var b strings.Builder
	b.Grow(len(htmlStr) / 4)

	skipDepth := 0
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			if z.Err() == io.EOF {
				return b.String()
			}
			return b.String()
		case html.StartTagToken:
			tok := z.Token()
			switch tok.Data {
			case "script", "style", "noscript", "template", "svg":
				skipDepth++
			}
		case html.EndTagToken:
			tok := z.Token()
			switch tok.Data {
			case "script", "style", "noscript", "template", "svg":
				if skipDepth > 0 {
					skipDepth--
				}
			}
		case html.TextToken:
			if skipDepth > 0 {
				continue
			}
			text := strings.TrimSpace(html.UnescapeString(string(z.Text())))
			if text == "" {
				continue
			}
			if b.Len() > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(text)
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
							// Extract visible text for indexing (captures body text; avoids markup).
							visibleText := ExtractVisibleText(page.Body)
							tokens := Tokenize(visibleText)

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
