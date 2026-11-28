package config

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"time"
)

// pricesJSON is the model price table; docs/learning.md says how to refresh it.
//
//go:embed prices.json
var pricesJSON []byte

// PricesJSON is the embedded price table as written.
func PricesJSON() []byte { return slices.Clone(pricesJSON) }

// PriceTable is every provider's public model prices.
type PriceTable struct {
	Providers []PriceProvider `json:"providers"`
}

// PriceProvider is one provider's prices for the models one runner names.
type PriceProvider struct {
	Name string `json:"name"`
	// Runner is the learn.runner whose CLI takes these model names.
	Runner string `json:"runner"`
	// Source is the public pricing page; empty for recall's own estimate.
	Source  string                `json:"source,omitempty"`
	Checked string                `json:"checked"`
	Models  map[string]ModelPrice `json:"models"`
}

// ModelPrice is one model's price: USD per million tokens, or credits per call.
type ModelPrice struct {
	InputUSDPerMTok  float64 `json:"input_usd_per_mtok,omitempty"`
	OutputUSDPerMTok float64 `json:"output_usd_per_mtok,omitempty"`
	CreditsPerCall   float64 `json:"credits_per_call,omitempty"`
	Note             string  `json:"note,omitempty"`
}

// Observe-call estimate inputs.
const (
	// CharsPerToken is about how many characters of a rendered session make one token.
	CharsPerToken = 4
	// ObserveOutputTokens is the output allowed for one observe call.
	ObserveOutputTokens = 1500
)

// Cost is about what calls model calls sending inputChars characters in all
// cost, in unit, or in dollars when unit is empty.
func (p ModelPrice) Cost(calls, inputChars int) (cost float64, unit string) {
	if p.CreditsPerCall > 0 {
		return p.CreditsPerCall * float64(calls), "credits"
	}
	in := float64(inputChars) / CharsPerToken
	out := float64(calls * ObserveOutputTokens)
	return (in*p.InputUSDPerMTok + out*p.OutputUSDPerMTok) / 1e6, ""
}

var prices = func() *PriceTable {
	t, err := ParsePrices(pricesJSON)
	if err != nil {
		panic(err)
	}
	return t
}()

// Prices is the embedded price table.
func Prices() *PriceTable { return prices }

// Lookup is the price of model through runner's CLI.
func (t *PriceTable) Lookup(runner, model string) (ModelPrice, bool) {
	for _, p := range t.Providers {
		if p.Runner == runner {
			if m, ok := p.Models[model]; ok {
				return m, true
			}
		}
	}
	return ModelPrice{}, false
}

// Checked is the date the provider pricing model through runner's CLI was
// checked, or "" when nothing prices it.
func (t *PriceTable) Checked(runner, model string) string {
	for _, p := range t.Providers {
		if _, ok := p.Models[model]; ok && p.Runner == runner {
			return p.Checked
		}
	}
	return ""
}

// ParsePrices reads a price table and checks it: known runners, dated,
// a source that is an https URL, a positive price for every model, and
// every default model of every known harness priced.
func ParsePrices(data []byte) (*PriceTable, error) {
	if err := strictKeys(data); err != nil {
		return nil, fmt.Errorf("prices: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var t PriceTable
	if err := dec.Decode(&t); err != nil {
		return nil, fmt.Errorf("prices: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("prices: trailing data")
	}
	for _, p := range t.Providers {
		if _, ok := defaultsFor(p.Runner); !ok {
			return nil, fmt.Errorf("prices: provider %q: unknown runner %q", p.Name, p.Runner)
		}
		if _, err := time.Parse(time.DateOnly, p.Checked); err != nil {
			return nil, fmt.Errorf("prices: provider %q: checked %q is not a date", p.Name, p.Checked)
		}
		if p.Source != "" {
			if u, err := url.Parse(p.Source); err != nil || u.Scheme != "https" || u.Host == "" {
				return nil, fmt.Errorf("prices: provider %q: source %q is not an https URL", p.Name, p.Source)
			}
		}
		names := make([]string, 0, len(p.Models))
		for name := range p.Models {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			m := p.Models[name]
			tokens := m.InputUSDPerMTok > 0 && m.OutputUSDPerMTok > 0 && m.CreditsPerCall == 0
			credits := m.CreditsPerCall > 0 && m.InputUSDPerMTok == 0 && m.OutputUSDPerMTok == 0
			if !tokens && !credits {
				return nil, fmt.Errorf("prices: provider %q model %q: want positive input and output USD per MTok, or positive credits per call", p.Name, name)
			}
			if len(m.Note) > maxNote {
				return nil, fmt.Errorf("prices: provider %q model %q: note is over %d bytes", p.Name, name, maxNote)
			}
		}
	}
	for _, h := range harnesses {
		for _, m := range []string{h.mapModel, h.reduceModel, h.applyModel} {
			if _, ok := t.Lookup(h.name, m); !ok {
				return nil, fmt.Errorf("prices: no price for %s's default model %q", h.name, m)
			}
		}
	}
	return &t, nil
}

// maxNote caps a model's note.
const maxNote = 200

// Bounds on a refreshed price, as a multiple of the old one.
const (
	minPriceMove = 0.1
	maxPriceMove = 10
)

// CheckPriceUpdate refuses a refreshed table that changes what only a
// person should: a provider's name, runner, or source, any part of a
// provider with no source, a model it priced, a model it didn't (unless
// allowNew), or a price by more than 10x either way. Each provider with a
// source must be checked today, and today can't be earlier than the old
// table's date.
func CheckPriceUpdate(old, next *PriceTable, today time.Time, allowNew bool) error {
	day := today.Format(time.DateOnly)
	if len(old.Providers) != len(next.Providers) {
		return fmt.Errorf("prices: %d providers, want %d", len(next.Providers), len(old.Providers))
	}
	for i, o := range old.Providers {
		n := next.Providers[i]
		if n.Name != o.Name || n.Runner != o.Runner {
			return fmt.Errorf("prices: provider %d is %s/%s, want %s/%s", i, n.Name, n.Runner, o.Name, o.Runner)
		}
		if n.Source != o.Source {
			return fmt.Errorf("prices: provider %q: source changed from %q to %q", o.Name, o.Source, n.Source)
		}
		if o.Source == "" {
			ob, _ := json.Marshal(o)
			nb, _ := json.Marshal(n)
			if !bytes.Equal(ob, nb) {
				return fmt.Errorf("prices: provider %q has no source, so only a hand edit changes it", o.Name)
			}
			continue
		}
		if day < o.Checked {
			return fmt.Errorf("prices: provider %q: today, %s, is earlier than its checked date %s", o.Name, day, o.Checked)
		}
		if n.Checked != day {
			return fmt.Errorf("prices: provider %q: checked is %s, want today, %s", o.Name, n.Checked, day)
		}
		for name := range o.Models {
			if _, ok := n.Models[name]; !ok {
				return fmt.Errorf("prices: provider %q dropped model %q", o.Name, name)
			}
		}
		names := make([]string, 0, len(n.Models))
		for name := range n.Models {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			om, ok := o.Models[name]
			if !ok {
				if !allowNew {
					return fmt.Errorf("prices: provider %q added model %q; allow new models to keep it", o.Name, name)
				}
				continue
			}
			nm := n.Models[name]
			for _, f := range []struct {
				field    string
				was, now float64
			}{
				{"input_usd_per_mtok", om.InputUSDPerMTok, nm.InputUSDPerMTok},
				{"output_usd_per_mtok", om.OutputUSDPerMTok, nm.OutputUSDPerMTok},
				{"credits_per_call", om.CreditsPerCall, nm.CreditsPerCall},
			} {
				if f.was == 0 && f.now == 0 {
					continue
				}
				if f.was == 0 || f.now == 0 || f.now < f.was*minPriceMove || f.now > f.was*maxPriceMove {
					return fmt.Errorf("prices: provider %q model %q: %s moves from %g to %g, past %gx to %gx; make a move that large by hand in prices.json",
						o.Name, name, f.field, f.was, f.now, minPriceMove, float64(maxPriceMove))
				}
			}
		}
	}
	return nil
}

// strictKeys refuses an object key that isn't a field's exact JSON name,
// and any key an object repeats; encoding/json allows both.
func strictKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	return walkKeys(dec, priceShape)
}

// shape is what one JSON value may hold: object fields by exact name, any
// key's value when any is set, or the elements of an array.
type shape struct {
	fields map[string]*shape
	any    *shape
	elem   *shape
}

var priceShape = func() *shape {
	scalar := &shape{}
	model := &shape{fields: map[string]*shape{"input_usd_per_mtok": scalar, "output_usd_per_mtok": scalar, "credits_per_call": scalar, "note": scalar}}
	provider := &shape{fields: map[string]*shape{"name": scalar, "runner": scalar, "source": scalar, "checked": scalar, "models": {any: model}}}
	return &shape{fields: map[string]*shape{"providers": {elem: provider}}}
}()

func walkKeys(dec *json.Decoder, sh *shape) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch tok {
	case json.Delim('{'):
		seen := map[string]bool{}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return err
			}
			key, _ := kt.(string)
			if seen[key] {
				return fmt.Errorf("key %q appears twice", key)
			}
			seen[key] = true
			next := sh.any
			if next == nil {
				next = sh.fields[key]
			}
			if next == nil {
				return fmt.Errorf("unknown key %q", key)
			}
			if err := walkKeys(dec, next); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	case json.Delim('['):
		for dec.More() {
			elem := sh.elem
			if elem == nil {
				elem = &shape{}
			}
			if err := walkKeys(dec, elem); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	}
	return nil
}
