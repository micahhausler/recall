package main

import (
	"testing"
	"time"

	"github.com/micahhausler/recall/internal/clock"
	"github.com/micahhausler/recall/internal/config"
	"github.com/micahhausler/recall/internal/credential"
	"github.com/micahhausler/recall/internal/review"
)

func TestPublisherUsesTheCredentialCommandOnlyWhenConfigured(t *testing.T) {
	plain := &archiveEnv{settings: &config.Settings{}, clk: clock.Real{}}
	if plain.credentials() != nil {
		t.Error("with no command configured, there is a credential source")
	}
	pub, err := plain.publisher("pr")
	if err != nil {
		t.Fatal(err)
	}
	if pr := pub.(*review.PR); pr.Credentials != nil {
		t.Error("with no command configured, the pr publisher got credentials")
	}

	settings := &config.Settings{}
	settings.Review.Credentials = config.CredentialSettings{Command: []string{"recall-credential", "token"}, Timeout: 5 * time.Second}
	env := &archiveEnv{settings: settings, clk: clock.Real{}}
	pub, err = env.publisher("pr")
	if err != nil {
		t.Fatal(err)
	}
	exec, ok := pub.(*review.PR).Credentials.(*credential.Exec)
	if !ok || len(exec.Command) != 2 || exec.Command[0] != "recall-credential" || exec.Timeout != 5*time.Second {
		t.Fatalf("pr publisher credentials = %+v", pub.(*review.PR).Credentials)
	}
	// One source for the whole process: the publisher, the review refresh,
	// and the evidence scan share its cache.
	if env.credentials() != credential.Source(exec) {
		t.Error("a second credential source was opened")
	}
	if again, _ := env.publisher("pr"); again.(*review.PR).Credentials != credential.Source(exec) {
		t.Error("a second publisher got its own credential source")
	}

	if pub, err := env.publisher("branch"); err != nil || pub != (review.Branch{}) {
		t.Errorf("branch publisher = %v, %v", pub, err)
	}
	if _, err := env.publisher("nope"); err == nil {
		t.Error("an unknown publisher opened")
	}
}
