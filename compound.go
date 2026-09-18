package main

import (
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// commonTxt is about 1250 common English words in rough order of frequency,
// with the function words taken out ("of", "and", "is") and a few that make
// domains more than they make sentences put back in ("my", "get", "go").
// Order matters: the compound generator pairs the first words with each other
// first.
//
//go:embed common.txt
var commonTxt []byte

// compound is the generator for two-word names: "word" + "cloud",
// "link" + "tree". It pairs every word of one list with every word of
// another, or of the same list twice.
//
//	# generate: compound common
//	# generate: compound adjectives.txt nouns.txt
//
// A list is a built-in name or a file, relative to sources.d like include:.
//
// The pairs come out ordered by the sum of the two ranks, not
// lexically: every pair of the first ten words before any pair involving the
// thousandth. A lexical walk would spend a short "scan -n" budget on
// "able"-anything; this spends it on the pairs made of the most common words,
// which are both the likeliest to be taken and the most interesting when one
// is not.
//
// A label can be split more than one way -- "sunset" + "tle" is not a pair,
// but "starlight" + "house" and "star" + "lighthouse" both are -- and must
// still come out once. The pair with the shortest left word is the one that
// counts; the others are skipped when they come up.
type compound struct {
	left, right []string
	lset, rset  map[string]bool
	min, max    int // label length bounds, from the source's min: and max:

	once sync.Once
	n    int
}

// parseCompound reads the arguments to "compound": one list, or two.
func parseCompound(arg, dir string) (*compound, error) {
	names := strings.Fields(arg)
	if len(names) < 1 || len(names) > 2 {
		return nil, fmt.Errorf("compound: want one or two lists, got %q", arg)
	}
	left, err := readCompoundList(names[0], dir)
	if err != nil {
		return nil, err
	}
	right := left
	if len(names) == 2 {
		if right, err = readCompoundList(names[1], dir); err != nil {
			return nil, err
		}
	}
	c := &compound{left: left, right: right, min: 1, max: maxLabelLen}
	c.lset = toSet(left)
	c.rset = toSet(right)
	if n := c.bound(); n > maxGenerated {
		return nil, fmt.Errorf("compound %s: %d x %d is %d pairs, over the %d cap; shorten a list",
			arg, len(left), len(right), n, maxGenerated)
	}
	return c, nil
}

// readCompoundList returns a list's words, lowercased, deduplicated and in
// file order, which is the rank order.
func readCompoundList(name, dir string) ([]string, error) {
	var rc io.ReadCloser
	var err error
	if isBuiltin(name) {
		rc, err = openBuiltin(name)
	} else {
		p := name
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		rc, err = os.Open(p)
	}
	if err != nil {
		return nil, fmt.Errorf("compound: %w", err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("compound: %s: %w", name, err)
	}
	var out []string
	seen := make(map[string]bool)
	for _, line := range strings.Split(string(b), "\n") {
		w := strings.ToLower(strings.TrimSpace(line))
		if w == "" || strings.HasPrefix(w, "#") || seen[w] || !isCandidate(w, 1, maxLabelLen) {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("compound: %s has no usable words", name)
	}
	return out, nil
}

func toSet(ws []string) map[string]bool {
	m := make(map[string]bool, len(ws))
	for _, w := range ws {
		m[w] = true
	}
	return m
}

// bound is the most labels this could yield, before deduplication and length
// bounds: what the generator cap is checked against.
func (c *compound) bound() int { return len(c.left) * len(c.right) }

// canonical reports whether l+r is the split of its label that counts, which
// is also the whole of the membership test: a label belongs to the generator
// if and only if some split of it is canonical.
func (c *compound) canonical(l, r string) bool {
	if l == r {
		return false // "bye" + "bye": a stutter, not a name
	}
	w := l + r
	if len(w) < c.min || len(w) > c.max {
		return false
	}
	for p := 1; p < len(l); p++ {
		if c.valid(w[:p], w[p:]) {
			return false // a shorter left word makes the same label
		}
	}
	return true
}

func (c *compound) valid(l, r string) bool {
	return l != r && c.lset[l] && c.rset[r]
}

// each walks the pairs by rank sum: the anti-diagonals of the left x right
// grid, top-left corner first.
func (c *compound) each(fn func(string) bool) {
	nl, nr := len(c.left), len(c.right)
	for k := 0; k <= nl+nr-2; k++ {
		i := k - (nr - 1)
		if i < 0 {
			i = 0
		}
		for ; i < nl && i <= k; i++ {
			l, r := c.left[i], c.right[k-i]
			if !c.canonical(l, r) {
				continue
			}
			if !fn(l + r) {
				return
			}
		}
	}
}

// count enumerates, unlike the positional generators, because deduplication
// is not a formula. A million and a half short strings is a fraction of a
// second, and it is done once.
func (c *compound) count() int {
	c.once.Do(func() {
		c.each(func(string) bool {
			c.n++
			return true
		})
	})
	return c.n
}

func (c *compound) contains(w string) bool {
	if len(w) < c.min || len(w) > c.max {
		return false
	}
	for p := 1; p < len(w); p++ {
		if c.valid(w[:p], w[p:]) {
			return true
		}
	}
	return false
}
