package api

import "github.com/smoreg/protobuffet/internal/registry"

// PublishManifest is the JSON part of a publish request. Artifact bodies
// travel as separate multipart files named by Artifact.File.
type PublishManifest struct {
	Module    string              `json:"module"`
	Version   string              `json:"version"`
	Labels    []string            `json:"labels,omitempty"`
	Artifacts []registry.Artifact `json:"artifacts"`
	// SchemaSHA256 lets the server verify the upload arrived intact.
	SchemaSHA256 string `json:"schema_sha256"`
}

// PublishResponse is returned after a successful publish.
type PublishResponse struct {
	Module   string           `json:"module"`
	Version  registry.Version `json:"version"`
	Breaking *BreakingReport  `json:"breaking,omitempty"`
}

// BreakingReport describes the compatibility check the server performed.
type BreakingReport struct {
	Checked  bool   `json:"checked"`
	Against  string `json:"against,omitempty"`
	Skipped  string `json:"skipped,omitempty"`
	Findings string `json:"findings,omitempty"`
}
