package db

import (
	"errors"
	"strings"
	"time"
)

const timestampLayout = time.RFC3339Nano

type ReportStatus string

const (
	ReportStatusPending    ReportStatus = "pending"
	ReportStatusProcessing ReportStatus = "processing"
	ReportStatusReady      ReportStatus = "ready"
	ReportStatusFailed     ReportStatus = "failed"
)

var (
	ErrProjectNotFound = errors.New("project not found")
	ErrReportNotFound  = errors.New("report not found")
	ErrAPIKeyNotFound  = errors.New("api key not found")
)

type Project struct {
	ID            string
	Slug          string
	RetentionDays *int
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type APIKey struct {
	ID        string
	Name      string
	KeyHash   string
	CreatedAt time.Time
	UpdatedAt time.Time
}

const APIKeyBootstrapName = "bootstrap"

type Report struct {
	ID                 string
	ProjectID          string
	ProjectSlug        string
	Status             ReportStatus
	ArchiveObjectKey   string
	GeneratedObjectKey string
	ArchiveFormat      string
	SourceFilename     string
	ErrorMessage       string
	CreatedAt          time.Time
	UpdatedAt          time.Time
	StartedAt          *time.Time
	CompletedAt        *time.Time
}

type CreateReportParams struct {
	ReportID         string
	ProjectSlug      string
	ArchiveObjectKey string
	ArchiveFormat    string
	SourceFilename   string
}

type UpdateReportStatusParams struct {
	ReportID            string
	Status              ReportStatus
	GeneratedObjectKey  string
	ErrorMessage        string
	CompletedAtOverride *time.Time
}

func (s ReportStatus) Valid() bool {
	switch s {
	case ReportStatusPending, ReportStatusProcessing, ReportStatusReady, ReportStatusFailed:
		return true
	default:
		return false
	}
}

func normalizeSlug(slug string) string {
	return strings.TrimSpace(slug)
}

func nowUTC() time.Time {
	return time.Now().UTC()
}

func formatTimestamp(t time.Time) string {
	return t.UTC().Format(timestampLayout)
}
