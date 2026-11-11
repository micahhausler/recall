package config

import (
	"github.com/pelletier/go-toml/v2/unstable"
)

// keyLines maps each dotted leaf key in a TOML document to the 1-based line
// its key appears on. Best effort: a key it can't place maps to 0, and the
// caller treats that as "line unknown". The document must already have parsed
// cleanly with toml.Unmarshal.
func keyLines(data []byte) map[string]int {
	lines := map[string]int{}
	p := &unstable.Parser{}
	p.Reset(data)
	var prefix []string
	for p.NextExpression() {
		e := p.Expression()
		switch e.Kind {
		case unstable.Table, unstable.ArrayTable:
			prefix = keyParts(e.Key())
		case unstable.KeyValue:
			key := append(append([]string{}, prefix...), keyParts(e.Key())...)
			recordLines(lines, key, e, p)
		}
	}
	return lines
}

func keyParts(it unstable.Iterator) []string {
	var parts []string
	for it.Next() {
		parts = append(parts, string(it.Node().Data))
	}
	return parts
}

// recordLines records kv's key line, recursing into inline tables. The line
// comes from the key node: container values (arrays, inline tables) carry no
// raw range of their own, and the key is the better place to point anyway.
func recordLines(lines map[string]int, key []string, kv *unstable.Node, p *unstable.Parser) {
	if value := kv.Value(); value.Kind == unstable.InlineTable {
		it := value.Children()
		for it.Next() {
			sub := it.Node()
			if sub.Kind != unstable.KeyValue {
				continue
			}
			subKey := append(append([]string{}, key...), keyParts(sub.Key())...)
			recordLines(lines, subKey, sub, p)
		}
		return
	}
	keyIt := kv.Key()
	line := 0
	if keyIt.Next() {
		line = p.Shape(keyIt.Node().Raw).Start.Line
	}
	k := ""
	for i, part := range key {
		if i > 0 {
			k += "."
		}
		k += part
	}
	// First writer wins within one file; TOML forbids duplicate keys anyway.
	if _, ok := lines[k]; !ok {
		lines[k] = line
	}
}
