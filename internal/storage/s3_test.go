package storage_test

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/testimony-dev/testimony/internal/config"
	"github.com/testimony-dev/testimony/internal/storage"
)

func TestS3StoreEnsureBucketAndCRUD(t *testing.T) {
	ctx := context.Background()
	fake := newFakeS3Server("testimony")
	httpServer := httptest.NewServer(http.HandlerFunc(fake.serveHTTP))
	defer httpServer.Close()

	store, err := storage.NewS3Store(ctx, config.S3Config{
		Endpoint:        httpServer.URL,
		Region:          "us-east-1",
		Bucket:          "testimony",
		AccessKeyID:     "minioadmin",
		SecretAccessKey: "minioadmin",
		UsePathStyle:    true,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewS3Store() error = %v", err)
	}

	if !fake.sawHeadBucket() {
		t.Fatal("expected HeadBucket request during store initialization")
	}
	if !fake.sawCreateBucket() {
		t.Fatal("expected CreateBucket request during store initialization")
	}

	key := "projects/alpha/reports/report-123/archive.zip"
	payload := []byte("zip-data")
	if err := store.Upload(ctx, key, bytes.NewReader(payload), int64(len(payload)), "application/zip"); err != nil {
		t.Fatalf("Upload() error = %v", err)
	}

	if got := fake.objectBody(key); !bytes.Equal(got, payload) {
		t.Fatalf("stored payload = %q, want %q", got, payload)
	}
	if got, want := fake.objectContentType(key), "application/zip"; got != want {
		t.Fatalf("stored content type = %q, want %q", got, want)
	}
	if !fake.usedPathStyleKey(key) {
		t.Fatalf("expected path-style request for key %q; saw paths %v", key, fake.paths())
	}

	result, err := store.Download(ctx, key)
	if err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	defer result.Body.Close()

	downloaded, err := io.ReadAll(result.Body)
	if err != nil {
		t.Fatalf("io.ReadAll(downloaded) error = %v", err)
	}
	if !bytes.Equal(downloaded, payload) {
		t.Fatalf("downloaded payload = %q, want %q", downloaded, payload)
	}
	if got, want := result.ContentType, "application/zip"; got != want {
		t.Fatalf("download content type = %q, want %q", got, want)
	}
	if got, want := result.ContentLength, int64(len(payload)); got != want {
		t.Fatalf("download content length = %d, want %d", got, want)
	}

	objects, err := store.List(ctx, "projects/alpha/")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(objects) != 1 {
		t.Fatalf("len(List()) = %d, want 1", len(objects))
	}
	if got, want := objects[0].Key, key; got != want {
		t.Fatalf("List()[0].Key = %q, want %q", got, want)
	}
	if got, want := objects[0].Size, int64(len(payload)); got != want {
		t.Fatalf("List()[0].Size = %d, want %d", got, want)
	}

	if err := store.Delete(ctx, key); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if got := fake.objectBody(key); got != nil {
		t.Fatalf("object still present after delete: %q", got)
	}
}

func TestNewS3StoreRejectsEmptyBucket(t *testing.T) {
	_, err := storage.NewS3Store(context.Background(), config.S3Config{
		Endpoint:        "http://127.0.0.1:9000",
		Region:          "us-east-1",
		Bucket:          " ",
		AccessKeyID:     "minioadmin",
		SecretAccessKey: "minioadmin",
		UsePathStyle:    true,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil || !strings.Contains(err.Error(), "empty bucket") {
		t.Fatalf("NewS3Store(empty bucket) error = %v, want empty bucket validation", err)
	}
}

type fakeS3Server struct {
	mu             sync.Mutex
	bucket         string
	bucketExists   bool
	headBucketSeen bool
	createSeen     bool
	objects        map[string][]byte
	contentTypes   map[string]string
	requestPaths   []string
}

func newFakeS3Server(bucket string) *fakeS3Server {
	return &fakeS3Server{
		bucket:       bucket,
		objects:      make(map[string][]byte),
		contentTypes: make(map[string]string),
		requestPaths: make([]string, 0),
	}
}

func (f *fakeS3Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requestPaths = append(f.requestPaths, r.URL.Path)
	f.mu.Unlock()

	switch {
	case r.Method == http.MethodHead && r.URL.Path == "/"+f.bucket:
		f.mu.Lock()
		f.headBucketSeen = true
		exists := f.bucketExists
		f.mu.Unlock()
		if !exists {
			http.Error(w, "missing bucket", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodPut && r.URL.Path == "/"+f.bucket:
		f.mu.Lock()
		f.createSeen = true
		f.bucketExists = true
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/"+f.bucket+"/"):
		key := mustDecodeKey(strings.TrimPrefix(r.URL.EscapedPath(), "/"+f.bucket+"/"))
		payload, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		f.mu.Lock()
		f.objects[key] = payload
		f.contentTypes[key] = r.Header.Get("Content-Type")
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2":
		prefix := r.URL.Query().Get("prefix")
		f.mu.Lock()
		contents := make([]listObject, 0)
		for key, payload := range f.objects {
			if prefix != "" && !strings.HasPrefix(key, prefix) {
				continue
			}
			contents = append(contents, listObject{
				Key:          key,
				LastModified: time.Date(2026, time.March, 24, 12, 0, 0, 0, time.UTC).Format(time.RFC3339),
				ETag:         fmt.Sprintf("\"%s\"", key),
				Size:         int64(len(payload)),
				StorageClass: "STANDARD",
			})
		}
		f.mu.Unlock()
		sort.Slice(contents, func(i, j int) bool { return contents[i].Key < contents[j].Key })

		result := listBucketResult{
			XMLNS:       "http://s3.amazonaws.com/doc/2006-03-01/",
			Name:        f.bucket,
			Prefix:      prefix,
			MaxKeys:     1000,
			KeyCount:    len(contents),
			IsTruncated: false,
			Contents:    contents,
		}
		w.Header().Set("Content-Type", "application/xml")
		_ = xml.NewEncoder(w).Encode(result)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/"+f.bucket+"/"):
		key := mustDecodeKey(strings.TrimPrefix(r.URL.EscapedPath(), "/"+f.bucket+"/"))
		f.mu.Lock()
		payload, ok := f.objects[key]
		contentType := f.contentTypes[key]
		f.mu.Unlock()
		if !ok {
			http.Error(w, "missing object", http.StatusNotFound)
			return
		}
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
		_, _ = w.Write(payload)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/"+f.bucket+"/"):
		key := mustDecodeKey(strings.TrimPrefix(r.URL.EscapedPath(), "/"+f.bucket+"/"))
		f.mu.Lock()
		delete(f.objects, key)
		delete(f.contentTypes, key)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "unexpected request", http.StatusNotFound)
	}
}

func (f *fakeS3Server) sawHeadBucket() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.headBucketSeen
}

func (f *fakeS3Server) sawCreateBucket() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.createSeen
}

func (f *fakeS3Server) objectBody(key string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	payload, ok := f.objects[key]
	if !ok {
		return nil
	}
	return append([]byte(nil), payload...)
}

func (f *fakeS3Server) objectContentType(key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.contentTypes[key]
}

func (f *fakeS3Server) usedPathStyleKey(key string) bool {
	want := "/" + f.bucket + "/" + key
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, path := range f.requestPaths {
		if path == want {
			return true
		}
	}
	return false
}

func (f *fakeS3Server) paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requestPaths...)
}

func mustDecodeKey(value string) string {
	decoded, err := url.PathUnescape(value)
	if err != nil {
		return value
	}
	return decoded
}

type listBucketResult struct {
	XMLName     xml.Name     `xml:"ListBucketResult"`
	XMLNS       string       `xml:"xmlns,attr,omitempty"`
	Name        string       `xml:"Name"`
	Prefix      string       `xml:"Prefix,omitempty"`
	MaxKeys     int          `xml:"MaxKeys"`
	KeyCount    int          `xml:"KeyCount"`
	IsTruncated bool         `xml:"IsTruncated"`
	Contents    []listObject `xml:"Contents"`
}

type listObject struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
}
