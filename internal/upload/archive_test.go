package upload_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"

	"github.com/testimony-dev/testimony/internal/upload"
)

func TestDetectArchiveFormatPrefersMagicBytesOverFilename(t *testing.T) {
	zipBytes := buildZipArchive(t, map[string]string{
		"widgets-result.json": `{"name":"widgets"}`,
	})

	format, err := upload.DetectArchiveFormat(zipBytes[:8], "application/gzip", "results.tar.gz")
	if err != nil {
		t.Fatalf("DetectArchiveFormat() error = %v", err)
	}
	if got, want := format, upload.ArchiveFormatZIP; got != want {
		t.Fatalf("format = %q, want %q", got, want)
	}
}

func TestPrepareArchiveSupportsZipAndTarGz(t *testing.T) {
	tests := []struct {
		name        string
		filename    string
		contentType string
		archive     func(t *testing.T) []byte
		wantFormat  upload.ArchiveFormat
		wantFile    string
	}{
		{
			name:        "zip",
			filename:    "results.bin",
			contentType: "application/octet-stream",
			archive: func(t *testing.T) []byte {
				return buildZipArchive(t, map[string]string{
					"suite/widgets-result.json": `{"name":"widgets"}`,
				})
			},
			wantFormat: upload.ArchiveFormatZIP,
			wantFile:   filepath.Join("suite", "widgets-result.json"),
		},
		{
			name:        "tar.gz",
			filename:    "results.unknown",
			contentType: "application/gzip",
			archive: func(t *testing.T) []byte {
				return buildTarGzArchive(t, map[string]string{
					"suite/widgets-container.json": `{"uuid":"123"}`,
				})
			},
			wantFormat: upload.ArchiveFormatTarGz,
			wantFile:   filepath.Join("suite", "widgets-container.json"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			archivePath := filepath.Join(t.TempDir(), tt.filename)
			if err := os.WriteFile(archivePath, tt.archive(t), 0o644); err != nil {
				t.Fatalf("os.WriteFile(%q) error = %v", archivePath, err)
			}

			prepared, err := upload.PrepareArchive(t.TempDir(), archivePath, tt.filename, tt.contentType)
			if err != nil {
				t.Fatalf("PrepareArchive() error = %v", err)
			}
			defer os.RemoveAll(prepared.ExtractedDir)

			if got, want := prepared.Format, tt.wantFormat; got != want {
				t.Fatalf("prepared.Format = %q, want %q", got, want)
			}
			if got, want := prepared.SourceFilename, tt.filename; got != want {
				t.Fatalf("prepared.SourceFilename = %q, want %q", got, want)
			}
			if _, err := os.Stat(filepath.Join(prepared.ExtractedDir, tt.wantFile)); err != nil {
				t.Fatalf("expected extracted file %q: %v", tt.wantFile, err)
			}
		})
	}
}

func TestPrepareArchiveRejectsPathTraversal(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "results.zip")
	payload := buildZipArchive(t, map[string]string{
		"../evil-result.json": `{"name":"oops"}`,
	})
	if err := os.WriteFile(archivePath, payload, 0o644); err != nil {
		t.Fatalf("os.WriteFile(%q) error = %v", archivePath, err)
	}

	_, err := upload.PrepareArchive(t.TempDir(), archivePath, "results.zip", "application/zip")
	if err == nil || err.Error() != `archive entry "../evil-result.json" escapes extraction root` {
		t.Fatalf("PrepareArchive() error = %v, want traversal rejection", err)
	}
}

func TestPrepareArchiveRejectsArchiveWithoutAllureSignals(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "results.zip")
	payload := buildZipArchive(t, map[string]string{
		"README.txt": "not allure",
	})
	if err := os.WriteFile(archivePath, payload, 0o644); err != nil {
		t.Fatalf("os.WriteFile(%q) error = %v", archivePath, err)
	}

	_, err := upload.PrepareArchive(t.TempDir(), archivePath, "results.zip", "application/zip")
	if err == nil || err.Error() != "archive does not look like Allure results: no known result files found" {
		t.Fatalf("PrepareArchive() error = %v, want allure validation error", err)
	}
}

func buildZipArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()

	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	for name, contents := range files {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatalf("zip.Create(%q) error = %v", name, err)
		}
		if _, err := entry.Write([]byte(contents)); err != nil {
			t.Fatalf("zip write %q error = %v", name, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("zip.Close() error = %v", err)
	}
	return buf.Bytes()
}

func buildTarGzArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()

	var buf bytes.Buffer
	gzipWriter := gzip.NewWriter(&buf)
	tarWriter := tar.NewWriter(gzipWriter)
	for name, contents := range files {
		payload := []byte(contents)
		header := &tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(payload)),
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatalf("tar.WriteHeader(%q) error = %v", name, err)
		}
		if _, err := tarWriter.Write(payload); err != nil {
			t.Fatalf("tar write %q error = %v", name, err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatalf("tar.Close() error = %v", err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatalf("gzip.Close() error = %v", err)
	}
	return buf.Bytes()
}
