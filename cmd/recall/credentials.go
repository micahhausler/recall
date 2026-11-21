package main

import (
	"github.com/micahhausler/recall/internal/credential"
	"github.com/micahhausler/recall/internal/review"
)

// credentials is the credential source review.credentials.command names,
// or nil when none is configured and recall acts as the machine's own gh
// login. It is opened once per process, so one run asks the command once
// per repository however many steps use the answer.
func (e *archiveEnv) credentials() credential.Source {
	if e.creds != nil {
		return e.creds
	}
	cfg := e.settings.Review.Credentials
	if len(cfg.Command) == 0 {
		return nil
	}
	e.creds = &credential.Exec{Command: cfg.Command, Timeout: cfg.Timeout, Clock: e.clk}
	return e.creds
}

// publisher opens the review publisher registered under name. With a
// credential command configured, the pr publisher acts as its credentials.
func (e *archiveEnv) publisher(name string) (review.Publisher, error) {
	pub, err := review.New(name)
	if err != nil {
		return nil, err
	}
	if pr, ok := pub.(*review.PR); ok {
		if creds := e.credentials(); creds != nil {
			pr.Credentials = creds
		}
	}
	return pub, nil
}
