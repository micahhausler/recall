package main

import (
	"context"
	"strings"
	"testing"
)

func TestParseWindow(t *testing.T) {
	for in, want := range map[string]reduceWindow{
		"":       {},
		"all":    {all: true},
		"365d":   {days: 365},
		"1d":     {days: 1},
		"36500d": {days: 36500},
	} {
		got, err := parseWindow(in)
		if err != nil || got != want {
			t.Errorf("parseWindow(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, in := range []string{"nonsense", "0d", "-1d", "365", "24h", "1w", "d", "all365d", "36501d", "106751991167300d", "99999999999999999999d"} {
		if _, err := parseWindow(in); err == nil {
			t.Errorf("parseWindow(%q) took it", in)
		}
	}
}

// A window past 100 years names all.
func TestParseWindowCapNamesAll(t *testing.T) {
	_, err := parseWindow("106751991167300d")
	if err == nil || !strings.Contains(err.Error(), "36500d") || !strings.Contains(err.Error(), "all") {
		t.Errorf("err = %v, want the cap and all named", err)
	}
}

// A bad --window is a usage error before anything runs.
func TestWindowFlagRejectsNonsense(t *testing.T) {
	for _, args := range [][]string{
		{"learn", "reduce", "--window", "nonsense"},
		{"learn", "run", "--window", "365"},
		{"propose", "--window", "nonsense"},
	} {
		lc, _ := recordExec(t)
		code, _, errText := capture(t, args...)
		if code != 2 || len(*lc) != 0 || !strings.Contains(errText, "--window") {
			t.Errorf("recall %v = %d, %q, ran %v; want 2 and nothing run", args, code, errText, *lc)
		}
	}
}

// The reducer reads the window --window names and keeps retirement on learn.retire_days.
func TestReducerTakesTheWindow(t *testing.T) {
	f := newHookFixture(t)
	ctx := context.Background()
	st, _, err := openStore(ctx, f.env)
	if err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]struct {
		days int
		all  bool
	}{"": {0, false}, "365d": {365, false}, "all": {0, true}} {
		w, err := parseWindow(in)
		if err != nil {
			t.Fatal(err)
		}
		r, _, err := newReducer(ctx, f.env, st, nil, w)
		if err != nil {
			t.Fatal(err)
		}
		if r.WindowDays != want.days || r.AllWindow != want.all || r.RetireDays != 90 {
			t.Errorf("--window %q: reducer window %d, all %v, retire %d", in, r.WindowDays, r.AllWindow, r.RetireDays)
		}
	}
}
