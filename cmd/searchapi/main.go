package main

import (
	"fmt"
	"google-in-a-day/internal/api"
	"net/http"
	"time"
)

func main() {
	srv, err := api.NewServerFromPData(".")
	if err != nil {
		// Missing/corrupted file should be surfaced clearly.
		panic(err)
	}

	httpSrv := &http.Server{
		Addr:              ":3600",
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	fmt.Println("Search API listening on http://localhost:3600/search")
	fmt.Println("Example: /search?query=python&sortBy=relevance")
	if err := httpSrv.ListenAndServe(); err != nil {
		panic(err)
	}
}
