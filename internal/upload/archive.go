package upload

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type ArchiveFormat string

const (
	ArchiveFormatZIP   ArchiveFormat = "zip"
	ArchiveFormatTarGz ArchiveFormat = "tar.gz"
)

type PreparedArchive struct {
	Format         ArchiveFormat
	ArchivePath    string
	ExtractedDir   string
	SourceFilename string
	Size           int64
}

func DetectArchiveFormat(header []byte, contentType, sourceFilename string) (ArchiveFormat, error) {
	if looksLikeZIP(header) {
		return ArchiveFormatZIP, nil
	}
	if looksLikeGZIP(header) {
		return ArchiveFormatTarGz, nil
	}

	switch normalizeContentType(contentType) {
	case "application/zip", "application/x-zip-compressed":
		return ArchiveFormatZIP, nil
	case "application/gzip", "application/x-gzip", "application/x-tgz", "application/x-tar+gzip":
		return ArchiveFormatTarGz, nil
	}

	lowerName := strings.ToLower(strings.TrimSpace(sourceFilename))
	switch {
	case strings.HasSuffix(lowerName, ".zip"):
		return ArchiveFormatZIP, nil
	case strings.HasSuffix(lowerName, ".tar.gz"), strings.HasSuffix(lowerName, ".tgz"):
		return ArchiveFormatTarGz, nil
	default:
		return "", fmt.Errorf("unsupported archive format: expected ZIP or tar.gz")
	}
}

func PrepareArchive(tempRoot, archivePath, sourceFilename, contentType string) (PreparedArchive, error) {
	cleanArchivePath := filepath.Clean(strings.TrimSpace(archivePath))
	if cleanArchivePath == "" || cleanArchivePath == "." {
		return PreparedArchive{}, fmt.Errorf("prepare archive: empty archive path")
	}
	if strings.TrimSpace(tempRoot) == "" {
		return PreparedArchive{}, fmt.Errorf("prepare archive %q: empty temp root", cleanArchivePath)
	}

	file, err := os.Open(cleanArchivePath)
	if err != nil {
		return PreparedArchive{}, fmt.Errorf("open archive %q: %w", cleanArchivePath, err)
	}
	defer file.Close()

	header := make([]byte, 512)
	headerBytes, readErr := io.ReadFull(file, header)
	if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) && !errors.Is(readErr, io.EOF) {
		return PreparedArchive{}, fmt.Errorf("read archive header %q: %w", cleanArchivePath, readErr)
	}

	format, err := DetectArchiveFormat(header[:headerBytes], contentType, sourceFilename)
	if err != nil {
		return PreparedArchive{}, err
	}

	stat, err := file.Stat()
	if err != nil {
		return PreparedArchive{}, fmt.Errorf("stat archive %q: %w", cleanArchivePath, err)
	}
	if stat.Size() == 0 {
		return PreparedArchive{}, fmt.Errorf("archive is empty")
	}

	extractedDir, err := os.MkdirTemp(tempRoot, "testimony-archive-*")
	if err != nil {
		return PreparedArchive{}, fmt.Errorf("create archive temp dir: %w", err)
	}

	if err := extractArchive(cleanArchivePath, extractedDir, format); err != nil {
		_ = os.RemoveAll(extractedDir)
		return PreparedArchive{}, err
	}
	if err := ValidateAllureResults(extractedDir); err != nil {
		_ = os.RemoveAll(extractedDir)
		return PreparedArchive{}, err
	}

	resolvedFilename := strings.TrimSpace(sourceFilename)
	if resolvedFilename == "" {
		resolvedFilename = defaultFilenameForFormat(format)
	}

	return PreparedArchive{
		Format:         format,
		ArchivePath:    cleanArchivePath,
		ExtractedDir:   extractedDir,
		SourceFilename: resolvedFilename,
		Size:           stat.Size(),
	}, nil
}

func ValidateAllureResults(root string) error {
	foundSignal := false
	filesSeen := 0

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		filesSeen++
		name := strings.ToLower(filepath.Base(path))
		if strings.HasSuffix(name, "-result.json") ||
			strings.HasSuffix(name, "-container.json") ||
			name == "executor.json" ||
			name == "categories.json" ||
			name == "environment.properties" ||
			name == "environment.xml" {
			foundSignal = true
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("walk extracted archive: %w", err)
	}
	if filesSeen == 0 {
		return fmt.Errorf("archive did not contain any files")
	}
	if !foundSignal {
		return fmt.Errorf("archive does not look like Allure results: no known result files found")
	}

	return nil
}

func extractArchive(archivePath, extractedDir string, format ArchiveFormat) error {
	switch format {
	case ArchiveFormatZIP:
		return extractZIP(archivePath, extractedDir)
	case ArchiveFormatTarGz:
		return extractTarGz(archivePath, extractedDir)
	default:
		return fmt.Errorf("unsupported archive format %q", format)
	}
}

func extractZIP(archivePath, extractedDir string) error {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("open zip archive %q: %w", archivePath, err)
	}
	defer reader.Close()

	for _, file := range reader.File {
		if file.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("zip archive contains unsupported symlink entry %q", file.Name)
		}

		targetPath, err := archiveEntryPath(extractedDir, file.Name)
		if err != nil {
			return err
		}

		if file.FileInfo().IsDir() {
			if err := os.MkdirAll(targetPath, 0o755); err != nil {
				return fmt.Errorf("create zip directory %q: %w", targetPath, err)
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
			return fmt.Errorf("create zip parent directory for %q: %w", targetPath, err)
		}

		src, err := file.Open()
		if err != nil {
			return fmt.Errorf("open zip entry %q: %w", file.Name, err)
		}

		if err := writeArchiveFile(targetPath, file.Mode(), src); err != nil {
			src.Close()
			return err
		}
		if err := src.Close(); err != nil {
			return fmt.Errorf("close zip entry %q: %w", file.Name, err)
		}
	}

	return nil
}

func extractTarGz(archivePath, extractedDir string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open tar.gz archive %q: %w", archivePath, err)
	}
	defer file.Close()

	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("open gzip stream %q: %w", archivePath, err)
	}
	defer gzipReader.Close()

	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read tar entry: %w", err)
		}

		targetPath, err := archiveEntryPath(extractedDir, header.Name)
		if err != nil {
			return err
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(targetPath, 0o755); err != nil {
				return fmt.Errorf("create tar directory %q: %w", targetPath, err)
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
				return fmt.Errorf("create tar parent directory for %q: %w", targetPath, err)
			}
			if err := writeArchiveFile(targetPath, os.FileMode(header.Mode), tarReader); err != nil {
				return err
			}
		case tar.TypeSymlink, tar.TypeLink:
			return fmt.Errorf("tar.gz archive contains unsupported link entry %q", header.Name)
		default:
			return fmt.Errorf("tar.gz archive contains unsupported entry %q of type %d", header.Name, header.Typeflag)
		}
	}
}

func writeArchiveFile(targetPath string, mode os.FileMode, src io.Reader) error {
	fileMode := mode.Perm()
	if fileMode == 0 {
		fileMode = 0o644
	}

	dst, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fileMode)
	if err != nil {
		return fmt.Errorf("create extracted file %q: %w", targetPath, err)
	}
	defer dst.Close()

	if _, err := io.Copy(dst, src); err != nil {
		return fmt.Errorf("write extracted file %q: %w", targetPath, err)
	}

	return nil
}

func archiveEntryPath(root, entryName string) (string, error) {
	trimmed := strings.TrimSpace(entryName)
	if trimmed == "" {
		return "", fmt.Errorf("archive contains empty entry name")
	}

	cleaned := filepath.Clean(trimmed)
	if cleaned == "." {
		return "", fmt.Errorf("archive contains invalid root entry %q", entryName)
	}
	if filepath.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("archive entry %q escapes extraction root", entryName)
	}

	targetPath := filepath.Join(root, cleaned)
	rel, err := filepath.Rel(root, targetPath)
	if err != nil {
		return "", fmt.Errorf("resolve archive entry %q: %w", entryName, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("archive entry %q escapes extraction root", entryName)
	}

	return targetPath, nil
}

func looksLikeZIP(header []byte) bool {
	return len(header) >= 4 && header[0] == 'P' && header[1] == 'K' && (header[2] == 3 || header[2] == 5 || header[2] == 7) && (header[3] == 4 || header[3] == 6 || header[3] == 8)
}

func looksLikeGZIP(header []byte) bool {
	return len(header) >= 2 && header[0] == 0x1f && header[1] == 0x8b
}

func normalizeContentType(contentType string) string {
	trimmed := strings.TrimSpace(contentType)
	if trimmed == "" {
		return ""
	}
	if idx := strings.Index(trimmed, ";"); idx >= 0 {
		trimmed = trimmed[:idx]
	}
	return strings.ToLower(strings.TrimSpace(trimmed))
}

func defaultFilenameForFormat(format ArchiveFormat) string {
	switch format {
	case ArchiveFormatZIP:
		return "upload.zip"
	case ArchiveFormatTarGz:
		return "upload.tar.gz"
	default:
		return "upload"
	}
}
