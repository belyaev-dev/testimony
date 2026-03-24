package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/testimony-dev/testimony/internal/config"
	"github.com/testimony-dev/testimony/internal/db"
)

func TestGracefulShutdown(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	storageBackend := newRuntimeS3Server("testimony")
	s3Server := httptest.NewServer(http.HandlerFunc(storageBackend.serveHTTP))
	defer s3Server.Close()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}

	stdout := &bytes.Buffer{}
	slowStarted := make(chan struct{})
	releaseSlow := make(chan struct{})
	slowDone := make(chan struct{})
	runErrCh := make(chan error, 1)

	go func() {
		runErrCh <- runWithOptions(ctx, runOptions{
			stdout:   stdout,
			lookup:   staticLookup(runtimeEnv(t, s3Server.URL, filepath.Join(t.TempDir(), "testimony.sqlite"), filepath.Join(t.TempDir(), "tmp"), nil)),
			listener: listener,
			registerRoutes: func(r chi.Router) {
				r.Get("/slow", func(w http.ResponseWriter, _ *http.Request) {
					close(slowStarted)
					<-releaseSlow
					_, _ = w.Write([]byte("drained"))
					close(slowDone)
				})
			},
		})
	}()

	baseURL := "http://" + listener.Addr().String()
	waitForHTTPStatus(t, baseURL+"/readyz", http.StatusOK, 5*time.Second)

	slowRespCh := make(chan struct {
		status int
		body   string
		err    error
	}, 1)
	go func() {
		resp, err := http.Get(baseURL + "/slow")
		if err != nil {
			slowRespCh <- struct {
				status int
				body   string
				err    error
			}{err: err}
			return
		}
		defer resp.Body.Close()

		body, readErr := io.ReadAll(resp.Body)
		slowRespCh <- struct {
			status int
			body   string
			err    error
		}{status: resp.StatusCode, body: string(body), err: readErr}
	}()

	select {
	case <-slowStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("slow request did not start")
	}

	cancel()
	waitForHTTPStatus(t, baseURL+"/readyz", http.StatusServiceUnavailable, 2*time.Second)

	readyResp, err := http.Get(baseURL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz during drain error = %v", err)
	}
	defer readyResp.Body.Close()

	if got, want := readyResp.StatusCode, http.StatusServiceUnavailable; got != want {
		t.Fatalf("/readyz status during drain = %d, want %d", got, want)
	}

	var readyBody map[string]string
	if err := json.NewDecoder(readyResp.Body).Decode(&readyBody); err != nil {
		t.Fatalf("decode /readyz body error = %v", err)
	}
	if got, want := readyBody["reason"], "draining"; got != want {
		t.Fatalf("/readyz reason during drain = %q, want %q", got, want)
	}

	select {
	case result := <-slowRespCh:
		t.Fatalf("slow request completed too early: status=%d body=%q err=%v", result.status, result.body, result.err)
	case <-time.After(200 * time.Millisecond):
	}

	close(releaseSlow)

	select {
	case result := <-slowRespCh:
		if result.err != nil {
			t.Fatalf("slow request error = %v", result.err)
		}
		if got, want := result.status, http.StatusOK; got != want {
			t.Fatalf("slow request status = %d, want %d", got, want)
		}
		if got, want := result.body, "drained"; got != want {
			t.Fatalf("slow request body = %q, want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("slow request did not drain before timeout")
	}

	select {
	case <-slowDone:
	case <-time.After(5 * time.Second):
		t.Fatal("slow handler did not finish")
	}

	select {
	case err := <-runErrCh:
		if err != nil {
			t.Fatalf("runWithOptions() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not shut down")
	}

	logOutput := stdout.String()
	for _, fragment := range []string{"shutdown requested", "shutdown drain started", "shutdown complete"} {
		if !strings.Contains(logOutput, fragment) {
			t.Fatalf("expected log output to contain %q, got %q", fragment, logOutput)
		}
	}
}

func TestRunWithOptionsEnforcesUploadAuthAndRunsRetentionCleanup(t *testing.T) {
	successCLI := writeRuntimeCLI(t, "allure-auth-retention.sh", `#!/bin/sh
set -eu
output=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    generate)
      shift
      ;;
    --clean)
      shift
      ;;
    -o)
      output="$2"
      shift 2
      ;;
    *)
      shift
      ;;
  esac
done
mkdir -p "$output/assets"
printf '<html><script src="./assets/app.js"></script>generated</html>' > "$output/index.html"
printf 'console.log("runtime")' > "$output/assets/app.js"
`)
	payload := buildRuntimeZipArchive(t, map[string]string{
		"widgets-result.json": `{"name":"widgets"}`,
	})
	const apiKey = "bootstrap-key-test"

	t.Run("viewer routes stay open by default and retention cleanup runs", func(t *testing.T) {
		runtime := startRuntimeServer(t, map[string]string{
			config.EnvGenerateCLIPath:          successCLI,
			config.EnvAuthEnabled:              "true",
			config.EnvAuthAPIKey:               apiKey,
			config.EnvRetentionDays:            "1",
			config.EnvRetentionCleanupInterval: "50ms",
		})
		stopped := false
		defer func() {
			if !stopped {
				runtime.stop(t)
			}
		}()

		waitForHTTPStatus(t, runtime.baseURL+"/healthz", http.StatusOK, 5*time.Second)
		waitForHTTPStatus(t, runtime.baseURL+"/readyz", http.StatusOK, 5*time.Second)

		unauthorizedResp := runtimeUploadArchiveRequest(t, runtime.baseURL, "widgets", payload, "")
		if got, want := unauthorizedResp.StatusCode, http.StatusUnauthorized; got != want {
			body := readRuntimeResponseBody(t, unauthorizedResp)
			unauthorizedResp.Body.Close()
			t.Fatalf("unauthorized upload status = %d, want %d, body=%q", got, want, body)
		}
		if got, want := unauthorizedResp.Header.Get("WWW-Authenticate"), "Bearer"; got != want {
			body := readRuntimeResponseBody(t, unauthorizedResp)
			unauthorizedResp.Body.Close()
			t.Fatalf("unauthorized upload challenge = %q, want %q, body=%q", got, want, body)
		}
		body := readRuntimeResponseBody(t, unauthorizedResp)
		unauthorizedResp.Body.Close()
		if !strings.Contains(body, http.StatusText(http.StatusUnauthorized)) {
			t.Fatalf("unauthorized upload body = %q, want unauthorized marker", body)
		}
		waitForLogFragment(t, runtime.stdout, "auth rejected")
		waitForLogFragment(t, runtime.stdout, "/api/v1/projects/widgets/upload")
		if strings.Contains(runtime.stdout.String(), apiKey) {
			t.Fatalf("runtime logs leaked bootstrap api key: %q", runtime.stdout.String())
		}

		response := uploadRuntimeArchiveWithAPIKey(t, runtime.baseURL, "widgets", payload, apiKey)
		store := openRuntimeSQLiteStore(t, runtime.sqlitePath)
		defer store.Close()

		report := waitForRuntimeReportStatus(t, store, response.ReportID, db.ReportStatusReady)
		assetKey := "projects/widgets/reports/" + report.ID + "/html/assets/app.js"

		indexResp := runtimeGetRequest(t, runtime.baseURL, "/", "")
		if got, want := indexResp.StatusCode, http.StatusOK; got != want {
			body := readRuntimeResponseBody(t, indexResp)
			indexResp.Body.Close()
			t.Fatalf("GET / status = %d, want %d, body=%q", got, want, body)
		}
		indexResp.Body.Close()

		projectResp := runtimeGetRequest(t, runtime.baseURL, "/projects/widgets", "")
		if got, want := projectResp.StatusCode, http.StatusOK; got != want {
			body := readRuntimeResponseBody(t, projectResp)
			projectResp.Body.Close()
			t.Fatalf("GET /projects/widgets status = %d, want %d, body=%q", got, want, body)
		}
		projectResp.Body.Close()

		reportResp := runtimeGetRequest(t, runtime.baseURL, "/reports/widgets/"+report.ID+"/", "")
		if got, want := reportResp.StatusCode, http.StatusOK; got != want {
			body := readRuntimeResponseBody(t, reportResp)
			reportResp.Body.Close()
			t.Fatalf("GET report root status = %d, want %d, body=%q", got, want, body)
		}
		reportBody := readRuntimeResponseBody(t, reportResp)
		reportResp.Body.Close()
		if !strings.Contains(reportBody, "generated") {
			t.Fatalf("report root body = %q, want generated marker", reportBody)
		}

		expiredAt := time.Now().UTC().Add(-48 * time.Hour)
		if _, err := store.UpdateReportStatus(context.Background(), db.UpdateReportStatusParams{
			ReportID:            report.ID,
			Status:              db.ReportStatusReady,
			GeneratedObjectKey:  report.GeneratedObjectKey,
			CompletedAtOverride: &expiredAt,
		}); err != nil {
			t.Fatalf("UpdateReportStatus(expired ready) error = %v", err)
		}

		waitForRuntimeReportMissing(t, store, report.ID)
		waitForRuntimeObjectDeleted(t, runtime.storage, report.GeneratedObjectKey)
		waitForRuntimeObjectDeleted(t, runtime.storage, assetKey)
		waitForRuntimeObjectDeleted(t, runtime.storage, report.ArchiveObjectKey)
		waitForLogFragment(t, runtime.stdout, "retention cleanup succeeded")
		waitForLogFragment(t, runtime.stdout, report.ID)

		runtime.stop(t)
		stopped = true

		for _, fragment := range []string{"bootstrap api key ready", "retention worker started", "retention worker stopped", "shutdown requested", "shutdown complete"} {
			if !strings.Contains(runtime.stdout.String(), fragment) {
				t.Fatalf("expected runtime logs to contain %q, got %q", fragment, runtime.stdout.String())
			}
		}
		if strings.Contains(runtime.stdout.String(), apiKey) {
			t.Fatalf("runtime logs leaked bootstrap api key after shutdown: %q", runtime.stdout.String())
		}
	})

	t.Run("viewer auth toggle protects browse and report routes", func(t *testing.T) {
		runtime := startRuntimeServer(t, map[string]string{
			config.EnvGenerateCLIPath:          successCLI,
			config.EnvAuthEnabled:              "true",
			config.EnvAuthAPIKey:               apiKey,
			config.EnvAuthRequireViewer:        "true",
			config.EnvRetentionCleanupInterval: "1h",
		})
		stopped := false
		defer func() {
			if !stopped {
				runtime.stop(t)
			}
		}()

		waitForHTTPStatus(t, runtime.baseURL+"/healthz", http.StatusOK, 5*time.Second)
		waitForHTTPStatus(t, runtime.baseURL+"/readyz", http.StatusOK, 5*time.Second)

		response := uploadRuntimeArchiveWithAPIKey(t, runtime.baseURL, "widgets", payload, apiKey)
		store := openRuntimeSQLiteStore(t, runtime.sqlitePath)
		defer store.Close()
		report := waitForRuntimeReportStatus(t, store, response.ReportID, db.ReportStatusReady)

		for _, path := range []string{"/", "/projects/widgets", "/reports/widgets/" + report.ID + "/"} {
			resp := runtimeGetRequest(t, runtime.baseURL, path, "")
			if got, want := resp.StatusCode, http.StatusUnauthorized; got != want {
				body := readRuntimeResponseBody(t, resp)
				resp.Body.Close()
				t.Fatalf("GET %s status = %d, want %d, body=%q", path, got, want, body)
			}
			if got, want := resp.Header.Get("WWW-Authenticate"), "Bearer"; got != want {
				body := readRuntimeResponseBody(t, resp)
				resp.Body.Close()
				t.Fatalf("GET %s challenge = %q, want %q, body=%q", path, got, want, body)
			}
			resp.Body.Close()
		}

		rootResp := runtimeGetRequest(t, runtime.baseURL, "/", apiKey)
		if got, want := rootResp.StatusCode, http.StatusOK; got != want {
			body := readRuntimeResponseBody(t, rootResp)
			rootResp.Body.Close()
			t.Fatalf("authorized GET / status = %d, want %d, body=%q", got, want, body)
		}
		rootResp.Body.Close()

		reportResp := runtimeGetRequest(t, runtime.baseURL, "/reports/widgets/"+report.ID+"/", apiKey)
		if got, want := reportResp.StatusCode, http.StatusOK; got != want {
			body := readRuntimeResponseBody(t, reportResp)
			reportResp.Body.Close()
			t.Fatalf("authorized GET report root status = %d, want %d, body=%q", got, want, body)
		}
		reportResp.Body.Close()

		waitForLogFragment(t, runtime.stdout, "auth rejected")
		waitForLogFragment(t, runtime.stdout, "/projects/widgets")
		if strings.Contains(runtime.stdout.String(), apiKey) {
			t.Fatalf("runtime logs leaked bootstrap api key: %q", runtime.stdout.String())
		}

		runtime.stop(t)
		stopped = true

		for _, fragment := range []string{"retention worker started", "retention worker stopped"} {
			if !strings.Contains(runtime.stdout.String(), fragment) {
				t.Fatalf("expected runtime logs to contain %q, got %q", fragment, runtime.stdout.String())
			}
		}
	})
}

func TestRunWithOptionsStartsGenerationPipeline(t *testing.T) {
	successCLI := writeRuntimeCLI(t, "allure-success.sh", `#!/bin/sh
set -eu
output=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    generate)
      shift
      ;;
    --clean)
      shift
      ;;
    -o)
      output="$2"
      shift 2
      ;;
    *)
      shift
      ;;
  esac
done
mkdir -p "$output/history"
printf '<html>generated</html>' > "$output/index.html"
printf 'history' > "$output/history/history-trend.json"
`)

	runtime := startRuntimeServer(t, map[string]string{
		config.EnvGenerateCLIPath: successCLI,
	})
	defer runtime.stop(t)

	payload := buildRuntimeZipArchive(t, map[string]string{
		"widgets-result.json": `{"name":"widgets"}`,
	})
	response := uploadRuntimeArchive(t, runtime.baseURL, "widgets", payload)
	if got, want := response.Status, db.ReportStatusPending; got != want {
		t.Fatalf("upload response status = %q, want %q", got, want)
	}

	store := openRuntimeSQLiteStore(t, runtime.sqlitePath)
	defer store.Close()

	report := waitForRuntimeReportStatus(t, store, response.ReportID, db.ReportStatusReady)
	if got, want := report.GeneratedObjectKey, "projects/widgets/reports/"+response.ReportID+"/html/index.html"; got != want {
		t.Fatalf("report.GeneratedObjectKey = %q, want %q", got, want)
	}
	if report.StartedAt == nil {
		t.Fatal("report.StartedAt = nil, want value")
	}
	if report.CompletedAt == nil {
		t.Fatal("report.CompletedAt = nil, want value")
	}
	if got := report.ErrorMessage; got != "" {
		t.Fatalf("report.ErrorMessage = %q, want empty", got)
	}

	if got := string(runtime.storage.object(report.GeneratedObjectKey)); got != "<html>generated</html>" {
		t.Fatalf("generated object body = %q, want generated html", got)
	}
	waitForLogFragment(t, runtime.stdout, "report_generation_started")
	waitForLogFragment(t, runtime.stdout, "report_generation_completed")
	waitForLogFragment(t, runtime.stdout, response.ReportID)
	waitForLogFragment(t, runtime.stdout, "widgets")
}

func TestRunWithOptionsServesReportRoutes(t *testing.T) {
	successCLI := writeRuntimeCLI(t, "allure-proxy.sh", `#!/bin/sh
set -eu
output=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    generate)
      shift
      ;;
    --clean)
      shift
      ;;
    -o)
      output="$2"
      shift 2
      ;;
    *)
      shift
      ;;
  esac
done
mkdir -p "$output/assets"
printf '<html><script src="./assets/app.js"></script>generated</html>' > "$output/index.html"
printf 'console.log("runtime")' > "$output/assets/app.js"
`)

	runtime := startRuntimeServer(t, map[string]string{
		config.EnvGenerateCLIPath: successCLI,
	})
	defer runtime.stop(t)

	payload := buildRuntimeZipArchive(t, map[string]string{
		"widgets-result.json": `{"name":"widgets"}`,
	})
	response := uploadRuntimeArchive(t, runtime.baseURL, "widgets", payload)
	store := openRuntimeSQLiteStore(t, runtime.sqlitePath)
	defer store.Close()

	report := waitForRuntimeReportStatus(t, store, response.ReportID, db.ReportStatusReady)

	indexResp, err := http.Get(runtime.baseURL + "/")
	if err != nil {
		t.Fatalf("GET project index error = %v", err)
	}
	defer indexResp.Body.Close()

	if got, want := indexResp.StatusCode, http.StatusOK; got != want {
		t.Fatalf("project index status = %d, want %d", got, want)
	}
	indexBody, err := io.ReadAll(indexResp.Body)
	if err != nil {
		t.Fatalf("io.ReadAll(project index) error = %v", err)
	}
	for _, fragment := range []string{"Browse generated reports", "/projects/widgets", "widgets"} {
		if !strings.Contains(string(indexBody), fragment) {
			t.Fatalf("project index missing %q in %q", fragment, string(indexBody))
		}
	}

	projectResp, err := http.Get(runtime.baseURL + "/projects/widgets")
	if err != nil {
		t.Fatalf("GET project page error = %v", err)
	}
	defer projectResp.Body.Close()

	if got, want := projectResp.StatusCode, http.StatusOK; got != want {
		t.Fatalf("project page status = %d, want %d", got, want)
	}
	projectBody, err := io.ReadAll(projectResp.Body)
	if err != nil {
		t.Fatalf("io.ReadAll(project page) error = %v", err)
	}
	readyLink := "/reports/widgets/" + report.ID + "/"
	for _, fragment := range []string{"Project history", report.ID, "ready", readyLink} {
		if !strings.Contains(string(projectBody), fragment) {
			t.Fatalf("project page missing %q in %q", fragment, string(projectBody))
		}
	}

	redirectClient := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	redirectResp, err := redirectClient.Get(runtime.baseURL + "/reports/widgets/" + report.ID)
	if err != nil {
		t.Fatalf("GET bare report route error = %v", err)
	}
	defer redirectResp.Body.Close()

	if got, want := redirectResp.StatusCode, http.StatusFound; got != want {
		t.Fatalf("bare report status = %d, want %d", got, want)
	}
	if got, want := redirectResp.Header.Get("Location"), "/reports/widgets/"+report.ID+"/"; got != want {
		t.Fatalf("bare report location = %q, want %q", got, want)
	}

	rootResp, err := http.Get(runtime.baseURL + "/reports/widgets/" + report.ID + "/")
	if err != nil {
		t.Fatalf("GET report root error = %v", err)
	}
	defer rootResp.Body.Close()

	if got, want := rootResp.StatusCode, http.StatusOK; got != want {
		t.Fatalf("report root status = %d, want %d", got, want)
	}
	rootBody, err := io.ReadAll(rootResp.Body)
	if err != nil {
		t.Fatalf("io.ReadAll(report root) error = %v", err)
	}
	if !strings.Contains(string(rootBody), "generated") {
		t.Fatalf("report root body = %q, want generated marker", string(rootBody))
	}
	if got := rootResp.Header.Get("Content-Type"); !strings.Contains(got, "text/html") {
		t.Fatalf("report root content-type = %q, want text/html", got)
	}

	assetResp, err := http.Get(runtime.baseURL + "/reports/widgets/" + report.ID + "/assets/app.js")
	if err != nil {
		t.Fatalf("GET report asset error = %v", err)
	}
	defer assetResp.Body.Close()

	if got, want := assetResp.StatusCode, http.StatusOK; got != want {
		t.Fatalf("report asset status = %d, want %d", got, want)
	}
	assetBody, err := io.ReadAll(assetResp.Body)
	if err != nil {
		t.Fatalf("io.ReadAll(report asset) error = %v", err)
	}
	if got, want := string(assetBody), `console.log("runtime")`; got != want {
		t.Fatalf("report asset body = %q, want %q", got, want)
	}
	if got := assetResp.Header.Get("Content-Type"); got == "" {
		t.Fatal("report asset content-type = empty, want passthrough metadata")
	}
}

func TestRunWithOptionsExposesGenerationFailureState(t *testing.T) {
	failureCLI := writeRuntimeCLI(t, "allure-fail.sh", "#!/bin/sh\necho 'fixture boom' >&2\nexit 23\n")

	runtime := startRuntimeServer(t, map[string]string{
		config.EnvGenerateCLIPath: failureCLI,
	})
	defer runtime.stop(t)

	payload := buildRuntimeZipArchive(t, map[string]string{
		"widgets-result.json": `{"name":"widgets"}`,
	})
	response := uploadRuntimeArchive(t, runtime.baseURL, "widgets", payload)
	if got, want := response.Status, db.ReportStatusPending; got != want {
		t.Fatalf("upload response status = %q, want %q", got, want)
	}

	store := openRuntimeSQLiteStore(t, runtime.sqlitePath)
	defer store.Close()

	report := waitForRuntimeReportStatus(t, store, response.ReportID, db.ReportStatusFailed)
	if got := report.GeneratedObjectKey; got != "" {
		t.Fatalf("report.GeneratedObjectKey = %q, want empty", got)
	}
	if report.StartedAt == nil {
		t.Fatal("report.StartedAt = nil, want value")
	}
	if report.CompletedAt == nil {
		t.Fatal("report.CompletedAt = nil, want value")
	}
	if got := report.ErrorMessage; !strings.Contains(got, "fixture boom") {
		t.Fatalf("report.ErrorMessage = %q, want stderr snippet", got)
	}

	waitForLogFragment(t, runtime.stdout, "report_generation_failed")
	waitForLogFragment(t, runtime.stdout, response.ReportID)
	waitForLogFragment(t, runtime.stdout, "fixture boom")
}

type runtimeServerInstance struct {
	cancel     context.CancelFunc
	runErrCh   chan error
	baseURL    string
	stdout     *bytes.Buffer
	storage    *runtimeS3Server
	s3Server   *httptest.Server
	sqlitePath string
}

func startRuntimeServer(t *testing.T, overrides map[string]string) runtimeServerInstance {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	storageBackend := newRuntimeS3Server("testimony")
	s3Server := httptest.NewServer(http.HandlerFunc(storageBackend.serveHTTP))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}

	sqlitePath := filepath.Join(t.TempDir(), "testimony.sqlite")
	tempDir := filepath.Join(t.TempDir(), "tmp")
	stdout := &bytes.Buffer{}
	runErrCh := make(chan error, 1)

	go func() {
		runErrCh <- runWithOptions(ctx, runOptions{
			stdout:   stdout,
			lookup:   staticLookup(runtimeEnv(t, s3Server.URL, sqlitePath, tempDir, overrides)),
			listener: listener,
		})
	}()

	baseURL := "http://" + listener.Addr().String()
	waitForHTTPStatus(t, baseURL+"/readyz", http.StatusOK, 5*time.Second)

	return runtimeServerInstance{
		cancel:     cancel,
		runErrCh:   runErrCh,
		baseURL:    baseURL,
		stdout:     stdout,
		storage:    storageBackend,
		s3Server:   s3Server,
		sqlitePath: sqlitePath,
	}
}

func (r runtimeServerInstance) stop(t *testing.T) {
	t.Helper()
	r.cancel()
	select {
	case err := <-r.runErrCh:
		if err != nil {
			t.Fatalf("runWithOptions() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runtime server did not stop")
	}
	r.s3Server.Close()
}

func runtimeEnv(t *testing.T, s3Endpoint, sqlitePath, tempDir string, overrides map[string]string) map[string]string {
	t.Helper()

	env := map[string]string{
		config.EnvServerHost:             "127.0.0.1",
		config.EnvServerPort:             "8080",
		config.EnvLogLevel:               "debug",
		config.EnvS3Endpoint:             s3Endpoint,
		config.EnvS3Region:               "us-east-1",
		config.EnvS3Bucket:               "testimony",
		config.EnvS3AccessKeyID:          "minioadmin",
		config.EnvS3SecretAccessKey:      "minioadmin",
		config.EnvS3UsePathStyle:         "true",
		config.EnvSQLitePath:             sqlitePath,
		config.EnvSQLiteBusyTimeout:      "5s",
		config.EnvGenerateVariant:        string(config.GenerateVariantAllure2),
		config.EnvGenerateCLIPath:        "allure",
		config.EnvGenerateTimeout:        "5s",
		config.EnvGenerateMaxConcurrency: "1",
		config.EnvGenerateHistoryDepth:   "5",
		config.EnvTempDir:                tempDir,
		config.EnvShutdownDrainDelay:     "1s",
		config.EnvShutdownTimeout:        "5s",
		config.EnvServerReadTimeout:      "5s",
		config.EnvServerWriteTimeout:     "5s",
		config.EnvServerIdleTimeout:      "5s",
	}
	for key, value := range overrides {
		env[key] = value
	}
	return env
}

func staticLookup(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func waitForHTTPStatus(t *testing.T, url string, want int, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == want {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for %s to return %d", url, want)
}

func uploadRuntimeArchive(t *testing.T, baseURL, projectSlug string, payload []byte) uploadResponseBody {
	t.Helper()
	return uploadRuntimeArchiveWithAPIKey(t, baseURL, projectSlug, payload, "")
}

func uploadRuntimeArchiveWithAPIKey(t *testing.T, baseURL, projectSlug string, payload []byte, apiKey string) uploadResponseBody {
	t.Helper()

	resp := runtimeUploadArchiveRequest(t, baseURL, projectSlug, payload, apiKey)
	defer resp.Body.Close()

	if got, want := resp.StatusCode, http.StatusAccepted; got != want {
		body := readRuntimeResponseBody(t, resp)
		t.Fatalf("upload status = %d, want %d, body=%q", got, want, body)
	}

	var body uploadResponseBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode upload response error = %v", err)
	}
	return body
}

func runtimeUploadArchiveRequest(t *testing.T, baseURL, projectSlug string, payload []byte, apiKey string) *http.Response {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, baseURL+"/api/v1/projects/"+projectSlug+"/upload", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	req.Header.Set("Content-Type", "application/zip")
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST upload error = %v", err)
	}
	return resp
}

func runtimeGetRequest(t *testing.T, baseURL, requestPath, apiKey string) *http.Response {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, baseURL+requestPath, nil)
	if err != nil {
		t.Fatalf("http.NewRequest(%q) error = %v", requestPath, err)
	}
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s error = %v", requestPath, err)
	}
	return resp
}

func readRuntimeResponseBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("io.ReadAll(response body) error = %v", err)
	}
	return string(body)
}

type uploadResponseBody struct {
	ReportID         string          `json:"report_id"`
	ProjectSlug      string          `json:"project_slug"`
	Status           db.ReportStatus `json:"status"`
	ArchiveFormat    string          `json:"archive_format"`
	ArchiveObjectKey string          `json:"archive_object_key"`
}

func openRuntimeSQLiteStore(t *testing.T, path string) *db.SQLiteStore {
	t.Helper()
	store, err := db.OpenSQLiteStore(context.Background(), path, 5*time.Second, nil)
	if err != nil {
		t.Fatalf("OpenSQLiteStore(%q) error = %v", path, err)
	}
	return store
}

func waitForRuntimeReportStatus(t *testing.T, store *db.SQLiteStore, reportID string, want db.ReportStatus) db.Report {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		report, err := store.GetReport(context.Background(), reportID)
		if err == nil && report.Status == want {
			return report
		}
		time.Sleep(25 * time.Millisecond)
	}

	report, err := store.GetReport(context.Background(), reportID)
	if err != nil {
		t.Fatalf("GetReport(%q) error = %v", reportID, err)
	}
	t.Fatalf("report %q status = %q, want %q", reportID, report.Status, want)
	panic("unreachable")
}

func waitForRuntimeReportMissing(t *testing.T, store *db.SQLiteStore, reportID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, err := store.GetReport(context.Background(), reportID)
		if err != nil && strings.Contains(err.Error(), db.ErrReportNotFound.Error()) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}

	report, err := store.GetReport(context.Background(), reportID)
	if err == nil {
		t.Fatalf("report %q still present with status %q, want deleted", reportID, report.Status)
	}
	t.Fatalf("report %q was not deleted; last error = %v", reportID, err)
}

func waitForRuntimeObjectDeleted(t *testing.T, storage *runtimeS3Server, key string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if storage.object(key) == nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("object %q still present after retention cleanup", key)
}

func waitForLogFragment(t *testing.T, stdout *bytes.Buffer, fragment string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(stdout.String(), fragment) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("log output did not contain %q; logs=%q", fragment, stdout.String())
}

func buildRuntimeZipArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	for name, payload := range files {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatalf("Create(%q) error = %v", name, err)
		}
		if _, err := entry.Write([]byte(payload)); err != nil {
			t.Fatalf("Write(%q) error = %v", name, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("zip writer close error = %v", err)
	}
	return buf.Bytes()
}

func writeRuntimeCLI(t *testing.T, name, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
	return path
}

type runtimeS3Server struct {
	mu           sync.Mutex
	bucket       string
	bucketExists bool
	objects      map[string][]byte
	contentTypes map[string]string
}

type runtimeListBucketResult struct {
	XMLName     xml.Name            `xml:"ListBucketResult"`
	XMLNS       string              `xml:"xmlns,attr,omitempty"`
	Name        string              `xml:"Name"`
	Prefix      string              `xml:"Prefix,omitempty"`
	MaxKeys     int                 `xml:"MaxKeys"`
	KeyCount    int                 `xml:"KeyCount"`
	IsTruncated bool                `xml:"IsTruncated"`
	Contents    []runtimeListObject `xml:"Contents"`
}

type runtimeListObject struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
}

func newRuntimeS3Server(bucket string) *runtimeS3Server {
	return &runtimeS3Server{
		bucket:       bucket,
		objects:      make(map[string][]byte),
		contentTypes: make(map[string]string),
	}
}

func (s *runtimeS3Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	trimmedPath := strings.TrimPrefix(r.URL.Path, "/")
	parts := strings.SplitN(trimmedPath, "/", 2)
	bucket := ""
	objectKey := ""
	if len(parts) > 0 {
		bucket = parts[0]
	}
	if len(parts) == 2 {
		objectKey = mustDecodeRuntimeKey(parts[1])
	}

	switch {
	case r.Method == http.MethodHead && bucket == s.bucket && objectKey == "":
		s.mu.Lock()
		exists := s.bucketExists
		s.mu.Unlock()
		if !exists {
			http.Error(w, "missing bucket", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodPut && bucket == s.bucket && objectKey == "":
		s.mu.Lock()
		s.bucketExists = true
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodPut && bucket == s.bucket && objectKey != "":
		payload, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.objects[objectKey] = payload
		s.contentTypes[objectKey] = r.Header.Get("Content-Type")
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodGet && bucket == s.bucket && r.URL.Query().Get("list-type") == "2":
		prefix := r.URL.Query().Get("prefix")
		s.mu.Lock()
		contents := make([]runtimeListObject, 0)
		for key, payload := range s.objects {
			if prefix != "" && !strings.HasPrefix(key, prefix) {
				continue
			}
			contents = append(contents, runtimeListObject{
				Key:          key,
				LastModified: time.Date(2026, time.March, 24, 12, 0, 0, 0, time.UTC).Format(time.RFC3339),
				ETag:         fmt.Sprintf("\"%s\"", key),
				Size:         int64(len(payload)),
				StorageClass: "STANDARD",
			})
		}
		s.mu.Unlock()
		sort.Slice(contents, func(i, j int) bool { return contents[i].Key < contents[j].Key })

		result := runtimeListBucketResult{
			XMLNS:       "http://s3.amazonaws.com/doc/2006-03-01/",
			Name:        s.bucket,
			Prefix:      prefix,
			MaxKeys:     1000,
			KeyCount:    len(contents),
			IsTruncated: false,
			Contents:    contents,
		}
		w.Header().Set("Content-Type", "application/xml")
		_ = xml.NewEncoder(w).Encode(result)
	case r.Method == http.MethodGet && bucket == s.bucket && objectKey != "":
		s.mu.Lock()
		payload, ok := s.objects[objectKey]
		contentType := s.contentTypes[objectKey]
		s.mu.Unlock()
		if !ok {
			http.Error(w, fmt.Sprintf("missing object %s", objectKey), http.StatusNotFound)
			return
		}
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
		_, _ = w.Write(payload)
	case r.Method == http.MethodDelete && bucket == s.bucket && objectKey != "":
		s.mu.Lock()
		delete(s.objects, objectKey)
		delete(s.contentTypes, objectKey)
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, fmt.Sprintf("unexpected request %s %s", r.Method, r.URL.Path), http.StatusNotFound)
	}
}

func mustDecodeRuntimeKey(value string) string {
	decoded, err := url.PathUnescape(value)
	if err != nil {
		return value
	}
	return decoded
}

func (s *runtimeS3Server) object(key string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.objects[key]...)
}
