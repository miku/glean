package main

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strings"
)

// defaultDict is the system dictionary, used by -dict and by the no-sources.d
// fallback. Note that it is not the same file everywhere: on the BSDs and
// macOS it is web2, on Debian it is whichever "wordlist" alternative is
// installed. For a list that does not change between machines, use the
// compiled-in web2 -- see web2gz in sources.go.
//
// "The wordlist makes a dandy 'grep' victim." -- share/dict/README, 1993.
const defaultDict = "/usr/share/dict/words"

// wordOpts controls which dictionary entries become candidates.
type wordOpts struct {
	path   string
	min    int
	max    int
	proper bool // lowercase and keep capitalized entries (proper nouns)
}

// readWords returns the deduplicated, sorted candidate list.
//
// The default filter is deliberately blunt: entries that are already lowercase
// ASCII of the right length. That drops proper nouns, which is mostly what you
// want -- web2 is full of them and they make poor generic domains -- along with
// the handful of accented and apostrophised entries, which are not registrable
// as written anyway.
func readWords(o wordOpts) ([]string, error) {
	f, err := os.Open(o.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	seen := make(map[string]bool, 1<<17)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	for sc.Scan() {
		w := strings.TrimSpace(sc.Text())
		if w == "" {
			continue
		}
		if o.proper {
			w = strings.ToLower(w)
		}
		if !isCandidate(w, o.min, o.max) {
			continue
		}
		seen[w] = true
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", o.path, err)
	}
	out := make([]string, 0, len(seen))
	for w := range seen {
		out = append(out, w)
	}
	sort.Strings(out)
	return out, nil
}

// isCandidate accepts a bare lowercase ASCII label of the right length. No
// hyphens: a hyphenated dictionary entry ("well-being") is a legal domain
// label but a worse one than either half, and web2's hyphenated list is a
// separate file anyway.
func isCandidate(w string, min, max int) bool {
	if len(w) < min || len(w) > max {
		return false
	}
	for i := 0; i < len(w); i++ {
		if w[i] < 'a' || w[i] > 'z' {
			return false
		}
	}
	return true
}

// readLines reads an explicit wordlist: one word per line, blanks and #
// comments ignored. Used for -w, so a curated list can replace the dictionary.
func readLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []string
	seen := make(map[string]bool)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	for sc.Scan() {
		w := strings.TrimSpace(sc.Text())
		if w == "" || strings.HasPrefix(w, "#") {
			continue
		}
		w = strings.ToLower(w)
		if !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	return out, sc.Err()
}
