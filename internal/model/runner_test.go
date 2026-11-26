package model

import (
	"strings"
	"testing"
)

func TestUsageString(t *testing.T) {
	u := Usage{InputTokens: 10, OutputTokens: 2, CostUSD: 0.01}
	if got := u.String(); strings.Contains(got, "credits") {
		t.Errorf("no-credit usage = %q", got)
	}
	u = u.Add(Usage{Credits: 0.5})
	if got := u.String(); !strings.Contains(got, "0.50 credits") {
		t.Errorf("credit usage = %q", got)
	}
	if u.Zero() || (Usage{}).Zero() != true {
		t.Errorf("Zero() wrong: %+v", u)
	}
}

// A runner without a check of its own takes any effort.
func TestCheckWithoutChecker(t *testing.T) {
	if err := Check(&Fake{}, Call{Model: "m", Effort: "anything"}); err != nil {
		t.Errorf("Check(Fake) = %v", err)
	}
}
