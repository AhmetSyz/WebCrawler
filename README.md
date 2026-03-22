# Google in a Day (Go WebCrawler + Search)

Go-native concurrent web crawler + inverted-index search engine with a small control-plane dashboard.

## Features
- Concurrent crawling with bounded frontier queue (back-pressure)
- Strict `max_urls` limit and cancellation-based shutdown
- Memory-aware processing (do not retain page bodies after extraction/tokenization)
- Streaming HTML text extraction/tokenization
- Incremental disk flushing of the inverted index to `storage/*.data`
- Visited URL export to `visited_urls.data`
- Dashboard UI (config / status / search)

## Project structure
- `cmd/crawler/main.go`: HTTP server + dashboard APIs
- `internal/indexer/*`: crawler, store (visited), inverted index, disk flushing
- `internal/search/searcher.go`: search implementation
- `ui/index.html`: single-page dashboard
- `storage/*.data`: on-disk inverted-index shards
- `visited_urls.data`: visited URL output (one URL per line)

## Run
1. Build:
   - `go build ./...`
2. Start server:
   - `go run ./cmd/crawler`
3. Open dashboard:
   - http://localhost:8080/

## HTTP API
- `POST /api/crawl`
  - Body JSON:
    - `startURL` (string)
    - `maxDepth` (int)
    - `maxConcurrency` (int)
    - `queueCapacity` (int)
    - `maxURLs` (int)
- `GET /api/status` → crawl status JSON (running, pages crawled, progress, recent URLs, elapsed)
- `GET /api/search?q=...` → JSON results

## Notes
- The disk index format is append-only and may contain duplicates across flushes; compaction/dedup is a future improvement.
- `visited_urls.data` is generated after a successful crawl.
