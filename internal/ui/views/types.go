package views

import "github.com/deastl/artifact-host/internal/model"

// GalleryData holds all data needed to render the root artifact gallery page.
type GalleryData struct {
	Artifacts  []model.Artifact
	AllTags    []string
	Query      string
	ActiveTag  string
	Status     model.DaemonStatus
	TotalCount int
}

// ViewerData holds all data needed to render the full-viewport artifact viewer.
type ViewerData struct {
	Artifact    model.Artifact
	Current     model.Version
	Versions    []model.Version
	Submissions []model.SubmissionMeta
	Status      model.DaemonStatus
}

// ErrorData holds all data needed to render error and 404 pages.
type ErrorData struct {
	StatusCode int
	Title      string
	Message    string
	Detail     string
	BackURL    string
	BackLabel  string
}
