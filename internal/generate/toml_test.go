package generate

import (
	"strings"
	"testing"
)

// sampleConfig is a minimal struct with nested sections, scalar fields, and
// the omitempty field that drives injectOmitted.
type sampleConfig struct {
	Name    string         `toml:"name"`
	Timeout int            `toml:"timeout"`
	Server  sampleServer   `toml:"server"`
	Clients map[string]any `toml:"clients,omitempty"`
}

type sampleServer struct {
	Host string `toml:"host"`
	Port int    `toml:"port"`
}

// nestedConfig holds two levels of tables: a section with its own scalars,
// followed by subtables that carry their own docs.
type nestedConfig struct {
	Space nestedSpace `toml:"space"`
}

type nestedSpace struct {
	MaxSize    string           `toml:"max_size"`
	Recompress nestedRecompress `toml:"recompress"`
	Purge      nestedPurge      `toml:"purge"`
}

type nestedRecompress struct {
	Enabled bool `toml:"enabled"`
}

type nestedPurge struct {
	ProtectFor string `toml:"protect_for"`
}

// arrayConfig holds an array of tables, which is a section that repeats,
// placed after a section of the ordinary kind.
type arrayConfig struct {
	Name     string         `toml:"name"`
	Server   sampleServer   `toml:"server"`
	Channels []arrayChannel `toml:"channels,omitempty"`
}

type arrayChannel struct {
	Platform string `toml:"platform"`
	Enabled  bool   `toml:"enabled"`
}

func sampleDocs() map[string]FieldDoc {
	return map[string]FieldDoc{
		"name": {
			Comment: "Human-readable project name.",
		},
		"timeout": {
			Comment:      "Request timeout in seconds.",
			Alternatives: []string{`timeout = 60`},
		},
		"server": {
			Comment: "Server bind settings.",
		},
		"server.host": {
			Comment: "Interface to bind.",
		},
		"server.port": {
			Comment: "Port to listen on.",
		},
		"clients": {
			Comment: "Known client registrations (empty by default).",
		},
	}
}

func arrayDocs() map[string]FieldDoc {
	return map[string]FieldDoc{
		"name":              {Comment: "Human-readable project name."},
		"server":            {Comment: "Server bind settings."},
		"server.host":       {Comment: "Interface to bind."},
		"server.port":       {Comment: "Port to listen on."},
		"channels":          {Comment: "Channels to watch."},
		"channels.platform": {Comment: "Platform the channel is on."},
		"channels.enabled":  {Comment: "Watch this channel."},
	}
}

func nestedDocs() map[string]FieldDoc {
	return map[string]FieldDoc{
		"space": {
			Comment: "What happens as the library fills.",
		},
		"space.max_size": {
			Comment: "Cap on total recording bytes.",
		},
		"space.recompress": {
			Comment: "Re-encode older recordings to a denser codec.",
		},
		"space.recompress.enabled": {
			Comment: "Turn re-encoding on.",
		},
		"space.purge": {
			Comment: "Scoring for the assisted purge.",
		},
		"space.purge.protect_for": {
			Comment: "Recordings younger than this never appear in the purge list.",
		},
	}
}

// ///////////////////////////////////////////////
// TOMLConfig.Generate
// ///////////////////////////////////////////////

func TestTOMLConfig_Generate_ArtifactBanner(t *testing.T) {
	cfg := TOMLConfig{
		ProjectName: "example",
		Defaults:    &sampleConfig{Name: "x", Server: sampleServer{Host: "0.0.0.0", Port: 80}},
	}
	out, err := cfg.Generate(OutputEntry{}) // Template defaults to false
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	got := string(out)
	for _, want := range []string{
		"# " + GeneratedByHeader,
		"# ///////////////////////////////////////////////",
		"# example Configuration",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "edit this copy freely") {
		t.Errorf("artifact banner leaked template wording:\n%s", got)
	}
}

// A template is committed and also copied to an operator's disk, so its
// header speaks to both audiences. A file somebody is told to edit must not
// open on an instruction not to.
func TestTOMLConfig_Generate_TemplateBanner(t *testing.T) {
	cfg := TOMLConfig{
		ProjectName: "example",
		Defaults:    &sampleConfig{Name: "x", Server: sampleServer{Host: "0.0.0.0", Port: 80}},
	}
	out, err := cfg.Generate(OutputEntry{Template: true})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	got := string(out)
	for _, want := range []string{
		"# example config. Defaults generated; edit this copy freely.",
		"# Contributors: update internal/config/*.go and run `make generate`.",
		"# example Configuration",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, GeneratedByHeader) {
		t.Errorf("template banner leaked artifact wording %q:\n%s", GeneratedByHeader, got)
	}
}

func TestTOMLConfig_Generate_InjectsFieldComments(t *testing.T) {
	cfg := TOMLConfig{
		ProjectName: "example",
		Defaults:    &sampleConfig{Name: "x", Timeout: 30, Server: sampleServer{Host: "0.0.0.0", Port: 80}},
		Docs:        sampleDocs(),
	}
	out, err := cfg.Generate(OutputEntry{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	got := string(out)
	for _, want := range []string{
		"# Human-readable project name.",
		`name = "x"`,
		"# Request timeout in seconds.",
		"timeout = 30",
		"# timeout = 60",
		"# Interface to bind.",
		`host = "0.0.0.0"`,
		"# Port to listen on.",
		"port = 80",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

func TestTOMLConfig_Generate_SectionHeader(t *testing.T) {
	cfg := TOMLConfig{
		ProjectName: "example",
		Defaults:    &sampleConfig{Server: sampleServer{Host: "h", Port: 1}},
		Docs:        sampleDocs(),
	}
	out, err := cfg.Generate(OutputEntry{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "# ///// Server /////") {
		t.Errorf("missing section label %q:\n%s", "# ///// Server /////", got)
	}
	if !strings.Contains(got, "[server]") {
		t.Errorf("missing [server] header:\n%s", got)
	}
}

func TestTOMLConfig_Generate_InjectsOmittedTopLevelSection(t *testing.T) {
	cfg := TOMLConfig{
		ProjectName: "example",
		Defaults:    &sampleConfig{}, // Clients is an empty map, which the encoder omits
		Docs:        sampleDocs(),
	}
	out, err := cfg.Generate(OutputEntry{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "# Known client registrations (empty by default).") {
		t.Errorf("missing omitted-top-level comment:\n%s", got)
	}
	if !strings.Contains(got, "# ///// Clients /////") {
		t.Errorf("missing omitted-top-level header:\n%s", got)
	}
}

// A subtable's own doc entry sits under its parent's prefix and has no
// further dot, which is the same shape as a key the encoder omitted. Read
// that way it is emitted twice: once as an orphan comment block attached to
// no key at the end of the parent, and again above its real header.
func TestTOMLConfig_Generate_DocumentsASubtableOnce(t *testing.T) {
	cfg := TOMLConfig{
		ProjectName: "example",
		Defaults: &nestedConfig{Space: nestedSpace{
			MaxSize:    "2TB",
			Recompress: nestedRecompress{Enabled: false},
			Purge:      nestedPurge{ProtectFor: "1w"},
		}},
		Docs: nestedDocs(),
	}
	out, err := cfg.Generate(OutputEntry{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	got := string(out)

	tests := []struct {
		name string
		want string
	}{
		{name: "the parent section comment", want: "# What happens as the library fills."},
		{name: "a subtable comment", want: "# Re-encode older recordings to a denser codec."},
		{name: "the other subtable comment", want: "# Scoring for the assisted purge."},
		{name: "a subtable header", want: "[space.recompress]"},
		{name: "the other subtable header", want: "[space.purge]"},
		{name: "a key inside a subtable", want: "enabled = false"},
		{name: "a key inside the other subtable", want: `protect_for = "1w"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if n := strings.Count(got, tt.want); n != 1 {
				t.Errorf("%q appears %d times, want exactly 1:\n%s", tt.want, n, got)
			}
		})
	}
}

// An array-of-tables header opens a section like any other. Read as an
// ordinary line it leaves the section stack on the table before it, so every
// key under it resolves against that table, its own docs never appear, and
// the section is emitted again at the end as an orphan.
func TestTOMLConfig_Generate_DocumentsAnArrayOfTables(t *testing.T) {
	cfg := TOMLConfig{
		ProjectName: "example",
		Defaults: &arrayConfig{
			Name:   "x",
			Server: sampleServer{Host: "h", Port: 1},
			Channels: []arrayChannel{
				{Platform: "twitch", Enabled: true},
				{Platform: "youtube", Enabled: false},
			},
		},
		Docs: arrayDocs(),
	}
	out, err := cfg.Generate(OutputEntry{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	got := string(out)

	counts := []struct {
		name string
		want string
		n    int
	}{
		{name: "the section banner", want: "# ///// Channels /////", n: 1},
		{name: "the section comment", want: "# Channels to watch.", n: 1},
		{name: "a field comment", want: "# Platform the channel is on.", n: 1},
		{name: "the other field comment", want: "# Watch this channel.", n: 1},
		{name: "the header itself", want: "[[channels]]", n: 2},
		{name: "the first entry", want: `platform = "twitch"`, n: 1},
		{name: "the second entry", want: `platform = "youtube"`, n: 1},
		{name: "the section before it", want: "# ///// Server /////", n: 1},
	}
	for _, tt := range counts {
		t.Run(tt.name, func(t *testing.T) {
			if n := strings.Count(got, tt.want); n != tt.n {
				t.Errorf("%q appears %d times, want %d:\n%s", tt.want, n, tt.n, got)
			}
		})
	}

	t.Run("the section is documented where it starts", func(t *testing.T) {
		banner := strings.Index(got, "# ///// Channels /////")
		header := strings.Index(got, "[[channels]]")
		if banner < 0 || header < 0 || banner > header {
			t.Errorf("banner at %d and header at %d, want the banner first:\n%s", banner, header, got)
		}
	})

	t.Run("a key resolves against its own section", func(t *testing.T) {
		// The comment for channels.platform sitting under [server] means the
		// key was attributed to the table before it.
		server := strings.Index(got, "[server]")
		header := strings.Index(got, "[[channels]]")
		comment := strings.Index(got, "# Platform the channel is on.")
		if comment < 0 || comment < header {
			t.Errorf("the platform comment is at %d, want it after the header at %d (server is at %d):\n%s",
				comment, header, server, got)
		}
	})
}

func TestTOMLConfig_Generate_Deterministic(t *testing.T) {
	cfg := TOMLConfig{
		ProjectName: "example",
		Defaults:    &sampleConfig{Name: "x", Timeout: 1, Server: sampleServer{Host: "h", Port: 2}},
		Docs:        sampleDocs(),
	}
	first, err := cfg.Generate(OutputEntry{})
	if err != nil {
		t.Fatalf("first Generate: %v", err)
	}
	for i := range 5 {
		next, err := cfg.Generate(OutputEntry{})
		if err != nil {
			t.Fatalf("Generate iteration %d: %v", i, err)
		}
		if string(next) != string(first) {
			t.Errorf("Generate iteration %d differs from first", i)
		}
	}
}

func TestTOMLConfig_Generate_EncodingError(t *testing.T) {
	cfg := TOMLConfig{
		ProjectName: "example",
		Defaults:    make(chan int), // not marshalable
	}
	if _, err := cfg.Generate(OutputEntry{}); err == nil {
		t.Error("Generate error = nil, want error for unmarshalable value")
	}
}
