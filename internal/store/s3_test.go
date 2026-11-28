package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/micahhausler/recall/internal/config"
)

// fakeS3 is a path-style S3 over HTTP: enough of PutObject (honoring
// If-None-Match), GetObject, HeadObject, DeleteObject, and ListObjectsV2
// for the store.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	puts    []string
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")
	if bucket != "bkt" {
		http.Error(w, "wrong bucket", http.StatusNotFound)
		return
	}
	switch {
	case r.Method == http.MethodPut:
		f.puts = append(f.puts, key)
		// A conditional put (If-None-Match: *) refuses an existing key; a
		// plain put, which only the reducer-claim handoff uses, replaces it.
		if r.Header.Get("If-None-Match") == "*" {
			if _, ok := f.objects[key]; ok {
				w.WriteHeader(http.StatusPreconditionFailed)
				fmt.Fprint(w, `<?xml version="1.0"?><Error><Code>PreconditionFailed</Code><Message>exists</Message></Error>`)
				return
			}
		}
		body, _ := io.ReadAll(r.Body)
		f.objects[key] = body
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodDelete:
		// S3 answers 204 whether or not the key held an object.
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodHead:
		data, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodGet && key == "":
		q, _ := url.ParseQuery(r.URL.RawQuery)
		prefix := q.Get("prefix")
		var keys []string
		for k := range f.objects {
			if strings.HasPrefix(k, prefix) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		fmt.Fprint(w, `<?xml version="1.0"?><ListBucketResult><IsTruncated>false</IsTruncated>`)
		for _, k := range keys {
			fmt.Fprintf(w, "<Contents><Key>%s</Key><Size>%d</Size></Contents>", k, len(f.objects[k]))
		}
		fmt.Fprint(w, `</ListBucketResult>`)
	case r.Method == http.MethodGet:
		data, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `<?xml version="1.0"?><Error><Code>NoSuchKey</Code><Message>missing</Message></Error>`)
			return
		}
		w.Write(data)
	default:
		http.Error(w, "unhandled", http.StatusBadRequest)
	}
}

func testClient(t *testing.T, endpoint string) *s3.Client {
	t.Helper()
	cfg := aws.Config{
		Region:      "us-west-2",
		Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""),
	}
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
		o.RetryMaxAttempts = 1
	})
}

func TestS3Contract(t *testing.T) {
	fake := &fakeS3{objects: map[string][]byte{}}
	server := httptest.NewServer(fake)
	defer server.Close()
	contract(t, NewS3(testClient(t, server.URL), "bkt"))
}

func TestS3PutsAreConditional(t *testing.T) {
	fake := &fakeS3{objects: map[string][]byte{}}
	server := httptest.NewServer(fake)
	defer server.Close()
	s := NewS3(testClient(t, server.URL), "bkt")
	ctx := context.Background()
	if err := s.PutIfAbsent(ctx, "k", strings.NewReader("v")); err != nil {
		t.Fatal(err)
	}
	if err := s.PutIfAbsent(ctx, "k", strings.NewReader("v2")); !errors.Is(err, ErrExists) {
		t.Fatalf("second put: %v, want ErrExists", err)
	}
	if len(fake.puts) != 2 {
		t.Errorf("puts = %v; the fake, not the client, must decide 412", fake.puts)
	}
}

// isolateAWS clears AWS_* from the environment and points the shared
// config files at an empty temp dir.
func isolateAWS(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "AWS_") {
			t.Setenv(k, "")
			os.Unsetenv(k)
		}
	}
	dir := t.TempDir()
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
}

func TestNamespacedBucket(t *testing.T) {
	if got := NamespacedBucket("123456789012", "us-west-2"); got != "chat-archive-123456789012-us-west-2-an" {
		t.Errorf("NamespacedBucket = %q", got)
	}
	for bucket, want := range map[string]bool{
		"chat-archive-123456789012-us-west-2-an": true,
		"mine-123456789012-us-west-2-an":         true,
		"mine-123456789012-eu-central-1-an":      false,
		"team-backup-123456789012-archive-an":    false,
		"chat-archive":                           false,
		"plan-an":                                false,
		"mine-12345-us-west-2-an":                false,
		"-123456789012-us-west-2-an":             false,
	} {
		if got := InNamespace(bucket, "us-west-2"); got != want {
			t.Errorf("InNamespace(%q, us-west-2) = %v, want %v", bucket, got, want)
		}
	}
	if InNamespace("chat-archive-123456789012-us-west-2-an", "") {
		t.Error("no region matched a namespace name")
	}
}

// Opening the S3 store sends no request: the bucket is the configured name.
func TestS3ClientMakesNoCalls(t *testing.T) {
	isolateAWS(t)
	var mu sync.Mutex
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	t.Setenv("AWS_ENDPOINT_URL", server.URL)
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	ctx := context.Background()
	if _, _, err := S3Client(ctx, config.S3Settings{Region: "us-west-2"}); err == nil || !strings.Contains(err.Error(), "store.s3.bucket") {
		t.Errorf("no bucket: %v, want an error naming store.s3.bucket", err)
	}
	_, bucket, err := S3Client(ctx, config.S3Settings{Region: "us-west-2", Bucket: "bkt"})
	if err != nil || bucket != "bkt" {
		t.Fatalf("S3Client = %q, %v", bucket, err)
	}
	st, err := Open(ctx, config.StoreSettings{Kind: "s3", S3: config.S3Settings{Region: "us-west-2", Bucket: "bkt"}})
	if err != nil {
		t.Fatal(err)
	}
	if b := st.(*S3Store).Bucket(); b != "bkt" {
		t.Errorf("bucket = %q", b)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 0 {
		t.Errorf("opening the store called %v", calls)
	}
}

func TestS3RequestsCarryTheAppID(t *testing.T) {
	isolateAWS(t)
	var ua string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua = r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	cfg, err := AWSConfig(context.Background(), config.S3Settings{Region: "us-west-2"})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Credentials = credentials.NewStaticCredentialsProvider("test", "test", "")
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(server.URL)
		o.UsePathStyle = true
		o.RetryMaxAttempts = 1
	})
	NewS3(client, "bkt").Head(context.Background(), "k")
	if !strings.Contains(ua, "app/recall-dev") {
		t.Errorf("User-Agent %q misses app/recall-dev", ua)
	}
}
