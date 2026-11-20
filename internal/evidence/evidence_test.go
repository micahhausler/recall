package evidence

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// fakeGH answers the one batched GraphQL call per repo from a map keyed by
// the repo slug, and fails the test on any other invocation: the source
// must make exactly one gh call per repo, and never fall back to a per-fact
// read. It also checks the query itself carries the load-bearing pieces:
// the viewer, update-time order, the trailer's commit bodies, the inline
// review threads, and the check rollup.
func fakeGH(t *testing.T, responses map[string]string) (run func(ctx context.Context, args ...string) ([]byte, error), calls *int) {
	calls = new(int)
	run = func(ctx context.Context, args ...string) ([]byte, error) {
		t.Helper()
		*calls++
		host := "github.com"
		var owner, name, query string
		graphql := false
		for i := 0; i < len(args); i++ {
			switch {
			case args[i] == "graphql":
				graphql = true
			case args[i] == "--hostname" && i+1 < len(args):
				i++
				if args[i] == "github.com" {
					t.Fatalf("--hostname passed for github.com: gh %s", strings.Join(args, " "))
				}
				host = args[i]
			case args[i] == "-f" && i+1 < len(args):
				i++
				query = strings.TrimPrefix(args[i], "query=")
			case args[i] == "-F" && i+1 < len(args):
				i++
				if v, ok := strings.CutPrefix(args[i], "owner="); ok {
					owner = v
				} else if v, ok := strings.CutPrefix(args[i], "name="); ok {
					name = v
				}
			}
		}
		if len(args) == 0 || args[0] != "api" || !graphql {
			t.Fatalf("not a graphql call: gh %s", strings.Join(args, " "))
		}
		for _, want := range []string{
			"viewer { login }",
			"orderBy: {field: UPDATED_AT, direction: DESC}",
			"messageBody",
			"reviewThreads",
			"statusCheckRollup",
		} {
			if !strings.Contains(query, want) {
				t.Fatalf("query lacks %q:\n%s", want, query)
			}
		}
		key := host + "/" + owner + "/" + name
		resp, ok := responses[key]
		if !ok {
			t.Fatalf("unexpected gh graphql call for %s", key)
		}
		return []byte(resp), nil
	}
	return run, calls
}

func TestGitHubItems(t *testing.T) {
	const repo = "github.com/you/example"
	// One response holds everything: #12 is the agent's work, #11 has no
	// trailer so its comment contributes nothing, and #3 was updated before
	// the window so even its trailer and comment are discarded.
	run, calls := fakeGH(t, map[string]string{
		repo: `{"data": {"viewer": {"login": "you"}, "repository": {"pullRequests": {"nodes": [
			{
				"number": 12, "title": "Fix the loop", "author": {"login": "you"},
				"updatedAt": "2026-11-10T10:00:00Z",
				"commits": {"nodes": [
					{"commit": {"messageBody": "Fix the loop\n\nCo-Authored-By: Claude <noreply@example.com>"}}
				]},
				"comments": {"nodes": [
					{"author": {"login": "you"}, "body": "use the injected clock here", "createdAt": "2026-11-10T09:00:00Z"},
					{"author": {"login": "reviewer"}, "body": "this re-reads the file every call", "createdAt": "2026-11-09T12:00:00Z"},
					{"author": {"login": "ci-bot[bot]"}, "body": "coverage report", "createdAt": "2026-11-09T12:30:00Z"}
				]},
				"reviews": {"nodes": [
					{"author": {"login": "reviewer"}, "body": "needs a test for the empty case", "submittedAt": "2026-11-09T13:00:00Z"},
					{"author": {"login": "reviewer"}, "body": "", "submittedAt": "2026-11-09T13:05:00Z"}
				]},
				"reviewThreads": {"nodes": [
					{"comments": {"nodes": [
						{"author": {"login": "reviewer"}, "body": "off by one at the boundary", "path": "a.go", "createdAt": "2026-11-10T08:00:00Z"}
					]}}
				]},
				"head": {"nodes": [{"commit": {"statusCheckRollup": {"contexts": {"nodes": [
					{"name": "test", "conclusion": "FAILURE", "completedAt": "2026-11-09T11:00:00Z"},
					{"name": "lint", "conclusion": "SUCCESS", "completedAt": "2026-11-09T11:00:00Z"},
					{"context": "ci/build", "state": "ERROR"}
				]}}}}]}
			},
			{
				"number": 11, "title": "Hand-written change", "author": {"login": "you"},
				"updatedAt": "2026-11-09T09:00:00Z",
				"commits": {"nodes": [{"commit": {"messageBody": "Hand-written change"}}]},
				"comments": {"nodes": [
					{"author": {"login": "reviewer"}, "body": "looks good", "createdAt": "2026-11-09T08:00:00Z"}
				]},
				"reviews": {"nodes": []},
				"reviewThreads": {"nodes": []},
				"head": {"nodes": [{"commit": {"statusCheckRollup": null}}]}
			},
			{
				"number": 3, "title": "Old work", "author": {"login": "you"},
				"updatedAt": "2026-01-01T00:00:00Z",
				"commits": {"nodes": [{"commit": {"messageBody": "x\n\nCo-Authored-By: Claude <noreply@example.com>"}}]},
				"comments": {"nodes": [
					{"author": {"login": "reviewer"}, "body": "before the window", "createdAt": "2026-01-01T00:00:00Z"}
				]},
				"reviews": {"nodes": []},
				"reviewThreads": {"nodes": []},
				"head": {"nodes": []}
			}
		]}}}}`,
	})
	g := &GitHub{Repos: []string{repo}, run: run}
	since := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	items, err := g.Items(context.Background(), since)
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 1 {
		t.Errorf("gh called %d times, want 1 per repo", *calls)
	}
	if len(items) != 6 {
		t.Fatalf("items = %d, want 6:\n%+v", len(items), items)
	}
	for i, it := range items {
		if it.Repo != repo || it.PR != repo+"#12" {
			t.Errorf("item %d tagged %q %q, want repo %q on PR #12", i, it.Repo, it.PR, repo)
		}
		if it.Owner != (it.Author == "you") {
			t.Errorf("item %d by %q has Owner = %v", i, it.Author, it.Owner)
		}
		switch it.Source {
		case SourceReview:
			if it.Author == "" {
				t.Errorf("review item %d has no author: %+v", i, it)
			}
		case SourceCI:
			if it.Author != "" || it.Owner {
				t.Errorf("ci item %d carries an author: %+v", i, it)
			}
		default:
			t.Errorf("item %d source = %q", i, it.Source)
		}
	}
	owner, reviews, cis := 0, 0, 0
	for _, it := range items {
		switch {
		case it.Owner:
			owner++
		case it.Source == SourceReview:
			reviews++
		case it.Source == SourceCI:
			cis++
		}
	}
	if owner != 1 || reviews != 3 || cis != 2 {
		t.Errorf("items = %d owner-authored, %d review, %d ci; want 1, 3, 2", owner, reviews, cis)
	}
	if items[5].Text != "check ci/build failed" || items[5].When.IsZero() {
		t.Errorf("a status with no completedAt should fall back to the pull request's update time: %+v", items[5])
	}
}

// A custom trailer (learn.evidence_trailer) marks agent-authored pull
// requests when the harness writes a different one; the default trailer
// then marks nothing.
func TestGitHubCustomTrailer(t *testing.T) {
	const repo = "github.com/you/example"
	src, err := Open("github", Options{Repos: []string{repo}, Trailer: "Agent: laptop-a"})
	if err != nil {
		t.Fatal(err)
	}
	g := src.(*GitHub)
	run, _ := fakeGH(t, map[string]string{
		repo: `{"data": {"viewer": {"login": "you"}, "repository": {"pullRequests": {"nodes": [
			{
				"number": 7, "title": "Fix the loop", "author": {"login": "you"},
				"updatedAt": "2026-11-10T10:00:00Z",
				"commits": {"nodes": [{"commit": {"messageBody": "Fix the loop\n\nAgent: laptop-a"}}]},
				"comments": {"nodes": [
					{"author": {"login": "reviewer"}, "body": "off by one", "createdAt": "2026-11-10T09:00:00Z"}
				]},
				"reviews": {"nodes": []},
				"reviewThreads": {"nodes": []},
				"head": {"nodes": []}
			},
			{
				"number": 6, "title": "x", "author": {"login": "you"},
				"updatedAt": "2026-11-10T09:00:00Z",
				"commits": {"nodes": [{"commit": {"messageBody": "x\n\nCo-Authored-By: Claude <noreply@example.com>"}}]},
				"comments": {"nodes": [
					{"author": {"login": "reviewer"}, "body": "skipped", "createdAt": "2026-11-10T08:00:00Z"}
				]},
				"reviews": {"nodes": []},
				"reviewThreads": {"nodes": []},
				"head": {"nodes": []}
			}
		]}}}}`,
	})
	g.run = run
	items, err := g.Items(context.Background(), time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].PR != repo+"#7" || items[0].Text != "off by one" {
		t.Fatalf("items = %+v, want only the comment on #7", items)
	}
}

// GitHub orders pull requests by creation date unless told otherwise, so an
// old pull request reviewed recently can fall beyond the page and its new
// comments would be silently missed. The query must ask for update-time
// order (fakeGH checks the orderBy clause), and an old-created, recently
// updated pull request must contribute its comments.
func TestGitHubPullsOldPRRecentlyUpdated(t *testing.T) {
	const repo = "github.com/you/example"
	run, _ := fakeGH(t, map[string]string{
		// Update order: #5, created long ago and reviewed after since,
		// comes first; the stale #9 follows.
		repo: `{"data": {"viewer": {"login": "you"}, "repository": {"pullRequests": {"nodes": [
			{
				"number": 5, "title": "Fix the loop", "author": {"login": "you"},
				"updatedAt": "2026-11-10T10:00:00Z",
				"commits": {"nodes": [{"commit": {"messageBody": "Fix the loop\n\nCo-Authored-By: Claude <noreply@example.com>"}}]},
				"comments": {"nodes": [
					{"author": {"login": "reviewer"}, "body": "stale comparison here", "createdAt": "2026-11-10T09:00:00Z"}
				]},
				"reviews": {"nodes": []},
				"reviewThreads": {"nodes": []},
				"head": {"nodes": []}
			},
			{
				"number": 9, "title": "Older", "author": {"login": "you"},
				"updatedAt": "2026-10-01T00:00:00Z",
				"commits": {"nodes": [{"commit": {"messageBody": "y\n\nCo-Authored-By: Claude <noreply@example.com>"}}]},
				"comments": {"nodes": []},
				"reviews": {"nodes": []},
				"reviewThreads": {"nodes": []},
				"head": {"nodes": []}
			}
		]}}}}`,
	})
	g := &GitHub{Repos: []string{repo}, run: run}
	items, err := g.Items(context.Background(), time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].PR != repo+"#5" || items[0].Text != "stale comparison here" {
		t.Fatalf("items = %+v, want the comment on old-created, recently updated #5", items)
	}
}

func TestGitHubWindow(t *testing.T) {
	// Every pull request is older than the window: nothing contributes. The
	// repo lives on another host, so the call carries --hostname (fakeGH
	// keys on it).
	const repo = "github.example.com/you/example"
	run, calls := fakeGH(t, map[string]string{
		repo: `{"data": {"viewer": {"login": "you"}, "repository": {"pullRequests": {"nodes": [
			{
				"number": 3, "title": "Old", "author": {"login": "you"},
				"updatedAt": "2026-01-01T00:00:00Z",
				"commits": {"nodes": [{"commit": {"messageBody": "x\n\nCo-Authored-By: Claude <noreply@example.com>"}}]},
				"comments": {"nodes": [
					{"author": {"login": "reviewer"}, "body": "before the window", "createdAt": "2026-01-01T00:00:00Z"}
				]},
				"reviews": {"nodes": []},
				"reviewThreads": {"nodes": []},
				"head": {"nodes": []}
			}
		]}}}}`,
	})
	g := &GitHub{Repos: []string{repo}, run: run}
	items, err := g.Items(context.Background(), time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	if err != nil || len(items) != 0 {
		t.Fatalf("items = %v, %v; want none", items, err)
	}
	if *calls != 1 {
		t.Errorf("gh called %d times, want 1 per repo", *calls)
	}
}

// One failing repo must not turn off evidence for the rest: its error is
// collected and named, and the other repos' items still come back.
func TestGitHubItemsSkipsFailingRepo(t *testing.T) {
	const bad = "github.com/you/unreachable"
	const good = "github.com/you/example"
	goodResp := `{"data": {"viewer": {"login": "you"}, "repository": {"pullRequests": {"nodes": [
		{
			"number": 4, "title": "Fix", "author": {"login": "you"},
			"updatedAt": "2026-11-10T10:00:00Z",
			"commits": {"nodes": [{"commit": {"messageBody": "x\n\nCo-Authored-By: Claude <noreply@example.com>"}}]},
			"comments": {"nodes": [
				{"author": {"login": "reviewer"}, "body": "survives", "createdAt": "2026-11-10T09:00:00Z"}
			]},
			"reviews": {"nodes": []},
			"reviewThreads": {"nodes": []},
			"head": {"nodes": []}
		}
	]}}}}`
	run := func(ctx context.Context, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "name=unreachable") {
			return nil, fmt.Errorf("could not resolve host")
		}
		return []byte(goodResp), nil
	}
	g := &GitHub{Repos: []string{bad, good}, run: run}
	items, err := g.Items(context.Background(), time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	if len(items) != 1 || items[0].Text != "survives" {
		t.Fatalf("items = %+v, want the good repo's comment", items)
	}
	if err == nil || !strings.Contains(err.Error(), bad) {
		t.Errorf("err = %v, want it to name %s", err, bad)
	}
	if err != nil && strings.Contains(err.Error(), good) {
		t.Errorf("err = %v, names the repo that answered", err)
	}
}

// An origin on another forge can never answer: it is skipped without a gh
// call, and without an error.
func TestGitHubItemsSkipsForeignHost(t *testing.T) {
	run := func(ctx context.Context, args ...string) ([]byte, error) {
		t.Errorf("gh called for a foreign host: gh %s", strings.Join(args, " "))
		return nil, fmt.Errorf("foreign host")
	}
	g := &GitHub{Repos: []string{"gitlab.com/you/example", "gitlab.example.com/you/example", "bitbucket.org/you/example"}, run: run}
	items, err := g.Items(context.Background(), time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	if err != nil || len(items) != 0 {
		t.Fatalf("items = %v, %v; want none and no error", items, err)
	}
}

func TestRegistry(t *testing.T) {
	src, err := Open("github", Options{Repos: []string{"github.com/you/example"}})
	if err != nil || src.Name() != "github" {
		t.Fatalf("Open(github) = %v, %v", src, err)
	}
	if g, ok := src.(*GitHub); !ok || len(g.Repos) != 1 {
		t.Errorf("Open(github) didn't carry the repos: %+v", src)
	}
	if _, err := Open("gerrit", Options{}); err == nil || !strings.Contains(err.Error(), "github") {
		t.Errorf("Open(gerrit) = %v, want an error naming the known sources", err)
	}
}

func TestFake(t *testing.T) {
	f := &Fake{List: []Item{{Source: SourceReview, Text: "t"}}}
	since := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	items, err := f.Items(context.Background(), since)
	if err != nil || len(items) != 1 || f.Name() != "fake" {
		t.Fatalf("fake = %v, %v, %q", items, err, f.Name())
	}
	if len(f.Since) != 1 || !f.Since[0].Equal(since) {
		t.Errorf("fake recorded %v", f.Since)
	}
}
