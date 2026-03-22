# Google in a Day (AI-Aided Computer Engineering)
## Phase 1 Product Requirement Document (PRD)

### Document Metadata
- **Project name:** Google in a Day
- **Phase:** 1 (Product Requirements)
- **Primary deliverable:** `product_prd.md`
- **Implementation language:** Go (standard library + Go concurrency primitives)

---

## 1. Overview
This project builds a functional web crawler and a real-time search engine from scratch using Go-native facilities. The crawler starts from a single origin URL and recursively discovers pages up to a maximum depth `k`. Crawling and indexing occur concurrently, enabling the search engine to answer queries while crawling is still in progress.

To ensure system stability, the system implements back-pressure via bounded queues and explicit throttling. A real-time dashboard exposes crawl progress, queue depth, and throttling status. As a bonus, the system is designed to support resumability after interruption.

---

## 2. Goals and Non-Goals

### 2.1 Goals
1. **Native implementation:** Use Go-native functionality (`net/http`, `html` parsing, `sync`, `channels`, concurrent-safe maps). Avoid high-level scraping/indexing frameworks.
2. **Recursive crawling:** Crawl from an origin URL up to max depth `k`, where depth is the shortest distance in link traversals from the origin (or the traversal depth used by the job scheduler; must be consistent).
3. **Concurrent execution:** Use concurrency-safe structures to run fetching, link extraction, indexing, and searching in parallel.
4. **Live indexing:** Search results are available while crawling continues (eventual consistency is acceptable; correctness must be well-defined).
5. **Back-pressure:** Prevent uncontrolled memory/CPU growth through queue bounds and rate throttling.
6. **Search output contract:** The searcher returns triples:
   - `(relevant_url, origin_url, depth)`
7. **Real-time dashboard & control:** Provide a UI and endpoints to **start** crawling, monitor progress, and run searches.
8. **Resumability (bonus):** Provide a checkpointing strategy to continue after a crash or manual stop.

### 2.2 Non-Goals (for Phase 1)
1. Full production-grade web compliance (e.g., exhaustive `robots.txt` policy evaluation) is optional unless required by course guidelines.
2. Semantic search / ML ranking beyond simple lexical scoring.
3. Distributed crawling across multiple machines.
4. Exact re-ranking of search results with a global consistent index snapshot (real-time implies streaming/incremental updates).

---

## 3. Users and User Stories

### 3.1 Users
1. **Course instructor / evaluator**: runs the system and validates correctness, concurrency behavior, and UX.
2. **Student developer**: modifies components and verifies performance and reliability.
3. **End user** (via dashboard/search API): enters queries and views crawling progress.

### 3.2 User Stories
1. As a user, I can start crawling from an origin URL with a configured maximum depth `k` and `max_urls` URL limit.
2. As a user, I can perform search queries immediately; results reflect pages already indexed.
3. As an evaluator, I can view a dashboard that shows crawl progress (fetched/indexed counts), queue depth, and throttling status.
4. As an operator (bonus), I can stop and restart the crawler and resume from the last checkpoint.

---

## 4. Assumptions and Definitions

### 4.1 Definitions
- **Origin URL:** The configured URL provided to the crawler; all discovered pages are associated with this origin.
- **Depth `d`:** The crawler job depth for a page: `origin` is depth `0`; children links increase depth by `1`.
- **Relevant URL:** A page URL returned by search based on matching the query terms.
- **Inverted index:** Mapping from tokens to postings lists referencing indexed documents.
- **Document metadata:** Stored per indexed page, including:
  - `doc_url`
  - `origin_url`
  - `depth`

### 4.2 Assumptions
1. The system will treat HTTP GET responses as crawlable content only when content-type indicates HTML (or similar text/html).
2. URL normalization and canonicalization are required for preventing duplicate crawls.
3. The system does not guarantee visiting all distinct URLs on the web; it guarantees consistent behavior for discovered URLs within the search space formed by allowed link extraction rules.
4. Search correctness is **incremental**: a query may or may not include pages currently being crawled, but once indexed they must be searchable.

---

## 5. System Architecture (High-Level)

### 5.1 Components
1. **Crawl Orchestrator**
   - Owns lifecycle (start/stop), configuration, cancellation contexts.
   - Manages the bounded frontier and global visited set.
   - Spawns worker pools for fetching and link extraction.
2. **Fetcher Workers**
   - `net/http` client fetches pages with timeouts and size limits.
   - Produces pages to the indexing pipeline.
3. **Link Extractor**
   - Parses HTML using Go-native HTML tokenization / parsing.
   - Extracts candidate URLs from anchors (`href`), optionally others.
   - Normalizes URLs and emits new crawl jobs (if depth permits).
4. **Indexer**
   - Updates an in-memory inverted index concurrently with crawling.
   - Stores document metadata for search result triple generation.
   - **Memory management:** periodically flushes index segments to disk and clears in-memory postings to keep RAM bounded.
5. **Search Service**
   - Serves user queries (HTTP endpoint and/or internal API).
   - Reads the live in-memory index concurrently.
   - If/when in-memory index has been cleared, it may fall back to disk segments (append-only) for token lookup.
   - Returns ranked triples: `(relevant_url, origin_url, depth)`.
6. **Metrics Reporter / Control Plane**
   - Aggregates real-time metrics about:
     - pages crawled
     - last URL
     - max URL limit
     - recent URLs
     - elapsed time
   - Feeds the dashboard via HTTP endpoints.
7. **Persistence Manager (Bonus)**
   - Provides checkpointing of:
     - visited set
     - frontier queue state
     - indexed document metadata and/or index snapshot
   - Restores system state on restart.

### 5.2 Concurrency Model
The system is composed of goroutine worker pools connected by bounded channels:
- `frontierJobs chan CrawlJob` (bounded)

Shared state must be concurrency-safe:
- Visited set: `sync.Map` or mutex-protected set
- Index: sharded maps with `sync.RWMutex` (or per-token shard locking) for concurrent read/write
- Metrics: atomic counters and/or mutex-protected metrics snapshot

---

## 6. Data Model and Core Data Structures

### 6.1 Crawl Job Structures
```go
type CrawlJob struct {
  URL        string
  OriginURL  string
  Depth      int
}
```

### 6.2 Fetched Page
```go
type FetchedPage struct {
  URL         string
  OriginURL   string
  Depth       int
  Body        []byte // bounded by max page size
  FinalURL    string // after redirects, if tracked
  StatusCode  int
  ContentType string
}
```

**Memory rule:** The crawler must not retain full HTML bodies after link extraction + tokenization. The body buffer must be eligible for GC as soon as the page has been processed.

### 6.3 Document Metadata
```go
type DocMeta struct {
  DocURL    string
  OriginURL string
  Depth     int
}
```

### 6.4 Inverted Index Types
Lexical index for token -> postings:
```go
type Posting struct {
  DocID uint64
  // Minimal ranking signal; can be extended to TF, positions, etc.
  TermCount uint32
}

type IndexShard struct {
  mu sync.RWMutex
  // token -> docID -> posting
  postings map[string]map[uint64]*Posting
}
```

To support concurrent indexing and searching efficiently:
- Shard the index by hashing token strings into `N` shards.
- Each shard has its own `RWMutex`.

**Disk segment format (Phase 1 memory optimization):**
- The system may flush index entries to `storage/<firstLetter>.data` (append-only).
- Each line stores one posting record:
  - `token,url,origin_url,depth,count`

### 6.5 Document Store
```go
type DocumentStore struct {
  mu sync.RWMutex
  docByID map[uint64]DocMeta
}
```

Optionally, store URL -> DocID mapping for deduplication:
```go
type URLDocMap struct {
  mu sync.RWMutex
  idByURL map[string]uint64
  nextID uint64
}
```

---

## 7. Functional Requirements

### 7.1 Configuration & Startup
The system must accept configuration parameters:
1. `origin_url` (required)
2. `max_depth_k` (required, integer `k >= 0`)
3. `max_concurrency` (fetcher workers)
4. `frontier_queue_capacity` (bounded)
5. `fetch_timeout_ms` (request timeout)
6. `max_page_bytes` (limit fetched body size)
7. `max_urls` (strict upper bound on number of crawled pages)
8. Allowed schemes (default: `http` + `https`)
9. URL normalization settings:
   - remove fragments
   - canonicalize host casing
   - resolve relative links
10. Optional:
   - max redirects
   - per-host request rate limit / delay
   - same-origin policy or host allowlist/denylist

### 7.2 Recursive Crawling to Depth `k`
1. The crawler enqueues the origin as a job with `Depth=0`.
2. Workers fetch job URLs and extract outbound links.
3. For each extracted link:
   - normalize/canonicalize
   - compute new job depth = `job.Depth + 1`
   - enqueue only if `new_depth <= k`
4. The visited set must prevent duplicate enqueue/fetch cycles:
   - If a URL has already been marked visited, it is not enqueued again.

### 7.3 Strict Crawl Limiting (`max_urls`)
1. The crawler must enforce a strict upper bound `max_urls`.
2. Once the system reaches the limit, it must initiate cancellation and stop scheduling/fetching new work.
3. Acceptance criteria:
   - The crawler stops without deadlocking.
   - The dashboard reports the final count and transitions to idle.

### 7.4 Live Indexing (Concurrent Search Availability)
1. As soon as a fetched page is parsed into tokens, the indexer updates the inverted index.
2. Search queries served during crawling read from the live index.
3. The system must ensure that:
   - Index updates do not corrupt index memory (data race free).
   - Search readers see either the old or the new index state, but never partially corrupted state.

### 7.5 Memory Management Requirements (Phase 1)
1. **Bounded page retention:** HTML bodies must not be stored beyond link extraction and tokenization.
2. **Streaming tokenization:** HTML-to-text extraction should avoid large intermediate strings where possible.
3. **Index memory cap by flushing:** the inverted index must support periodic flushing to disk (append-only segments) and clearing in-memory postings to release RAM.
4. **GC friendliness:** long-lived references to large buffers/strings should be avoided; periodic GC is permitted as an operational safeguard.

### 7.6 Back-Pressure and Throttling
The system must implement at least one explicit back-pressure mechanism and ideally two:
1. **Bounded queues:** Use bounded channels for frontier and (optionally) fetch/index stages.
   - If the frontier is full, job producers must block or apply throttling.
2. **Rate limiting / delay:** Implement an explicit inter-request delay (global or per-host) using Go-native mechanisms.
3. **Concurrency limits:** Limit worker pool sizes to cap in-flight requests.

Acceptance criteria:
- Memory usage must remain bounded under load (queue capacities + periodic index flushing).
- System must not deadlock under normal operation.

### 7.7 Search Output Contract
When the user queries the search engine:
1. The search service identifies matching documents using the inverted index (and disk segments if enabled).
2. It returns a list of triples:
   - `relevant_url` (document URL)
   - `origin_url` (origin associated with that document)
   - `depth` (stored depth)
3. Ranking:
   - Phase 1 may use simple ranking such as sum of term counts or number of matched tokens.
   - The PRD must define ordering deterministically (e.g., highest score first, then stable tie-breaker such as URL lexicographic order).

### 7.8 Dashboard and Monitoring
The system must expose dashboard data in real time:
1. **Progress:** pages crawled, max URLs, percent complete.
2. **Operational:** running/idle, elapsed time, last crawled URL, recent URLs.
3. Optional metrics:
   - queue depth
   - failed fetch count
   - throttling status

Mechanism:
- Provide HTTP endpoints:
  - `POST /api/crawl` starts a crawl with a JSON configuration
  - `GET /api/status` returns a JSON snapshot
  - `GET /api/search` runs a query against the live index

### 7.9 Persistence (Bonus): Resumable Crawling and Indexing
The system should support:
1. **Checkpointing cadence:** periodic snapshots (e.g., every N seconds or after M indexed documents).
2. **Restart behavior:**
   - Load checkpoint state
   - Restore visited set
   - Restore frontier jobs queue
   - Restore index state (or replay indexed logs)
3. **Consistency model:**
   - Prefer “at least once” indexing semantics combined with deduplication.
   - Ensure that resuming does not corrupt index structures or cause uncontrolled duplicates.

Persistence scope suggestion:
- Minimum: resume crawling (visited + frontier).
- Bonus extension: resume indexing quickly by persisting the document store and index snapshot.

---

## 8. Non-Functional Requirements

### 8.1 Performance
- The system should support concurrent search while crawling.
- Index update latency should be low enough that search results appear quickly after pages are fetched.
- Bounded queues prevent unbounded memory growth.

### 8.2 Reliability and Correctness
- Data races must be eliminated via correct synchronization (`-race` acceptable for evaluation).
- Index and metrics must not panic under concurrent access.
- URL normalization must be deterministic to avoid inconsistent visited behavior.

### 8.3 Security and Safety
- Enforce request timeouts (`http.Client` with `Timeout` and/or context deadlines).
- Enforce max page size to avoid memory blowups.
- Validate URL schemes and sanitize extracted links.

### 8.4 Observability
- Structured logs (optional) for crawl start/stop, errors, throttling events, checkpoint saves.
- Dashboard metrics for queue depth and indexing progress.

---

## 9. API Requirements (Suggested)

### 9.1 Search API
`GET /search?q=<query>&origin=<optional>&limit=<optional>`
- Response JSON includes:
  - `results`: array of triples
  - Each triple object:
    - `relevant_url`
    - `origin_url`
    - `depth`
  - `query_time_ms`
  - `indexed_docs_so_far` (optional)

### 9.2 Status API
`GET /status`
- Returns JSON snapshot:
  - `fetched_count`
  - `indexed_count`
  - `failed_count`
  - `queue_depth`
  - `inflight_fetchers`
  - `active_workers`
  - `throttling_summary`
  - `started_at`, `last_checkpoint_at`

### 9.3 Events API
`GET /events`
- SSE stream:
  - periodic updates every `T` milliseconds (configurable)

---

## 10. Ranking and Relevance Model (Phase 1)
To satisfy lexical matching without heavy libraries:
1. Tokenize page text derived from HTML:
   - Extract visible text from HTML body.
2. Normalize tokens:
   - lowercase
   - remove punctuation
   - optional stopword list
3. Index:
   - For each token, track term count per document.
4. Search:
   - Compute score(doc) = sum(termCount for query tokens) or number of matched tokens.
5. Output:
   - Convert docID -> DocMeta to emit `(relevant_url, origin_url, depth)`.

Ranking must be deterministic:
- Sort by descending score.
- Tie-break by ascending `depth` (smaller depth first) or lexicographic `relevant_url`.

---

## 11. Back-Pressure and Throttling Details (Implementation Guidance)
### 11.1 Bounded Frontier
- `frontierJobs` is a buffered channel of capacity `frontier_queue_capacity`.
- Enqueue path:
  - Mark visited before enqueue to avoid duplicates.
  - If channel is full:
    - either block (preferred for correctness and simple control), or
    - apply a timeout and record throttle metrics.

### 11.2 Worker Pools
- `max_concurrency` fetch workers:
  - each worker loops reading from `frontierJobs`
  - fetches, extracts links, and pushes new jobs back to frontier

### 11.3 Rate Limiter (Per Host)
- Maintain a limiter per host:
  - A channel of tokens replenished via a goroutine or ticker.
  - Workers acquire a token before sending a request.
- Metrics:
  - count of blocked acquisitions
  - current token availability (approximate)

---

## 12. Persistence Design (Bonus)
### 12.1 Checkpoint Strategy
- Periodically write:
  - `visited` set snapshot
  - `frontier` queued jobs snapshot
  - `document store` snapshot (DocID -> DocMeta)
  - index snapshot (optional) or an append-only update log

**Visited persistence (required for resumability):**
- Persist the visited set as an append-only file `visited_urls.data`.
- Format: one normalized URL per line (UTF-8), e.g.
  - `https://example.com/page1`
  - `https://example.com/page2`
- Checkpoint behavior:
  - On checkpoint, flush newly visited URLs to `visited_urls.data`.
  - On restart, load `visited_urls.data` into the in-memory visited structure before resuming scheduling.
- Correctness requirement:
  - Loading the file must be idempotent (duplicate lines must not break behavior).
  - URL normalization rules used for this file must match the crawler’s runtime normalization.

### 12.2 Crash Recovery Workflow
1. On restart:
   - load last checkpoint
   - validate schema/version
2. Resume crawling:
   - restore frontier jobs
   - restore visited set to prevent re-crawl loops
3. Resume indexing:
   - if index snapshot exists, load it
   - if not, rebuild by replaying persisted fetch/index events (if recorded)

### 12.3 Consistency Model
- Prefer:
  - **Idempotent indexing** based on `URL -> DocID` mapping and token deduplication per doc.
- Checkpoint atomicity:
  - write to temp file, then rename (atomic on POSIX)

---

## 13. Acceptance Criteria
1. **Crawl correctness:** Starting from `origin_url`, pages are crawled to depth `k`.
2. **No data races:** System runs with concurrency without crashing or corrupting index.
3. **Live search:** Search endpoint returns results while crawling is active.
4. **Back-pressure:** System remains stable under load; queue depth is bounded by configuration.
5. **Search triple contract:** Results are triples `(relevant_url, origin_url, depth)` with accurate metadata.
6. **Dashboard:** Dashboard displays live metrics including queue depth and throttling status.
7. **Memory stability:** RAM usage remains bounded by enforcing max page size, releasing page bodies after processing, and periodically flushing/clearing the inverted index to disk.
8. **Bonus resumability:** Restart resumes with controlled duplication and without losing crawl state.

---

## 14. Risks and Mitigations
1. **Duplicate crawling due to URL normalization differences**
   - Mitigation: strict normalization + canonicalization rules.
2. **Index contention leading to slow crawling/search**
   - Mitigation: sharded index with per-shard locks; minimize lock hold time.
3. **Deadlocks caused by bounded channels**
   - Mitigation: carefully design producer/consumer flow; ensure workers can progress even when queues are full; use timeouts where needed.
4. **High memory usage from storing large pages**
   - Mitigation: enforce max page size, do not retain full HTML bodies beyond extraction/tokenization, and flush index segments to disk.
5. **Search returns incomplete results early**
   - Mitigation: define eventual consistency; display “indexed docs so far” on dashboard.
6. **Append-only disk index duplication**
   - Mitigation: treat disk segments as write-ahead logs and either deduplicate at query time or compact periodically.

---

## 15. Open Questions (To Resolve Before Implementation)
1. Should crawling be restricted to the same host/origin domain, or allow all discovered hosts?
2. Should `depth` represent shortest link distance (requires BFS semantics) or the scheduler traversal depth (simpler with job propagation)?
3. What ranking is expected by the course rubric: unordered match, TF-based scoring, or something else?
4. Are `robots.txt` and crawl politeness mandatory for the assignment?
5. Persistence expectations: is it enough to resume crawl frontier/visited, or must the index be restored too?

