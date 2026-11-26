// Package ghapi makes one GitHub REST call: the headers GitHub asks for,
// JSON in and out, a retry when one is safe, and a failure as a typed
// error.
package ghapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultAPI is GitHub's REST root.
const DefaultAPI = "https://api.github.com"

// UserAgent names this tool to GitHub; empty sends Go's default.
var UserAgent string

// Error is a call GitHub answered with a failure.
type Error struct {
	Method, Path string
	Status       int
	Message      string
	// RetryAfter is how long GitHub asked to wait, zero when it didn't.
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	if e.RetryAfter > 0 {
		return fmt.Sprintf("github %s %s: %d %s (retry after %s)", e.Method, e.Path, e.Status, msg, e.RetryAfter.Round(time.Second))
	}
	return fmt.Sprintf("github %s %s: %d %s", e.Method, e.Path, e.Status, msg)
}

// TransportError is a call that got no answer: the request never reached
// GitHub, or its answer couldn't be read. Err never holds the URL, so a
// caller whose path is a secret can report Err alone.
type TransportError struct {
	Method, Path string
	Err          error
}

func (e *TransportError) Error() string {
	return fmt.Sprintf("github %s %s: %v", e.Method, e.Path, e.Err)
}

func (e *TransportError) Unwrap() error { return e.Err }

// Backoff is the wait before each retry; its length is how many retries
// a call gets. Each wait is jittered down by up to half.
var Backoff = []time.Duration{500 * time.Millisecond, time.Second}

// MaxRetryAfter is the longest wait GitHub can ask for that is waited out.
var MaxRetryAfter = 20 * time.Second

var now = time.Now

// Call makes one GitHub REST call with bearer as the Authorization bearer
// (empty sends none), body as JSON when not nil, and decodes the answer
// into out when not nil. api is the REST root, empty for DefaultAPI, and
// client nil for http.DefaultClient.
//
// A GET is retried after a transport error or a 500, 502, 503, or 504.
// Any other method is retried only when the request never reached GitHub:
// a POST GitHub saw may have minted something, and DELETE /installation/token
// twice fails the second time. A rate limit refused the request, so any
// method is retried after it, once the wait GitHub asked for is over.
func Call(ctx context.Context, client *http.Client, api, method, path, bearer string, body, out any) error {
	var err error
	for attempt := 0; ; attempt++ {
		err = once(ctx, client, api, method, path, bearer, body, out)
		if err == nil || attempt == len(Backoff) || !retryable(method, err) {
			return err
		}
		if !wait(ctx, delay(err, Backoff[attempt])) {
			return err
		}
	}
}

// CallOnce is Call with no retry, for a call that can't be made twice.
func CallOnce(ctx context.Context, client *http.Client, api, method, path, bearer string, body, out any) error {
	return once(ctx, client, api, method, path, bearer, body, out)
}

func retryable(method string, err error) bool {
	var te *TransportError
	if errors.As(err, &te) {
		return method == http.MethodGet || unsent(te.Err)
	}
	var ae *Error
	if !errors.As(err, &ae) {
		return false
	}
	if ae.RetryAfter > MaxRetryAfter {
		return false
	}
	if rateLimited(ae) {
		return true
	}
	if method == http.MethodGet {
		switch ae.Status {
		case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		}
	}
	return false
}

func rateLimited(e *Error) bool {
	return e.RetryAfter > 0 && (e.Status == http.StatusForbidden || e.Status == http.StatusTooManyRequests)
}

// delay is the wait GitHub asked for, or backoff less up to half of it.
func delay(err error, backoff time.Duration) time.Duration {
	var ae *Error
	if errors.As(err, &ae) && ae.RetryAfter > 0 {
		return ae.RetryAfter
	}
	return backoff - rand.N(backoff/2+1)
}

// retryAfter is the wait a failed answer asks for: Retry-After, or for a
// rate limit with none, the time until x-ratelimit-reset.
func retryAfter(status int, h http.Header) time.Duration {
	if v := h.Get("Retry-After"); v != "" {
		if secs, err := strconv.ParseInt(v, 10, 64); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
		return 0
	}
	if status != http.StatusForbidden && status != http.StatusTooManyRequests {
		return 0
	}
	if h.Get("X-Ratelimit-Remaining") != "0" {
		return 0
	}
	reset, err := strconv.ParseInt(h.Get("X-Ratelimit-Reset"), 10, 64)
	if err != nil {
		return 0
	}
	// A second past the reset, for clock skew.
	if d := time.Unix(reset, 0).Sub(now()) + time.Second; d > 0 {
		return d
	}
	return time.Second
}

// unsent reports an error from before the request was written: the
// connection never opened.
func unsent(err error) bool {
	var oe *net.OpError
	return errors.As(err, &oe) && oe.Op == "dial"
}

// wait sleeps for d, and reports false when ctx ends first or would end
// before then.
func wait(ctx context.Context, d time.Duration) bool {
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < d {
		return false
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func once(ctx context.Context, client *http.Client, api, method, path, bearer string, body, out any) error {
	if api == "" {
		api = DefaultAPI
	}
	if client == nil {
		client = http.DefaultClient
	}
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(api, "/")+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if UserAgent != "" {
		req.Header.Set("User-Agent", UserAgent)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := client.Do(req)
	if err != nil {
		// The client wraps its error with the URL; keep the cause only.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return &TransportError{Method: method, Path: path, Err: err}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return &TransportError{Method: method, Path: path, Err: err}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &e)
		return &Error{Method: method, Path: path, Status: resp.StatusCode, Message: e.Message, RetryAfter: retryAfter(resp.StatusCode, resp.Header)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return &TransportError{Method: method, Path: path, Err: err}
	}
	return nil
}
