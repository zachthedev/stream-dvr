package generate

import (
	"encoding/json"
	"strings"
	"testing"
)

type sampleJSON struct {
	Name    string `json:"name"`
	Count   int    `json:"count"`
	Enabled bool   `json:"enabled"`
}

// ///////////////////////////////////////////////
// JSONConfig.Generate
// ///////////////////////////////////////////////

func TestJSONConfig_Generate_DefaultIndent(t *testing.T) {
	cfg := JSONConfig{
		Defaults: sampleJSON{Name: "x", Count: 5, Enabled: true},
	}
	data, err := cfg.Generate(OutputEntry{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, "  \"name\": \"x\"") {
		t.Errorf("output missing 2-space indent:\n%s", got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Error("output missing trailing newline")
	}
	var round sampleJSON
	if err := json.Unmarshal(data, &round); err != nil {
		t.Fatalf("round-trip unmarshal: %v", err)
	}
	if round.Count != 5 {
		t.Errorf("round-trip Count = %d, want 5", round.Count)
	}
}

func TestJSONConfig_Generate_CustomIndent(t *testing.T) {
	cfg := JSONConfig{
		Defaults: sampleJSON{Name: "x"},
		Indent:   "\t",
	}
	data, err := cfg.Generate(OutputEntry{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.Contains(string(data), "\t\"name\"") {
		t.Errorf("output missing tab indent:\n%s", data)
	}
}

func TestJSONConfig_Generate_EncodingError(t *testing.T) {
	cfg := JSONConfig{
		Defaults: make(chan int), // channels are not JSON-encodable
	}
	if _, err := cfg.Generate(OutputEntry{}); err == nil {
		t.Error("Generate error = nil, want error for unmarshalable value")
	}
}
