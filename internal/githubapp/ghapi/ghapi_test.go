package ghapi

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fastBackoff(t *testing.T) {
	t.Helper()
	saved := Backoff
	Backoff = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { Backoff = saved })
}

// failing answers status for the first n calls, then 200 {}.
func failing(t *testing.T, n int32, status int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= n {
			w.WriteHeader(status)
			return
		}
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// refusing fails the first n requests as a dial would, before sending.
type refusing struct {
	n     atomic.Int32
	calls atomic.Int32
}

func (r *refusing) RoundTrip(req *http.Request) (*http.Response, error) {
	if r.calls.Add(1) <= r.n.Load() {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	}
	return http.DefaultTransport.RoundTrip(req)
}

func TestGetRetriesServerErrors(t *testing.T) {
	fastBackoff(t)
	for _, status := range []int{500, 502, 503, 504} {
		srv, calls := failing(t, 2, status)
		if err := Call(context.Background(), nil, srv.URL, http.MethodGet, "/app", "", nil, nil); err != nil {
			t.Errorf("%d twice, then 200: %v; want success", status, err)
		}
		if got := calls.Load(); got != 3 {
			t.Errorf("%d: %d calls, want 3", status, got)
		}
	}
}

func TestGetGivesUpAfterBackoff(t *testing.T) {
	fastBackoff(t)
	srv, calls := failing(t, 99, http.StatusBadGateway)
	err := Call(context.Background(), nil, srv.URL, http.MethodGet, "/app", "", nil, nil)
	var ae *Error
	if !errors.As(err, &ae) || ae.Status != http.StatusBadGateway {
		t.Errorf("err = %v; want the last 502", err)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("%d calls, want 3", got)
	}
}

func TestClientErrorsAreNotRetried(t *testing.T) {
	fastBackoff(t)
	for _, status := range []int{401, 403, 404, 422, 429} {
		srv, calls := failing(t, 1, status)
		if err := Call(context.Background(), nil, srv.URL, http.MethodGet, "/app", "", nil, nil); err == nil {
			t.Errorf("%d: no error", status)
		}
		if got := calls.Load(); got != 1 {
			t.Errorf("%d: %d calls, want 1", status, got)
		}
	}
}

func TestPostAndDeleteSeenByGitHubAreNotRetried(t *testing.T) {
	fastBackoff(t)
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		srv, calls := failing(t, 1, http.StatusBadGateway)
		if err := Call(context.Background(), nil, srv.URL, method, "/x", "", nil, nil); err == nil {
			t.Errorf("%s: no error", method)
		}
		if got := calls.Load(); got != 1 {
			t.Errorf("%s after a 502: %d calls, want 1", method, got)
		}
	}
}

func TestPostRetriesWhenNeverSent(t *testing.T) {
	fastBackoff(t)
	srv, calls := failing(t, 0, 0)
	rt := &refusing{}
	rt.n.Store(2)
	body := map[string]string{"a": "b"}
	if err := Call(context.Background(), &http.Client{Transport: rt}, srv.URL, http.MethodPost, "/x", "", body, nil); err != nil {
		t.Fatalf("refused twice, then sent: %v; want success", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("server saw %d calls, want 1", got)
	}
}

func TestCallOnceNeverRetries(t *testing.T) {
	fastBackoff(t)
	srv, calls := failing(t, 0, 0)
	rt := &refusing{}
	rt.n.Store(1)
	if err := CallOnce(context.Background(), &http.Client{Transport: rt}, srv.URL, http.MethodPost, "/x", "", nil, nil); err == nil {
		t.Error("refused once: no error")
	}
	if got := rt.calls.Load(); got != 1 {
		t.Errorf("%d attempts, want 1", got)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("server saw %d calls, want 0", got)
	}
}

func TestRetryStopsAtTheDeadline(t *testing.T) {
	saved := Backoff
	Backoff = []time.Duration{time.Hour}
	t.Cleanup(func() { Backoff = saved })
	srv, calls := failing(t, 99, http.StatusServiceUnavailable)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	err := Call(ctx, nil, srv.URL, http.MethodGet, "/app", "", nil, nil)
	var ae *Error
	if !errors.As(err, &ae) || ae.Status != http.StatusServiceUnavailable {
		t.Errorf("err = %v; want the 503", err)
	}
	if calls.Load() != 1 || time.Since(start) > 500*time.Millisecond {
		t.Errorf("%d calls in %v; want 1, without waiting", calls.Load(), time.Since(start))
	}
}

func TestRealRefusalCountsAsUnsent(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	err = CallOnce(context.Background(), nil, "http://"+addr, http.MethodPost, "/x", "", nil, nil)
	var te *TransportError
	if !errors.As(err, &te) || !unsent(te.Err) {
		t.Errorf("err = %v; want a transport error that counts as unsent", err)
	}
}

// limited answers status with headers for the first n calls, then 200 {}.
func limited(t *testing.T, n int32, status int, headers map[string]string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= n {
			for k, v := range headers {
				w.Header().Set(k, v)
			}
			w.WriteHeader(status)
			w.Write([]byte(`{"message":"API rate limit exceeded"}`))
			return
		}
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestRateLimitedPostWaitsAndRetries(t *testing.T) {
	srv, calls := limited(t, 1, http.StatusTooManyRequests, map[string]string{"Retry-After": "1"})
	start := time.Now()
	if err := Call(context.Background(), nil, srv.URL, http.MethodPost, "/x", "", nil, nil); err != nil {
		t.Fatalf("429 with Retry-After 1, then 200: %v; want success", err)
	}
	if calls.Load() != 2 || time.Since(start) < time.Second {
		t.Errorf("%d calls in %v; want 2, a second apart", calls.Load(), time.Since(start))
	}
}

func TestRetryAfterBeyondTheLimitIsNotWaited(t *testing.T) {
	srv, calls := limited(t, 99, http.StatusForbidden, map[string]string{"Retry-After": "3600"})
	start := time.Now()
	err := Call(context.Background(), nil, srv.URL, http.MethodGet, "/app", "", nil, nil)
	var ae *Error
	if !errors.As(err, &ae) || ae.RetryAfter != time.Hour || !strings.Contains(err.Error(), "retry after 1h0m0s") {
		t.Errorf("err = %v; want the 403 with its hour's wait", err)
	}
	if calls.Load() != 1 || time.Since(start) > 500*time.Millisecond {
		t.Errorf("%d calls in %v; want 1, without waiting", calls.Load(), time.Since(start))
	}
}

func TestRetryAfterBeyondTheDeadlineIsNotWaited(t *testing.T) {
	srv, calls := limited(t, 99, http.StatusTooManyRequests, map[string]string{"Retry-After": "10"})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	if err := Call(ctx, nil, srv.URL, http.MethodGet, "/app", "", nil, nil); err == nil {
		t.Error("no error")
	}
	if calls.Load() != 1 || time.Since(start) > 500*time.Millisecond {
		t.Errorf("%d calls in %v; want 1, without waiting", calls.Load(), time.Since(start))
	}
}

func TestCallOnceIgnoresRetryAfter(t *testing.T) {
	srv, calls := limited(t, 1, http.StatusTooManyRequests, map[string]string{"Retry-After": "1"})
	if err := CallOnce(context.Background(), nil, srv.URL, http.MethodPost, "/x", "", nil, nil); err == nil {
		t.Error("no error")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("%d calls, want 1", got)
	}
}

func TestRetryAfterFromHeaders(t *testing.T) {
	fixed := time.Unix(1_000_000, 0)
	now = func() time.Time { return fixed }
	t.Cleanup(func() { now = time.Now })
	reset := strconv.FormatInt(fixed.Add(5*time.Second).Unix(), 10)
	for _, c := range []struct {
		name   string
		status int
		h      map[string]string
		want   time.Duration
	}{
		{"Retry-After", 429, map[string]string{"Retry-After": "7"}, 7 * time.Second},
		{"Retry-After on a 503", 503, map[string]string{"Retry-After": "2"}, 2 * time.Second},
		{"Retry-After as a date is ignored", 429, map[string]string{"Retry-After": "Wed, 21 Oct 2026 07:28:00 GMT"}, 0},
		{"primary limit reset", 403, map[string]string{"X-Ratelimit-Remaining": "0", "X-Ratelimit-Reset": reset}, 6 * time.Second},
		{"reset already past", 403, map[string]string{"X-Ratelimit-Remaining": "0", "X-Ratelimit-Reset": "1"}, time.Second},
		{"reset with requests left", 403, map[string]string{"X-Ratelimit-Remaining": "12", "X-Ratelimit-Reset": reset}, 0},
		{"reset on a 404", 404, map[string]string{"X-Ratelimit-Remaining": "0", "X-Ratelimit-Reset": reset}, 0},
		{"a plain 403", 403, nil, 0},
	} {
		h := http.Header{}
		for k, v := range c.h {
			h.Set(k, v)
		}
		if got := retryAfter(c.status, h); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
