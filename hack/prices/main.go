// Command prices refreshes internal/config/prices.json from each provider's
// public pricing page with one model call through recall's own headless
// runner. It writes the file only when the answer passes the table's checks.
//
// Run from the repo root: go run ./hack/prices
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/micahhausler/recall/internal/config"
	"github.com/micahhausler/recall/internal/model"
	"github.com/micahhausler/recall/internal/model/runners"
)

const prompt = `You update a model price table from providers' public pricing pages.

Return the table below with every provider's per-model prices and "checked" date filled in from its page, as one JSON object and nothing else.
Keep every provider, its "name", "runner", and "source" exactly as given, and every model key, which is the name recall passes to that runner's CLI ("sonnet" is the current Sonnet model, and so on).
A model priced in tokens has "input_usd_per_mtok" and "output_usd_per_mtok", USD per million tokens at the base rate, with no caching or batch discount.
A model priced in credits has only "credits_per_call"; a provider with no "source" stays as it is.
"note" names the model version the price is for, and any limit the price holds under.
Set "checked" to %s for each provider you read a page for.

Schema: {"providers": [{"name": string, "runner": string, "source": string (optional), "checked": "YYYY-MM-DD", "models": {"<name>": {"input_usd_per_mtok": number, "output_usd_per_mtok": number, "credits_per_call": number, "note": string}}}]}

# CURRENT TABLE

%s
`

func main() {
	file := flag.String("file", "internal/config/prices.json", "the price table to refresh")
	runner := flag.String("runner", config.HarnessClaude, "the runner to call")
	modelName := flag.String("model", "", "the model to ask (default: the runner's default reduce model)")
	page := flag.String("page", "", "a saved copy of the pricing page, instead of fetching it")
	newModels := flag.Bool("new-models", false, "keep models the answer adds")
	flag.Parse()
	ctx := context.Background()
	current, err := os.ReadFile(*file)
	if err != nil {
		fail(err)
	}
	old, err := config.ParsePrices(current)
	if err != nil {
		fail(err)
	}
	pages := map[string]string{}
	for _, p := range old.Providers {
		switch {
		case p.Source == "":
		case *page != "":
			b, err := os.ReadFile(*page)
			if err != nil {
				fail(err)
			}
			pages[p.Source] = pageText(string(b))
		default:
			text, err := fetch(ctx, p.Source)
			if err != nil {
				fail(err)
			}
			pages[p.Source] = text
		}
	}
	r, err := runners.Open(*runner)
	if err != nil {
		fail(err)
	}
	call := model.Call{Model: modelFor(*runner, *modelName)}
	out, err := refresh(ctx, r, call, current, pages, time.Now(), *newModels)
	if err != nil {
		fail(err)
	}
	next, _ := config.ParsePrices(out)
	diff := changes(old, next)
	if len(diff) == 0 {
		fmt.Println("prices: no change")
	}
	for _, line := range diff {
		fmt.Println(line)
	}
	if err := os.WriteFile(*file, out, 0o644); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "prices: %v\n", err)
	os.Exit(1)
}

// refresh asks the model for the filled table and returns it formatted,
// or an error when the answer fails the table's checks.
func refresh(ctx context.Context, r model.Runner, call model.Call, current []byte, pages map[string]string, now time.Time, allowNew bool) ([]byte, error) {
	old, err := config.ParsePrices(current)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, prompt, now.Format(time.DateOnly), current)
	for _, p := range old.Providers {
		if text, ok := pages[p.Source]; ok {
			fmt.Fprintf(&b, "\n# PAGE %s\n\n%s\n", p.Source, text)
		}
	}
	answer, _, err := r.Run(ctx, call, b.String())
	if err != nil {
		return nil, err
	}
	next, err := config.ParsePrices([]byte(jsonOf(answer)))
	if err != nil {
		return nil, fmt.Errorf("the answer: %w", err)
	}
	if err := config.CheckPriceUpdate(old, next, now, allowNew); err != nil {
		return nil, fmt.Errorf("the answer: %w", err)
	}
	return format(next)
}

// jsonOf is the answer without a code fence around it.
func jsonOf(answer string) string {
	s := strings.TrimSpace(answer)
	if strings.HasPrefix(s, "```") {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	}
	return s
}

func format(t *config.PriceTable) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(t); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// changes lists each price that differs, one line per model.
func changes(old, next *config.PriceTable) []string {
	var out []string
	for i, n := range next.Providers {
		o := old.Providers[i]
		if o.Checked != n.Checked {
			out = append(out, fmt.Sprintf("%s: checked %s -> %s", n.Name, o.Checked, n.Checked))
		}
		for name, nm := range n.Models {
			om, ok := o.Models[name]
			switch {
			case !ok:
				out = append(out, fmt.Sprintf("%s %s: added %s", n.Name, name, priceString(nm)))
			case om != nm:
				out = append(out, fmt.Sprintf("%s %s: %s -> %s", n.Name, name, priceString(om), priceString(nm)))
			}
		}
	}
	sort.Strings(out)
	return out
}

func priceString(m config.ModelPrice) string {
	if m.CreditsPerCall > 0 {
		return fmt.Sprintf("%g credits per call (%s)", m.CreditsPerCall, m.Note)
	}
	return fmt.Sprintf("$%g in / $%g out per MTok (%s)", m.InputUSDPerMTok, m.OutputUSDPerMTok, m.Note)
}

func fetch(ctx context.Context, url string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{CheckRedirect: sameHost(req.URL)}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", url, resp.Status)
	}
	return readPage(resp.Body)
}

// maxPage is the most of a page refresh sends.
const maxPage = 4 << 20

// readPage is a page's text, or an error for a page over maxPage rather
// than a truncated one.
func readPage(r io.Reader) (string, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxPage+1))
	if err != nil {
		return "", err
	}
	if len(body) > maxPage {
		return "", fmt.Errorf("the page is over %d bytes", maxPage)
	}
	return pageText(string(body)), nil
}

// sameHost follows a redirect only to https on the source's own host.
func sameHost(source *neturl.URL) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("%s: too many redirects", source)
		}
		if req.URL.Scheme != "https" || req.URL.Host != source.Host {
			return fmt.Errorf("%s redirects to %s, off https on %s", source, req.URL, source.Host)
		}
		return nil
	}
}

// modelFor is the model to ask: the flag's, or the runner's reduce default.
func modelFor(runner, flagModel string) string {
	if flagModel != "" {
		return flagModel
	}
	_, reduce, _ := config.DefaultModels(runner)
	return reduce
}

var (
	dropBlocks = regexp.MustCompile(`(?is)<(script|style|svg|head)\b.*?</(script|style|svg|head)>`)
	tags       = regexp.MustCompile(`(?s)<[^>]*>`)
	blankRuns  = regexp.MustCompile(`\n\s*\n+`)
)

// pageText is a page's visible text, roughly.
func pageText(page string) string {
	s := dropBlocks.ReplaceAllString(page, " ")
	s = tags.ReplaceAllString(s, "\n")
	s = html.UnescapeString(s)
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		lines = append(lines, strings.Join(strings.Fields(l), " "))
	}
	return strings.TrimSpace(blankRuns.ReplaceAllString(strings.Join(lines, "\n"), "\n"))
}
