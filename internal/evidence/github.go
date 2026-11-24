package evidence

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/micahhausler/recall/internal/config"
	"github.com/micahhausler/recall/internal/credential"
)

func init() {
	Register("github", func(o Options) (Source, error) {
		return &GitHub{Repos: o.Repos, Trailer: o.Trailer, Credentials: o.Credentials}, nil
	})
}

// prLimit is how many of a repo's most recently updated pull requests one
// run scans.
const prLimit = 30

// prQuery fetches everything one scan needs from one repository in a single
// GraphQL call: the viewer (the login gh is authenticated as, the owner)
// and the most recently updated pull requests in any state, each with its
// commit message bodies (where the agent trailer lives), issue comments,
// review bodies, inline review-thread comments, and the head commit's check
// contexts. Update-time order matters: without it, an old pull request
// reviewed recently falls beyond the page and its new comments are silently
// missed. The first/last caps bound the response well above what review on
// one agent's recent work produces: prLimit pull requests, each with its
// last 20 commits, last 50 issue comments, last 20 reviews, last 50 review
// threads of 20 comments each, and first 50 check contexts; anything beyond
// a cap is dropped, never paged.
var prQuery = fmt.Sprintf(`query($owner: String!, $name: String!) {
  viewer { login }
  repository(owner: $owner, name: $name) {
    pullRequests(first: %d, states: [OPEN, CLOSED, MERGED], orderBy: {field: UPDATED_AT, direction: DESC}) {
      nodes {
        number
        title
        author { login }
        updatedAt
        commits(last: 20) { nodes { commit { messageBody } } }
        comments(last: 50) { nodes { author { login } body createdAt } }
        reviews(last: 20) { nodes { author { login } body submittedAt } }
        reviewThreads(last: 50) {
          nodes { comments(first: 20) { nodes { author { login } body path createdAt } } }
        }
        head: commits(last: 1) {
          nodes { commit { statusCheckRollup { contexts(first: 50) { nodes {
            ... on CheckRun { name conclusion completedAt }
            ... on StatusContext { context state }
          } } } } }
        }
      }
    }
  }
}`, prLimit)

// GitHub reads review comments and failed checks on pull requests whose
// commits carry the agent's co-author trailer, in one batched GraphQL query
// per repo through the gh CLI. Like the model runner, it execs the CLI and
// never holds an API client, so existing gh auth works unchanged; with
// Credentials, gh runs with their token instead.
type GitHub struct {
	// Repos are the repositories to scan, as host/owner/name slugs, the
	// form a checkout's origin has.
	Repos []string
	// Trailer overrides the default agent trailer.
	Trailer string
	// Credentials, when set, supply a token per repo for gh to run with,
	// and name the owner, whom gh's own sign-in would otherwise name.
	Credentials credential.Source
	// GH is the gh binary, default "gh".
	GH string
	// run executes gh and returns its stdout; tests inject a fake.
	run func(ctx context.Context, args ...string) ([]byte, error)
}

func (g *GitHub) Name() string { return "github" }

// trailer is the commit trailer that marks an agent-authored commit, and so
// an agent-authored pull request: Claude Code's when none is set.
func (g *GitHub) trailer() string {
	if g.Trailer == "" {
		return config.DefaultTrailer(config.HarnessClaude)
	}
	return g.Trailer
}

// gh runs the gh CLI with env added to the process's own environment.
func (g *GitHub) gh(ctx context.Context, env []string, args ...string) ([]byte, error) {
	if g.run != nil {
		return g.run(ctx, args...)
	}
	bin := g.GH
	if bin == "" {
		bin = "gh"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(errOut.String())
		if detail == "" {
			detail = err.Error()
		}
		return nil, fmt.Errorf("gh %s: %s", strings.Join(args, " "), detail)
	}
	return out.Bytes(), nil
}

// Items lists the review comments and failed checks on each repo's
// agent-authored pull requests updated at or after since: one gh call per
// repo. A repo that can't be scanned (not on GitHub, unreachable) is
// skipped, never fatal: the items from the repos that answered come back
// alongside an error joining the failures, each named by its repo.
func (g *GitHub) Items(ctx context.Context, since time.Time) ([]Item, error) {
	var items []Item
	var errs []error
	for _, repo := range g.Repos {
		if host, _, _ := strings.Cut(repo, "/"); foreignHost(host) {
			continue
		}
		its, err := g.scan(ctx, repo, since)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", repo, err))
			continue
		}
		items = append(items, its...)
	}
	return items, errors.Join(errs...)
}

// foreignHost reports whether a repo's host is unmistakably another forge,
// so the scan skips it without a gh call. GitHub Enterprise can live on any
// hostname, so only the obvious ones are listed; any other non-GitHub host
// just errors and is skipped like any failing repo.
func foreignHost(host string) bool {
	h := strings.ToLower(host)
	if h == "gitlab" || strings.HasPrefix(h, "gitlab.") {
		return true
	}
	switch h {
	case "bitbucket.org", "codeberg.org", "git.sr.ht":
		return true
	}
	return false
}

// ghUser is how GraphQL names a user: an object with a login, or null for a
// deleted account, which decodes to an empty login.
type ghUser struct {
	Login string `json:"login"`
}

// prNode is one pull request in the GraphQL response.
type prNode struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Author    ghUser    `json:"author"`
	UpdatedAt time.Time `json:"updatedAt"`
	Commits   struct {
		Nodes []struct {
			Commit struct {
				MessageBody string `json:"messageBody"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
	Comments struct {
		Nodes []struct {
			Author    ghUser    `json:"author"`
			Body      string    `json:"body"`
			CreatedAt time.Time `json:"createdAt"`
		} `json:"nodes"`
	} `json:"comments"`
	Reviews struct {
		Nodes []struct {
			Author      ghUser    `json:"author"`
			Body        string    `json:"body"`
			SubmittedAt time.Time `json:"submittedAt"`
		} `json:"nodes"`
	} `json:"reviews"`
	// ReviewThreads hold the comments left on the diff itself, which gh pr
	// view never reported and where most review feedback lives.
	ReviewThreads struct {
		Nodes []struct {
			Comments struct {
				Nodes []struct {
					Author    ghUser    `json:"author"`
					Body      string    `json:"body"`
					Path      string    `json:"path"`
					CreatedAt time.Time `json:"createdAt"`
				} `json:"nodes"`
			} `json:"comments"`
		} `json:"nodes"`
	} `json:"reviewThreads"`
	// Head is the head commit, where GraphQL hangs the check rollup; the
	// rollup is null when nothing ever ran.
	Head struct {
		Nodes []struct {
			Commit struct {
				StatusCheckRollup *struct {
					Contexts struct {
						Nodes []checkContext `json:"nodes"`
					} `json:"contexts"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"head"`
}

// checkContext is one entry in the head commit's check rollup: a check run
// with a name, conclusion, and completion time, or a commit status with a
// context and state and no time of its own.
type checkContext struct {
	Name        string    `json:"name"`
	Context     string    `json:"context"`
	Conclusion  string    `json:"conclusion"`
	State       string    `json:"state"`
	CompletedAt time.Time `json:"completedAt"`
}

// scan reads one repo's recent pull requests in a single batched call and
// returns the evidence on the agent-authored ones updated at or after
// since. A pull request without the agent's trailer in any commit isn't the
// agent's work, and contributes nothing.
func (g *GitHub) scan(ctx context.Context, repo string, since time.Time) ([]Item, error) {
	host, path, _ := strings.Cut(repo, "/")
	repoOwner, name, _ := strings.Cut(path, "/")
	// With credentials, gh reads as them, and they say who the owner is:
	// the query's viewer is whoever the token belongs to, often a bot. A
	// credential that names no owner leaves every comment someone else's,
	// never owner-typed.
	var env []string
	credOwner := ""
	if g.Credentials != nil {
		cred, err := g.Credentials.For(ctx, repo)
		if err != nil {
			return nil, err
		}
		if cred.Token == "" {
			return nil, errors.New("the credentials returned an empty token")
		}
		credOwner = cred.Owner
		env = []string{"GH_TOKEN=" + cred.Token}
	}
	args := []string{"api"}
	if host != "" && host != "github.com" {
		args = append(args, "--hostname", host)
	}
	args = append(args, "graphql", "-f", "query="+prQuery, "-F", "owner="+repoOwner, "-F", "name="+name)
	out, err := g.gh(ctx, env, args...)
	if err != nil {
		return nil, err
	}
	var v struct {
		Data struct {
			// Viewer is the login gh is authenticated as: the owner. A
			// comment by anyone else is other people's words and never
			// owner-typed.
			Viewer     ghUser `json:"viewer"`
			Repository struct {
				PullRequests struct {
					Nodes []prNode `json:"nodes"`
				} `json:"pullRequests"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return nil, fmt.Errorf("gh api graphql on %s: %w", repo, err)
	}
	owner := v.Data.Viewer.Login
	if g.Credentials != nil {
		owner = credOwner
	}
	trailer := strings.ToLower(g.trailer())
	var items []Item
	for _, p := range v.Data.Repository.PullRequests.Nodes {
		if p.UpdatedAt.Before(since) {
			continue
		}
		agent := false
		for _, c := range p.Commits.Nodes {
			agent = agent || strings.Contains(strings.ToLower(c.Commit.MessageBody), trailer)
		}
		if !agent {
			continue
		}
		pr := fmt.Sprintf("%s#%d", repo, p.Number)
		add := func(author, body string, when time.Time) {
			// A bot's comment is machine output wearing a login; the failed
			// checks already carry the CI side.
			if strings.TrimSpace(body) == "" || strings.HasSuffix(author, "[bot]") {
				return
			}
			items = append(items, Item{Source: SourceReview, Repo: repo, PR: pr, Author: author,
				Owner: author != "" && author == owner, Text: body, When: when})
		}
		for _, c := range p.Comments.Nodes {
			add(c.Author.Login, c.Body, c.CreatedAt)
		}
		for _, r := range p.Reviews.Nodes {
			add(r.Author.Login, r.Body, r.SubmittedAt)
		}
		for _, th := range p.ReviewThreads.Nodes {
			for _, c := range th.Comments.Nodes {
				add(c.Author.Login, c.Body, c.CreatedAt)
			}
		}
		for _, h := range p.Head.Nodes {
			if h.Commit.StatusCheckRollup == nil {
				continue
			}
			for _, c := range h.Commit.StatusCheckRollup.Contexts.Nodes {
				if !checkFailed(c.Conclusion) && !checkFailed(c.State) {
					continue
				}
				check := c.Name
				if check == "" {
					check = c.Context
				}
				when := c.CompletedAt
				if when.IsZero() {
					when = p.UpdatedAt
				}
				items = append(items, Item{Source: SourceCI, Repo: repo, PR: pr,
					Text: "check " + check + " failed", When: when})
			}
		}
	}
	return items, nil
}

// checkFailed reports whether a check's conclusion, or a commit status's
// state, is a failure the agent should learn from. A cancelled or skipped
// check says nothing about the change.
func checkFailed(s string) bool {
	switch s {
	case "FAILURE", "ERROR", "TIMED_OUT":
		return true
	}
	return false
}
