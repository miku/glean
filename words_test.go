package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "words")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadWords(t *testing.T) {
	// web2 in miniature: proper nouns, accented and apostrophised entries,
	// single letters, and one word of the right shape.
	dict := writeTemp(t, "a\nAaron\nzebra\nZulu\ncafé\nO'Brien\nwell-being\nmango\nmango\nsupercalifragilistic\n")

	got, err := readWords(wordOpts{path: dict, min: 4, max: 8})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"mango", "zebra"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("readWords = %v, want %v", got, want)
	}

	// -proper lowercases capitalized entries and keeps them.
	got, err = readWords(wordOpts{path: dict, min: 4, max: 8, proper: true})
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"aaron", "mango", "zebra", "zulu"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("readWords(proper) = %v, want %v", got, want)
	}
}

func TestIsCandidate(t *testing.T) {
	tests := []struct {
		w    string
		want bool
	}{
		{"mango", true},
		{"abcd", true},
		{"abc", false},        // below min
		{"abcdefghi", false},  // above max
		{"Mango", false},      // capitalized
		{"well-being", false}, // hyphenated: the halves are better domains
		{"café", false},       // not a registrable label as written
		{"a1b2", false},       // digits are legal in a label but not words
		{"", false},
	}
	for _, tt := range tests {
		if got := isCandidate(tt.w, 4, 8); got != tt.want {
			t.Errorf("isCandidate(%q) = %v, want %v", tt.w, got, tt.want)
		}
	}
}

func TestReadLines(t *testing.T) {
	path := writeTemp(t, "# a curated list\nDelta\n\n  sigma  \ndelta\n# trailing comment\nomega\n")
	got, err := readLines(path)
	if err != nil {
		t.Fatal(err)
	}
	// Order is preserved (a curated list is curated in order), comments and
	// blanks dropped, case folded, duplicates removed.
	want := []string{"delta", "sigma", "omega"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("readLines = %v, want %v", got, want)
	}
}

func TestReadWordsMissingDict(t *testing.T) {
	if _, err := readWords(wordOpts{path: "/nonexistent/dict", min: 4, max: 8}); err == nil {
		t.Error("expected an error for a missing dictionary")
	}
}
