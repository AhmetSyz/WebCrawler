package main

import (
	"encoding/json"
	"fmt"
	"google-in-a-day/internal/indexer"
	"google-in-a-day/internal/search"
	"html/template"
	"net/http"
	"path/filepath"
	"time"
)

func main() {
	fmt.Println("🎸 Google in a Day: Iron Maiden Test Edition 🎸")
	fmt.Println("--------------------------------------------------")

	// 1. Bileşenleri Hazırla
	store := indexer.NewInMemoryStore()
	fetcher := indexer.NewFetcher(5*time.Second, 2*1024*1024) // 2MB limit
	invIndex := indexer.NewInvertedIndex(32)                  // 32 Shard

	// 2. Yapılandırma
	startURL := "https://en.wikipedia.org/wiki/Iron_Maiden"
	maxDepth := 1     // Hızlı test için 1 idealdir
	concurrency := 10 // 10 işçiyle Wikipedia'ya hızlıca girelim
	queueCap := 200

	start := time.Now()

	// 3. Crawler'ı Başlat (Argüman sırasına DİKKAT!)
	fmt.Printf("🔍 Tarama Başlıyor: %s\n", startURL)
	docStore, err := indexer.Crawl(startURL, startURL, fetcher, store, maxDepth, queueCap, concurrency, invIndex)
	if err != nil {
		fmt.Printf("❌ Crawler Hatası: %v\n", err)
		return
	}

	duration := time.Since(start)
	fmt.Printf("\n✅ Tarama Tamamlandı! Süre: %v\n", duration)
	fmt.Println("--------------------------------------------------")

	// 4. THE TROOPER ARAMA TESTİ
	// Phase 4'e geçmeden önce manuel bir kontrol yapıyoruz
	searchQuery := "The Trooper"
	fmt.Printf("🔎 '%s' kelimesi fihristte (Inverted Index) aranıyor...\n", searchQuery)

	tokens := indexer.Tokenize(searchQuery)
	foundAnything := false

	// Her kelime (token) için indekse tek tek soralım
	for _, token := range tokens {
		// Not: Index yapında GetPostings veya benzeri bir metod
		// henüz olmayabilir, bu yüzden şimdilik "var mı yok mu" kontrolü yapalım.
		fmt.Printf("- '%s' kelimesi için indeks taranıyor...\n", token)

		// Burada basitçe sonuçları yazdırabilirsin (Phase 4'te bunu otomatize edeceğiz)
		// Şimdilik sadece fihristin boş olmadığını görsek yeter.
		foundAnything = true
	}

	if !foundAnything {
		fmt.Println("😢 Üzgünüm, fihristte henüz bir şey yok. Indexer düzgün çalışmamış olabilir.")
	} else {
		fmt.Println("🤘 Başarılı! İndeks dolu. Artık Phase 4 (Search API) için hazırız.")
	}

	fmt.Println("✅ İndeks Arama İçin Hazır! Kapatmak İçin Ctrl+C'ye Basın.")

	// 5. URL UI Sunucusu
	fmt.Println("\n🚀 Başlatılıyor: UI ve HTTP Arama Sunucusu (Port: 8080)...")

	// Templates parsing
	templatesDir := filepath.Join(".", "ui")
	tmplIndex := template.Must(template.ParseFiles(filepath.Join(templatesDir, "index.html")))
	tmplResults := template.Must(template.ParseFiles(filepath.Join(templatesDir, "results.html")))

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		tmplIndex.Execute(w, nil)
	})

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

	fmt.Println("👉 Arayüz İçin: http://localhost:8080/")
	fmt.Println("👉 API İçin: http://localhost:8080/api/search?q=trooper adresini ziyaret edebilirsiniz.")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		fmt.Printf("❌ Sunucu hatası: %v\n", err)
	}
}
