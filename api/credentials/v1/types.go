// Package v1 is the recall.credentials/v1 contract (docs/credentials.md):
// the one object recall hands a credential command in its environment and
// reads back from the command's stdout. A command written in Go imports it
// so the two sides can't spell a field differently. It imports only the
// standard library, and nothing here runs a command or talks to a host.
//
// The contract grows by adding optional fields to Spec and Status. Both
// sides ignore fields they don't know, so decode with encoding/json's
// default, never DisallowUnknownFields.
package v1

import (
	"fmt"
	"time"
)

// The apiVersion and kind every object carries, in both directions.
// Either side refuses anything else.
const (
	APIVersion = "recall.credentials/v1"
	Kind       = "Credential"
)

// ExecInfoEnv is the environment variable recall sets for the command to
// the request, a Credential as JSON with Spec set, the way a Kubernetes
// client sets KUBERNETES_EXEC_INFO. It is the only place the request
// goes: the command's stdin is empty.
const ExecInfoEnv = "RECALL_EXEC_INFO"

// Credential is the contract's one object, shaped like a Kubernetes
// TokenRequest: recall fills in Spec, what it asks for, and the command
// answers with Status, what it grants.
type Credential struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Spec       Spec   `json:"spec"`
	// Status is nil in what recall sends. The command sets it.
	Status *Status `json:"status,omitempty"`
}

// Spec is what recall asks for.
type Spec struct {
	// Repo is the repository to act on, as host/owner/name, like
	// github.com/you/example: the origin's slug, the same whatever
	// protocol the checkout was cloned over.
	Repo string `json:"repo"`
}

// Status is what the command grants.
type Status struct {
	// Token authenticates gh and git push on the requested repository.
	// It is the one required field, and a secret: it is marshaled to JSON
	// because that is the contract, and String and GoString leave it out.
	Token string `json:"token"`
	// ExpirationTimestamp is when the token stops working. Nil is
	// unknown, and recall keeps the credential for the run.
	ExpirationTimestamp *time.Time `json:"expirationTimestamp,omitempty"`
	// CommitIdentity authors and commits recall's commits, so they match
	// whoever the token opens the pull request as.
	CommitIdentity *CommitIdentity `json:"commitIdentity,omitempty"`
	// Owner is the login of the person the credential acts for. A bot's
	// token names the bot, so recall can't learn it from the host.
	Owner string `json:"owner,omitempty"`
}

// CommitIdentity is a git author and committer. Both fields are needed.
type CommitIdentity struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// String formats a status without its token, so one that reaches a log or
// an error message by accident doesn't carry the secret. fmt calls it for
// %v, %+v, and %s, on a Status and on the Credential that points to one.
func (s Status) String() string {
	token := "none"
	if s.Token != "" {
		token = "redacted"
	}
	expires := "unknown"
	if s.ExpirationTimestamp != nil {
		expires = s.ExpirationTimestamp.UTC().Format(time.RFC3339)
	}
	identity := "none"
	if s.CommitIdentity != nil {
		identity = fmt.Sprintf("%s <%s>", s.CommitIdentity.Name, s.CommitIdentity.Email)
	}
	return fmt.Sprintf("Status{Token: %s, ExpirationTimestamp: %s, CommitIdentity: %s, Owner: %q}", token, expires, identity, s.Owner)
}

// GoString is String for %#v, which would otherwise print every field.
func (s Status) GoString() string { return s.String() }
