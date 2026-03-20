package indexer

import (
	"strings"
	"unicode"
)

// Tokenize converts an HTML body to lowercase, removes punctuation, and splits into words.
// It also filters out very short tokens (length < 3) to keep the index cleaner.
func Tokenize(body string) []string {
	// Normalize to lowercase first.
	s := strings.ToLower(body)

	// Replace punctuation/symbols with spaces so we can split on whitespace later.
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		// Keep letters and digits; everything else becomes a separator.
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte(' ')
		}
	}

	words := strings.Fields(b.String())
	if len(words) == 0 {
		return nil
	}

	out := make([]string, 0, len(words))
	for _, w := range words {
		if len(w) < 3 {
			continue
		}
		out = append(out, w)
	}
	return out
}
