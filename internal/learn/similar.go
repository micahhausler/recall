package learn

import (
	"math"
	"strings"
	"unicode"
)

// stopwords carry no meaning for matching two sentences about the same
// thing.
var stopwords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true, "be": true,
	"but": true, "by": true, "for": true, "from": true, "has": true, "have": true, "in": true,
	"into": true, "is": true, "it": true, "its": true, "of": true, "on": true, "or": true,
	"so": true, "than": true, "that": true, "the": true, "their": true, "then": true,
	"there": true, "these": true, "this": true, "to": true, "was": true, "were": true,
	"when": true, "which": true, "who": true, "will": true, "with": true,
}

// tokens is the set of meaningful words in text: lowercased, punctuation
// dropped, stopwords removed, and common English endings stripped so
// "pushes", "pushed", and "pushing" all match "push".
func tokens(text string) map[string]bool {
	set := map[string]bool{}
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	for _, w := range words {
		if stopwords[w] {
			continue
		}
		if w = stem(w); w != "" {
			set[w] = true
		}
	}
	return set
}

// stem strips one common ending from a word long enough to survive it.
func stem(w string) string {
	if len(w) <= 4 {
		return w
	}
	for _, suffix := range []string{"ing", "ed", "es", "s"} {
		if rest, ok := strings.CutSuffix(w, suffix); ok && len(rest) >= 3 {
			return rest
		}
	}
	return w
}

// jaccard is how much two token sets overlap, 0 to 1.
func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	shared := 0
	for t := range a {
		if b[t] {
			shared++
		}
	}
	return float64(shared) / float64(len(a)+len(b)-shared)
}

// similarity is how alike two sentences are by their words, 0 to 1.
func similarity(a, b string) float64 {
	return jaccard(tokens(a), tokens(b))
}

// rarity weighs each word by how rare it is across a set of texts, so two
// sentences that share an unusual word ("zsh", a file name) count as more
// alike than two that share only common ones ("owner", "file").
type rarity struct {
	texts float64
	seen  map[string]int
}

func newRarity(texts []map[string]bool) rarity {
	r := rarity{texts: float64(len(texts)), seen: map[string]int{}}
	for _, t := range texts {
		for w := range t {
			r.seen[w]++
		}
	}
	return r
}

func (r rarity) weight(word string) float64 {
	seen := r.seen[word]
	if seen == 0 {
		seen = 1
	}
	return math.Log(1 + r.texts/float64(seen))
}

// similarity is the weighted overlap of two token sets, 0 to 1: the weight
// of the words they share over the weight of all their words.
func (r rarity) similarity(a, b map[string]bool) float64 {
	var shared, all float64
	for w := range a {
		if b[w] {
			shared += r.weight(w)
		}
		all += r.weight(w)
	}
	for w := range b {
		if !a[w] {
			all += r.weight(w)
		}
	}
	if all == 0 {
		return 0
	}
	return shared / all
}
