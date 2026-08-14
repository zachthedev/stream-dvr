package generate

import (
	"encoding/json"
	"strings"
	"testing"
)

type sampleManifest struct {
	Name       string   `json:"name"                 jsonschema:"required"`
	Tags       []string `json:"tags,omitempty"`
	MaxWorkers int      `json:"max_workers,omitempty"`
}

// ///////////////////////////////////////////////
// JSONSchema.Generate
// ///////////////////////////////////////////////

func TestJSONSchema_Generate_PopulatesMetadata(t *testing.T) {
	s := JSONSchema{
		Target:      &sampleManifest{},
		ID:          "https://example.com/sample.json",
		Title:       "Sample Manifest",
		Description: "A test manifest for validation.",
	}
	data, err := s.Generate(OutputEntry{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("parsing schema: %v", err)
	}
	if got, want := parsed["$id"], "https://example.com/sample.json"; got != want {
		t.Errorf("$id = %v, want %v", got, want)
	}
	if got, want := parsed["title"], "Sample Manifest"; got != want {
		t.Errorf("title = %v, want %v", got, want)
	}
	if got, want := parsed["description"], "A test manifest for validation."; got != want {
		t.Errorf("description = %v, want %v", got, want)
	}
}

func TestJSONSchema_Generate_ReflectsFields(t *testing.T) {
	s := JSONSchema{Target: &sampleManifest{}}
	data, err := s.Generate(OutputEntry{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	got := string(data)
	for _, want := range []string{`"name"`, `"tags"`, `"max_workers"`} {
		if !strings.Contains(got, want) {
			t.Errorf("schema missing field %s:\n%s", want, got)
		}
	}
}

func TestJSONSchema_Generate_TrailingNewline(t *testing.T) {
	s := JSONSchema{Target: &sampleManifest{}}
	data, err := s.Generate(OutputEntry{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Error("output missing trailing newline")
	}
}

func TestJSONSchema_Generate_CustomIndent(t *testing.T) {
	s := JSONSchema{Target: &sampleManifest{}, Indent: "\t"}
	data, err := s.Generate(OutputEntry{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.Contains(string(data), "\t\"") {
		t.Errorf("output missing tab indent:\n%s", data)
	}
}

func TestJSONSchema_Generate_NilTargetErrors(t *testing.T) {
	s := JSONSchema{}
	if _, err := s.Generate(OutputEntry{}); err == nil {
		t.Error("Generate error = nil, want error for nil Target")
	}
}

// A template leaves off the $comment field, so the on-disk schema reads as
// the deployer's own file and not as build output.
func TestJSONSchema_Generate_TemplateOmitsComment(t *testing.T) {
	s := JSONSchema{Target: &sampleManifest{}}

	artifact, err := s.Generate(OutputEntry{})
	if err != nil {
		t.Fatalf("Generate(artifact): %v", err)
	}
	tpl, err := s.Generate(OutputEntry{Template: true})
	if err != nil {
		t.Fatalf("Generate(template): %v", err)
	}

	var artifactParsed, tplParsed map[string]any
	if err := json.Unmarshal(artifact, &artifactParsed); err != nil {
		t.Fatalf("parse artifact: %v", err)
	}
	if err := json.Unmarshal(tpl, &tplParsed); err != nil {
		t.Fatalf("parse template: %v", err)
	}
	if got := artifactParsed["$comment"]; got != GeneratedByHeader {
		t.Errorf("artifact $comment = %v, want %q", got, GeneratedByHeader)
	}
	if _, present := tplParsed["$comment"]; present {
		t.Errorf("template $comment should be omitted, got: %v", tplParsed["$comment"])
	}
}
