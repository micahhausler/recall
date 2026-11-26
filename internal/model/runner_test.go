package model

import "testing"

func TestUsageString(t *testing.T) {
	for _, tc := range []struct {
		u    Usage
		want string
	}{
		{Usage{InputTokens: 10, OutputTokens: 2, CostUSD: 0.01}, "10 in, 2 out, $0.0100"},
		{Usage{Credits: 0.5}, "0.50 credits"},
		{Usage{InputTokens: 10, OutputTokens: 2}, "10 in, 2 out"},
		{Usage{InputTokens: 10, OutputTokens: 2, CostUSD: 0.01, Credits: 0.5}, "10 in, 2 out, $0.0100, 0.50 credits"},
		{Usage{ModelID: "m"}, "no usage reported"},
	} {
		if got := tc.u.String(); got != tc.want {
			t.Errorf("%+v = %q, want %q", tc.u, got, tc.want)
		}
	}
	u := Usage{InputTokens: 10}.Add(Usage{Credits: 0.5})
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
