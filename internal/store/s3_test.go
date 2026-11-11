package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/micahhausler/recall/internal/config"
)

// fakeS3 is a path-style S3 over HTTP: enough of PutObject (honoring
// If-None-Match), GetObject, HeadObject, and ListObjectsV2 for the store.
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
		if r.Header.Get("If-None-Match") != "*" {
			http.Error(w, "test wants conditional puts only", http.StatusBadRequest)
			return
		}
		if _, ok := f.objects[key]; ok {
			w.WriteHeader(http.StatusPreconditionFailed)
			fmt.Fprint(w, `<?xml version="1.0"?><Error><Code>PreconditionFailed</Code><Message>exists</Message></Error>`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.objects[key] = body
		w.WriteHeader(http.StatusOK)
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

type fakeSTS struct{ account string }

func (f *fakeSTS) GetCallerIdentity(ctx context.Context, in *sts.GetCallerIdentityInput, opts ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	if f.account == "" {
		return nil, errors.New("no credentials")
	}
	return &sts.GetCallerIdentityOutput{Account: aws.String(f.account)}, nil
}

func TestResolveBucket(t *testing.T) {
	ctx := context.Background()
	got, err := ResolveBucket(ctx, config.S3Settings{Bucket: "mine"}, &fakeSTS{})
	if err != nil || got != "mine" {
		t.Errorf("explicit bucket: %q, %v", got, err)
	}
	got, err = ResolveBucket(ctx, config.S3Settings{BucketPrefix: "chat-archive", Region: "us-west-2"}, &fakeSTS{account: "111122223333"})
	if err != nil || got != "chat-archive-111122223333-us-west-2-an" {
		t.Errorf("namespaced bucket: %q, %v", got, err)
	}
	if _, err := ResolveBucket(ctx, config.S3Settings{BucketPrefix: "p"}, &fakeSTS{account: "1"}); err == nil {
		t.Error("no region: want an error")
	}
	if _, err := ResolveBucket(ctx, config.S3Settings{BucketPrefix: "p", Region: "us-west-2"}, &fakeSTS{}); err == nil {
		t.Error("no credentials: want an error")
	}
}

func TestS3RequestsCarryTheAppID(t *testing.T) {
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
