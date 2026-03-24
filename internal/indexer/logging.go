package indexer

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
)

var (
	loggerMu sync.Mutex
)

// SetupLogger configures the standard library logger to write to data/log/*.log.
// If alsoStdout is true, logs are written to both the file and stdout.
//
// Call this once from main() early.
func SetupLogger(baseDir string, alsoStdout bool) (*os.File, error) {
	loggerMu.Lock()
	defer loggerMu.Unlock()

	if baseDir == "" {
		baseDir = "."
	}
	logDir := filepath.Join(baseDir, "data", "log")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, err
	}

	f, err := os.OpenFile(filepath.Join(logDir, "crawler.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}

	var w io.Writer = f
	if alsoStdout {
		w = io.MultiWriter(os.Stdout, f)
	}

	log.SetOutput(w)
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds | log.LUTC)
	return f, nil
}
