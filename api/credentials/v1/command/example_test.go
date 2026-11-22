package command_test

import (
	"context"
	"fmt"
	"os"
	"time"

	credentialsv1 "github.com/micahhausler/recall/api/credentials/v1"
	"github.com/micahhausler/recall/api/credentials/v1/command"
)

// grant is the one part a credential command writes itself: where a token
// for the requested repository comes from. This one makes its token up.
func grant(ctx context.Context, req command.Request) (credentialsv1.Status, error) {
	if req.Host != "example.com" {
		return credentialsv1.Status{}, fmt.Errorf("%s is on %s; this command covers example.com only", req.Spec.Repo, req.Host)
	}
	expires := time.Date(2026, 3, 2, 13, 0, 0, 0, time.UTC)
	return credentialsv1.Status{
		Token:               "token-for-" + req.Owner + "-" + req.Name,
		ExpirationTimestamp: &expires,
		CommitIdentity:      &credentialsv1.CommitIdentity{Name: "recall-you[bot]", Email: "recall-you[bot]@example.com"},
		Owner:               "you",
	}, nil
}

// A whole credential command is a grant function and a main with this
// body. recall sets the request in the environment, and a command reads it
// with os.Getenv(credentialsv1.ExecInfoEnv); the example spells one out.
func Example() {
	execInfo := `{"apiVersion":"recall.credentials/v1","kind":"Credential","spec":{"repo":"example.com/you/example"}}`
	if err := command.Run(context.Background(), execInfo, os.Stdout, grant); err != nil {
		fmt.Fprintln(os.Stderr, "recall-credential:", err)
		os.Exit(1)
	}
	// Output:
	// {
	//   "apiVersion": "recall.credentials/v1",
	//   "kind": "Credential",
	//   "spec": {
	//     "repo": "example.com/you/example"
	//   },
	//   "status": {
	//     "token": "token-for-you-example",
	//     "expirationTimestamp": "2026-03-02T13:00:00Z",
	//     "commitIdentity": {
	//       "name": "recall-you[bot]",
	//       "email": "recall-you[bot]@example.com"
	//     },
	//     "owner": "you"
	//   }
	// }
}
