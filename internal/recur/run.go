package recur

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/micahhausler/recall/internal/config"
)

// Run runs one job now, exactly as the scheduler would: /bin/sh from home,
// the recur env set, output appended to <state>/<name>.log with start and
// exit markers. echo mirrors output to it (a terminal, usually).
func (s Setup) Run(name string, jobs map[string]config.Job, echo io.Writer) (int, error) {
	job, ok := jobs[name]
	if !ok {
		return 2, fmt.Errorf("no job %q declared on this machine", name)
	}
	child := os.Environ()
	for k, v := range s.Env {
		if k == "CRON_TZ" {
			continue
		}
		v = strings.ReplaceAll(v, "~/", s.Home+"/")
		v = os.Expand(v, func(n string) string {
			if n == "HOME" {
				return s.Home
			}
			return os.Getenv(n)
		})
		child = append(child, k+"="+v)
	}
	if err := os.MkdirAll(StateDir(s.Home), 0o755); err != nil {
		return 2, err
	}
	logPath := filepath.Join(StateDir(s.Home), name+".log")
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return 2, err
	}
	defer log.Close()

	out := io.Writer(log)
	if echo != nil {
		out = io.MultiWriter(log, echo)
	}
	stamp := time.Now().Format(time.RFC3339)
	fmt.Fprintf(log, "=== %s start %s: %s\n", stamp, name, job.Command)
	cmd := exec.Command("/bin/sh", "-c", job.Command)
	cmd.Dir = s.Home
	cmd.Env = child
	cmd.Stdout = out
	cmd.Stderr = out
	err = cmd.Run()
	rc := 0
	if err != nil {
		rc = 1
		if ee, ok := err.(*exec.ExitError); ok {
			rc = ee.ExitCode()
		}
	}
	fmt.Fprintf(log, "=== %s exit %d\n", time.Now().Format(time.RFC3339), rc)
	return rc, nil
}
