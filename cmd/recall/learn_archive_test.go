package main

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/micahhausler/recall/internal/archive"
	"github.com/micahhausler/recall/internal/learn"
)

// weekTarZst builds one week's archive the way archive-chats uploads it.
func weekTarZst(t *testing.T, sessions map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(zw)
	for name, content := range sessions {
		hdr := &tar.Header{Name: "p/" + name + ".jsonl", Mode: 0o644, Size: int64(len(content))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func archivedTranscript(ts string) string {
	return fmt.Sprintf(`{"type":"user","timestamp":%q,"cwd":"/home/you/w","entrypoint":"cli","message":{"role":"user","content":"map my history"}}
{"type":"assistant","timestamp":%q,"message":{"model":"claude-sonnet-4-5","stop_reason":"end_turn","content":[{"type":"text","text":"ok"}]}}
`, ts, ts)
}

// Done-when for the full-history recalculation: --archives maps this host's
// own archived sessions when their transcripts are gone from disk, while a
// session still on disk keeps reading from disk, and other hosts' sessions
// are read as before.
func TestArchivesMapThisHostsSessionsGoneFromDisk(t *testing.T) {
	f := newHookFixture(t)
	ctx := context.Background()
	st, _, err := openStore(ctx, f.env)
	if err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	ts := when.Format(time.RFC3339)
	y, w := when.ISOWeek()
	week := archive.Week{Year: y, Num: w}
	mine := weekTarZst(t, map[string]string{
		"sess-on-disk": archivedTranscript(ts),
		"sess-gone":    archivedTranscript(ts),
	})
	if err := st.PutIfAbsent(ctx, archive.Key("claude", "laptop-a", week), bytes.NewReader(mine)); err != nil {
		t.Fatal(err)
	}
	theirs := weekTarZst(t, map[string]string{"sess-remote": archivedTranscript(ts)})
	if err := st.PutIfAbsent(ctx, archive.Key("claude", "laptop-b", week), bytes.NewReader(theirs)); err != nil {
		t.Fatal(err)
	}

	cutoffTime, err := parseSince("30d", f.clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	cutoff := cutoffTime.UTC().Format("2006-01-02T15:04:05Z")
	mapperFn := func(host string) *learn.Mapper { return &learn.Mapper{Host: host} }
	skipped := 0
	jobs, err := archiveJobs(ctx, f.env, st, f.env.harnesses[0], cutoffTime, cutoff, mapperFn,
		map[string]bool{"sess-on-disk": true}, &skipped)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, j := range jobs {
		got[j.session] = j.m.Host
	}
	if got["sess-gone"] != "laptop-a" {
		t.Errorf("this host's archived session wasn't mapped: jobs = %v", got)
	}
	if got["sess-remote"] != "laptop-b" {
		t.Errorf("another host's archived session wasn't mapped: jobs = %v", got)
	}
	if _, ok := got["sess-on-disk"]; ok {
		t.Error("a session still on disk was read from the archive too")
	}
}
