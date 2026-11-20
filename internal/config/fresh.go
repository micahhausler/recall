package config

import (
	"fmt"
	"strings"
)

// KV is one config key and the raw TOML value to set it to.
type KV struct {
	Key, Value string
	// Why says what the value buys, for the person reading the plan.
	Why string
}

// The commands behind the loop's three steps. A hook step is covered when
// an enabled recur job's command runs the same step, judged by the step's
// command and never by whether any job exists: the hook worker skips a
// covered step, and recall init leaves its key off, so a step never runs
// twice.
const (
	StepArchive = "recall archive-chats run"
	StepMap     = "recall learn map"
	StepLearn   = "recall learn run"
)

// Covers reports whether an enabled recur job's command already runs step.
func (r RecurSettings) Covers(step string) bool {
	for _, j := range r.Jobs {
		if strings.Contains(j.Command, step) {
			return true
		}
	}
	return false
}

// Fresh returns what `recall init` sets on a machine, so it goes from
// install to a first reviewed proposal with no bucket, no scheduler, no git
// remote, and no hand-edited TOML: the session hooks are the default way to
// run the loop, and recur jobs stay the opt-in for a fixed schedule. It
// never changes a key a config file already sets, and it leaves a hook step
// off when an enabled recur job's command already runs that step, judged
// step by step, so a machine that schedules part of the loop keeps it and
// gets hooks for the rest. The store needs no key: it is a local directory
// by default.
//
// existing says whether any config file was already there. The review moves
// to a local branch only on a machine with no config at all: a machine with
// one relies on learn.review's default, pull requests, and switching it
// would silently reroute its proposals. host is this machine's name, so a
// note about another machine's claim shows both sides. reducer, when set,
// is asked for the host that already runs the reduce on this machine's
// store, so only one machine per store proposes; nil means none is known.
// What init leaves alone it says in notes, with the command that changes it.
func Fresh(c *Config, existing bool, host string, reducer func() (string, error)) (plan []KV, notes []string) {
	unset := func(key string) bool { _, ok := c.Get(key); return !ok }
	if unset("learn.review") {
		if existing {
			notes = append(notes, "learn.review = \"pr\" (the default, unchanged): proposals go up as pull requests; "+
				"`recall config set learn.review '\"branch\"'` keeps them on a local branch instead")
		} else {
			plan = append(plan, KV{"learn.review", `"branch"`, "review proposals on a local branch in each checkout; no git remote or gh needed"})
		}
	}
	commands := jobCommands(c)
	covered := func(step string) bool {
		for _, cmd := range commands {
			if strings.Contains(cmd, step) {
				return true
			}
		}
		return false
	}
	if unset("hooks.map") {
		if covered(StepMap) {
			notes = append(notes, "hooks.map stays off: a recur job already runs `"+StepMap+"`")
		} else {
			plan = append(plan, KV{"hooks.map", "true", "observe each session from now on when it goes quiet, from the plugin's session-end hook, at a few cents of model use each; no cron"})
		}
	}
	if unset("hooks.archive") {
		if covered(StepArchive) {
			notes = append(notes, "hooks.archive stays off: a recur job already runs `"+StepArchive+"`")
		} else {
			plan = append(plan, KV{"hooks.archive", "true", "keep a weekly archive of transcripts in the store, from the same hook"})
		}
	}
	if unset("hooks.learn_every") {
		claim, err := "", error(nil)
		if !covered(StepLearn) && reducer != nil {
			claim, err = reducer()
		}
		switch {
		case covered(StepLearn):
			notes = append(notes, "hooks.learn_every stays off: a recur job already runs `"+StepLearn+"`")
		case err != nil:
			notes = append(notes, fmt.Sprintf("hooks.learn_every stays off: couldn't check the store for another machine's reduce (%v); "+
				"turn it on on one machine with `recall config set hooks.learn_every 7`", err))
		case claim != "":
			notes = append(notes, fmt.Sprintf("hooks.learn_every stays off: %s already runs the reduce on this store (this machine is %s), "+
				"and one reducer per store avoids duplicate proposals; if %s is gone, `recall learn reducer --take` moves the claim here", claim, host, claim))
		default:
			plan = append(plan, KV{"hooks.learn_every", "7", "propose changes once a week, from the same hook: one reduce and one apply model call"})
		}
	}
	return plan, notes
}

// jobCommands lists the commands of the enabled recur jobs.
func jobCommands(c *Config) []string {
	var out []string
	for _, name := range c.tableNames("recur.jobs") {
		base := "recur.jobs." + name
		if v, ok := c.Get(base + ".enabled"); ok {
			if enabled, ok := v.(bool); ok && !enabled {
				continue
			}
		}
		if v, ok := c.Get(base + ".command"); ok {
			if cmd, ok := v.(string); ok {
				out = append(out, cmd)
			}
		}
	}
	return out
}
