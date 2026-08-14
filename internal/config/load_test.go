package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// ///////////////////////////////////////////////
// Helpers
// ///////////////////////////////////////////////

// fixtureRoot returns a library root that is absolute on the running
// platform. A rooted path carrying no volume, such as "/srv/vods", is
// drive-relative on Windows and absolute everywhere else, so one literal
// cannot stand in for both.
func fixtureRoot() string {
	if runtime.GOOS == "windows" {
		return `D:\recordings`
	}
	return "/srv/vods"
}

// fixtureRootTOML returns fixtureRoot as a TOML literal string, in which a
// backslash is a plain character rather than the start of an escape.
func fixtureRootTOML() string {
	return "'" + fixtureRoot() + "'"
}

// validConfig returns the smallest config that passes validation.
func validConfig() Config {
	cfg := DefaultConfig()
	cfg.Library.Root = fixtureRoot()
	return cfg
}

// projectFile reads a file at the repository root.
func projectFile(t *testing.T, name string) string {
	t.Helper()

	body, err := os.ReadFile(filepath.Join("..", "..", name))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(body)
}

// problemFields returns the fields a validation error names.
func problemFields(t *testing.T, err error) []string {
	t.Helper()

	var invalid *ValidationError
	if !errors.As(err, &invalid) {
		t.Fatalf("err = %v, want a *ValidationError", err)
	}
	fields := make([]string, 0, len(invalid.Problems))
	for _, p := range invalid.Problems {
		fields = append(fields, p.Field)
	}
	return fields
}

// hasField reports whether fields contains want.
func hasField(fields []string, want string) bool {
	return slices.Contains(fields, want)
}

// ///////////////////////////////////////////////
// Defaults
// ///////////////////////////////////////////////

func TestDefaultConfig_NeedsOnlyALibraryRoot(t *testing.T) {
	// Every default must be usable as shipped, so the operator's first
	// edit is the one value the tool cannot guess.
	cfg := DefaultConfig()

	err := cfg.Validate()
	fields := problemFields(t, err)
	if len(fields) != 1 || fields[0] != "library.root" {
		t.Errorf("DefaultConfig().Validate() problems = %v, want only library.root", fields)
	}

	cfg.Library.Root = fixtureRoot()
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() with a root err = %v, want nil", err)
	}
}

func TestDefaultConfig_RecoversAutomatically(t *testing.T) {
	// A recorder that does not fill its own gaps is the behaviour that has
	// to be asked for, not the one that ships. Defaulting this off would
	// leave every install silently waiting on a command nobody knew to run.
	if !DefaultConfig().Backfill.Automatic {
		t.Error("backfill.automatic defaults to false, want a recorder that recovers on its own")
	}
}

func TestDefaultConfig_TemplateParses(t *testing.T) {
	if _, err := DefaultConfig().Template(); err != nil {
		t.Errorf("Template() err = %v, want nil", err)
	}
}

// ///////////////////////////////////////////////
// Validation
// ///////////////////////////////////////////////

func TestValidate_Rejects(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*Config)
		wantField string
	}{
		{
			name:      "missing library root",
			mutate:    func(c *Config) { c.Library.Root = "" },
			wantField: "library.root",
		},
		{
			// A url channel has no listing of past broadcasts to search, so
			// backfill on one is a setting that could never do anything.
			// Refusing at load is what stops an operator waiting on a
			// recovery that was never going to run.
			name: "backfill on a url channel",
			mutate: func(c *Config) {
				c.Channels = []Channel{{
					Platform: PlatformURL,
					Name:     "https://example.com/live/stream.m3u8",
					Backfill: true,
				}}
			},
			wantField: "channels[0].backfill",
		},
		{
			name:      "config written by a newer build",
			mutate:    func(c *Config) { c.Version = SchemaVersion + 1 },
			wantField: "schema_version",
		},
		{
			name:      "version below one",
			mutate:    func(c *Config) { c.Version = 0 },
			wantField: "schema_version",
		},
		{
			name:      "whitespace library root",
			mutate:    func(c *Config) { c.Library.Root = "   " },
			wantField: "library.root",
		},
		{
			name:      "negative max size",
			mutate:    func(c *Config) { c.Space.MaxSize = -1 },
			wantField: "space.max_size",
		},
		{
			name:      "negative min free",
			mutate:    func(c *Config) { c.Space.MinFree = -1 },
			wantField: "space.min_free",
		},
		{
			name:      "poll interval too short",
			mutate:    func(c *Config) { c.Capture.PollInterval = 1 },
			wantField: "capture.poll_interval",
		},
		{
			name:      "poll interval too long",
			mutate:    func(c *Config) { c.Capture.PollInterval = maxPollInterval + 1 },
			wantField: "capture.poll_interval",
		},
		{
			name:      "no quality ladder",
			mutate:    func(c *Config) { c.Capture.Quality = nil },
			wantField: "capture.quality",
		},
		{
			name:      "zero concurrency",
			mutate:    func(c *Config) { c.Capture.MaxConcurrent = 0 },
			wantField: "capture.max_concurrent",
		},
		{
			name:      "excessive concurrency",
			mutate:    func(c *Config) { c.Capture.MaxConcurrent = maxConcurrentLimit + 1 },
			wantField: "capture.max_concurrent",
		},
		{
			name:      "unsupported container",
			mutate:    func(c *Config) { c.Capture.Container = "avi" },
			wantField: "capture.container",
		},
		{
			name:      "unparseable naming template",
			mutate:    func(c *Config) { c.Naming.Template = "{streamer}" },
			wantField: "naming.template",
		},
		{
			name:      "template with no placeholders",
			mutate:    func(c *Config) { c.Naming.Template = "recording.mkv" },
			wantField: "naming.template",
		},
		{
			name:      "unknown timezone",
			mutate:    func(c *Config) { c.Naming.Timezone = "Mars/Olympus" },
			wantField: "naming.timezone",
		},
		{
			name:      "negative purge weight",
			mutate:    func(c *Config) { c.Space.Purge.WatchedWeight = -1 },
			wantField: "space.purge.watched_weight",
		},
		{
			name:      "negative protect window",
			mutate:    func(c *Config) { c.Space.Purge.ProtectFor = -1 },
			wantField: "space.purge.protect_for",
		},
		{
			name:      "webhook without a scheme",
			mutate:    func(c *Config) { c.Notify.WebhookURL = "ntfy.sh/topic" },
			wantField: "notify.webhook_url",
		},
		{
			// A prefix test accepts this, and it is not an address.
			name:      "webhook with a space in the host",
			mutate:    func(c *Config) { c.Notify.WebhookURL = "https:// space/topic" },
			wantField: "notify.webhook_url",
		},
		{
			name:      "webhook carrying a newline",
			mutate:    func(c *Config) { c.Notify.WebhookURL = "https://ntfy.sh/to\npic" },
			wantField: "notify.webhook_url",
		},
		{
			name:      "webhook with no host",
			mutate:    func(c *Config) { c.Notify.WebhookURL = "https:///topic" },
			wantField: "notify.webhook_url",
		},
		{
			// A password in the URL reaches every log line and process
			// list that renders it.
			name:      "webhook carrying credentials",
			mutate:    func(c *Config) { c.Notify.WebhookURL = "https://user:pass@ntfy.sh/topic" },
			wantField: "notify.webhook_url",
		},
		{
			// The cloud metadata service answers here and hands out
			// credentials to anything that asks.
			name:      "webhook pointed at the link-local range",
			mutate:    func(c *Config) { c.Notify.WebhookURL = "http://169.254.169.254/latest/meta-data/" },
			wantField: "notify.webhook_url",
		},
		{
			name:      "webhook pointed at a link-local v6 address",
			mutate:    func(c *Config) { c.Notify.WebhookURL = "http://[fe80::1]/topic" },
			wantField: "notify.webhook_url",
		},
		{
			name: "unknown platform",
			mutate: func(c *Config) {
				c.Channels = []Channel{{Platform: "kick", Name: "someone", Enabled: true}}
			},
			wantField: "channels[0].platform",
		},
		{
			name: "channel with no name",
			mutate: func(c *Config) {
				c.Channels = []Channel{{Platform: PlatformTwitch, Name: "", Enabled: true}}
			},
			wantField: "channels[0].name",
		},
		{
			name: "url platform without a scheme",
			mutate: func(c *Config) {
				c.Channels = []Channel{{Platform: PlatformURL, Name: "example.com/live", Enabled: true}}
			},
			wantField: "channels[0].name",
		},
		{
			name: "duplicate channel",
			mutate: func(c *Config) {
				c.Channels = []Channel{
					{Platform: PlatformTwitch, Name: "examplechannel", Enabled: true},
					{Platform: PlatformTwitch, Name: "ExampleChannel", Enabled: true},
				}
			},
			wantField: "channels[1]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.mutate(&cfg)

			fields := problemFields(t, cfg.Validate())
			if !hasField(fields, tt.wantField) {
				t.Errorf("Validate() problems = %v, want one naming %q", fields, tt.wantField)
			}
		})
	}
}

func TestContainers_EachHasAnIgnoreRule(t *testing.T) {
	// The quick start runs the binary inside a checkout, so a real recording
	// can land beside the source. A container with no rule offers hours of
	// broadcast to git status, and the operator's first sight of it is a
	// staged file measured in gigabytes.
	//
	// Compared line by line rather than by substring, because a checkout on
	// Windows carries CRLF and a rule searched for wrapped in "\n" reads as
	// missing on that platform only.
	rules := map[string]bool{}
	for line := range strings.SplitSeq(projectFile(t, ".gitignore"), "\n") {
		rules[strings.TrimSpace(line)] = true
	}

	for _, container := range Containers {
		if !rules["*."+container] {
			t.Errorf(".gitignore has no rule for *.%s, so a recording in that container is offered to git", container)
		}
	}
}

func TestValidate_Accepts(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "no channels", mutate: func(c *Config) { c.Channels = nil }},
		{name: "zero max size disables the cap", mutate: func(c *Config) { c.Space.MaxSize = 0 }},
		{name: "empty webhook", mutate: func(c *Config) { c.Notify.WebhookURL = "" }},
		{name: "https webhook", mutate: func(c *Config) { c.Notify.WebhookURL = "https://ntfy.sh/t" }},
		// A scheme is case-insensitive, and this is the same address.
		{name: "an upper case webhook scheme", mutate: func(c *Config) { c.Notify.WebhookURL = "HTTPS://ntfy.sh/t" }},
		{
			// The operator's own network is the ordinary case for a url
			// channel, so a private address is not a fault.
			name: "a channel on the local network",
			mutate: func(c *Config) {
				c.Channels = []Channel{{Platform: PlatformURL, Name: "http://192.168.1.5:8080/live", Enabled: true}}
			},
		},
		{
			name: "a youtube handle with a period and a hyphen",
			mutate: func(c *Config) {
				c.Channels = []Channel{{Platform: PlatformYouTube, Name: "example.channel-two", Enabled: true}}
			},
		},
		{name: "explicit timezone", mutate: func(c *Config) { c.Naming.Timezone = "UTC" }},
		{name: "empty timezone means local", mutate: func(c *Config) { c.Naming.Timezone = "" }},
		{
			name: "same name on different platforms",
			mutate: func(c *Config) {
				c.Channels = []Channel{
					{Platform: PlatformTwitch, Name: "examplechannel", Enabled: true},
					{Platform: PlatformYouTube, Name: "examplechannel", Enabled: true},
				}
			},
		},
		{
			name: "url channel with a scheme",
			mutate: func(c *Config) {
				c.Channels = []Channel{{Platform: PlatformURL, Name: "https://example.com/live", Enabled: true}}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.mutate(&cfg)

			if err := cfg.Validate(); err != nil {
				t.Errorf("Validate() err = %v, want nil", err)
			}
		})
	}
}

func TestValidate_RejectsAChannelNameThatIsAnArgumentRatherThanAName(t *testing.T) {
	// The name becomes part of a URL and then an argument to streamlink. A
	// value starting with "-" is read there as an option, and --config
	// points streamlink at a file that can set --player to any executable
	// on the disk. A test for "://" is satisfied by a scheme-like tail.
	tests := []struct {
		name     string
		platform string
		channel  string
	}{
		{name: "a streamlink option", platform: PlatformTwitch, channel: "--loglevel"},
		{name: "an option with a scheme-like tail", platform: PlatformURL, channel: "--config=./pwned.conf://x"},
		{name: "an option carrying a URL", platform: PlatformURL, channel: "--player-external-http://y"},
		{name: "a file URL", platform: PlatformURL, channel: "file:///c:/windows/win.ini"},
		{name: "a bare path", platform: PlatformURL, channel: "/etc/passwd"},
		{name: "a URL with no host", platform: PlatformURL, channel: "https://"},
		{name: "a path separator in a twitch name", platform: PlatformTwitch, channel: "example/../other"},
		{name: "a query in a twitch name", platform: PlatformTwitch, channel: "example?x=1"},
		{name: "a twitch name past the login limit", platform: PlatformTwitch, channel: strings.Repeat("a", 26)},
		{name: "a slash in a youtube handle", platform: PlatformYouTube, channel: "example/live"},
		{name: "a newline in a name", platform: PlatformTwitch, channel: "example\nchannel"},
		{name: "a tab in a name", platform: PlatformTwitch, channel: "example\tchannel"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Channels = []Channel{{Platform: tt.platform, Name: tt.channel, Enabled: true}}

			fields := problemFields(t, cfg.Validate())
			if !hasField(fields, "channels[0].name") {
				t.Errorf("Validate() reported %v, want a problem on channels[0].name", fields)
			}
		})
	}
}

func TestChannelNamePatterns_BoundEveryPlatformButURL(t *testing.T) {
	// The pattern is what keeps a name from becoming a streamlink option. A
	// platform with no entry accepts any printable name that does not open
	// with a dash, and validation reports nothing at all.
	//
	// PlatformURL is the documented exception: channelNameProblem bounds it
	// with webAddressProblem, which is a parse rather than a pattern.
	for _, platform := range SupportedPlatforms {
		if platform == PlatformURL {
			continue
		}
		if _, bounded := channelNamePatterns[platform]; !bounded {
			t.Errorf("%q has no channel name pattern, so any printable name is accepted for it", platform)
		}
	}

	for platform := range channelNamePatterns {
		if !slices.Contains(SupportedPlatforms, platform) {
			t.Errorf("channelNamePatterns bounds %q, which is not a supported platform", platform)
		}
	}
}

func TestValidate_RequiresAnAbsoluteLibraryRoot(t *testing.T) {
	// The root is the prefix of every path the daemon builds, and those
	// paths reach ffmpeg and streamlink argument lists. A root written as
	// "-vods" is relative, so requiring an absolute path is also what stops
	// an ffmpeg operand from being read as an option.
	t.Run("rejects", func(t *testing.T) {
		tests := []struct {
			name string
			root string
		}{
			{name: "a bare name", root: "vods"},
			{name: "an explicitly relative path", root: "." + string(filepath.Separator) + "vods"},
			{name: "a parent reference", root: filepath.Join("..", "vods")},
			{name: "a name that reads as an option", root: "-vods"},
			{name: "a name that reads as a long option", root: "--output"},
			{name: "a home shorthand nothing expands", root: "~/vods"},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				cfg := validConfig()
				cfg.Library.Root = tt.root

				fields := problemFields(t, cfg.Validate())
				if !hasField(fields, "library.root") {
					t.Errorf("Validate() problems = %v, want one naming library.root", fields)
				}
			})
		}
	})

	t.Run("accepts", func(t *testing.T) {
		roots := []string{fixtureRoot()}
		if runtime.GOOS == "windows" {
			roots = append(roots, `C:/recordings`, `\\server\share\recordings`)
		}

		for _, root := range roots {
			t.Run(root, func(t *testing.T) {
				cfg := validConfig()
				cfg.Library.Root = root

				if err := cfg.Validate(); err != nil {
					t.Errorf("Validate() err = %v, want nil", err)
				}
			})
		}
	})
}

func TestValidate_BoundsEveryQualityInALadder(t *testing.T) {
	// The ladder reaches streamlink as one argument with its entries joined
	// by commas, so an entry holding a comma is silently two entries and one
	// starting with "-" reads as an option.
	tests := []struct {
		name    string
		quality string
		valid   bool
	}{
		{name: "a resolution and frame rate", quality: "1080p60", valid: true},
		{name: "a resolution", quality: "1080p", valid: true},
		{name: "a lower resolution", quality: "720p60", valid: true},
		{name: "best", quality: "best", valid: true},
		{name: "worst", quality: "worst", valid: true},
		{name: "audio only", quality: "audio_only", valid: true},
		{name: "empty", quality: ""},
		{name: "a streamlink option", quality: "--config=./pwned.conf"},
		{name: "a leading dash", quality: "-best"},
		{name: "an embedded comma", quality: "best,--config=x"},
		{name: "a trailing comma", quality: "best,"},
		{name: "a space", quality: "1080p 60"},
		{name: "a newline", quality: "best\nworst"},
		{name: "a path", quality: "../../etc/passwd"},
		{name: "a shell metacharacter", quality: "best;calc"},
	}

	ladders := []struct {
		name  string
		field string
		set   func(*Config, []string)
	}{
		{
			name:  "capture",
			field: "capture.quality[0]",
			set:   func(c *Config, ladder []string) { c.Capture.Quality = ladder },
		},
		{
			name:  "channel",
			field: "channels[0].quality[0]",
			set: func(c *Config, ladder []string) {
				c.Channels = []Channel{{
					Platform: PlatformTwitch,
					Name:     "examplechannel",
					Enabled:  true,
					Quality:  ladder,
				}}
			},
		},
	}

	for _, ladder := range ladders {
		t.Run(ladder.name, func(t *testing.T) {
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					cfg := validConfig()
					ladder.set(&cfg, []string{tt.quality})

					err := cfg.Validate()
					if tt.valid {
						if err != nil {
							t.Errorf("Validate() err = %v, want nil", err)
						}
						return
					}
					fields := problemFields(t, err)
					if !hasField(fields, ladder.field) {
						t.Errorf("Validate() problems = %v, want one naming %q", fields, ladder.field)
					}
				})
			}
		})
	}
}

func TestValidate_RefusesAControlCharacterInEveryStringField(t *testing.T) {
	// A TOML \u0000 escape puts a real NUL in a field, and a check that reads
	// what a value says rather than what it holds passes it. Nothing
	// downstream carries the byte intact: url.PathEscape writes it as "%00"
	// and a C API takes it as the end of the string, so the path that is used
	// is not the path that was written.
	fields := []struct {
		field  string
		mutate func(*Config, string)
	}{
		{
			field:  "library.root",
			mutate: func(c *Config, bad string) { c.Library.Root = fixtureRoot() + bad },
		},
		{
			field:  "capture.container",
			mutate: func(c *Config, bad string) { c.Capture.Container = "mkv" + bad },
		},
		{
			field:  "capture.quality[0]",
			mutate: func(c *Config, bad string) { c.Capture.Quality = []string{"best" + bad} },
		},
		{
			field:  "naming.template",
			mutate: func(c *Config, bad string) { c.Naming.Template += bad },
		},
		{
			field:  "naming.timezone",
			mutate: func(c *Config, bad string) { c.Naming.Timezone = "UTC" + bad },
		},
		{
			field:  "space.recompress.codec",
			mutate: func(c *Config, bad string) { c.Space.Recompress.Codec = CodecHEVC + bad },
		},
		{
			field:  "notify.webhook_url",
			mutate: func(c *Config, bad string) { c.Notify.WebhookURL = "https://ntfy.sh/topic" + bad },
		},
		{
			field: "channels[0].platform",
			mutate: func(c *Config, bad string) {
				c.Channels = []Channel{{Platform: PlatformTwitch + bad, Name: "examplechannel", Enabled: true}}
			},
		},
		{
			field: "channels[0].name",
			mutate: func(c *Config, bad string) {
				c.Channels = []Channel{{Platform: PlatformTwitch, Name: "examplechannel" + bad, Enabled: true}}
			},
		},
		{
			field: "channels[0].quality[0]",
			mutate: func(c *Config, bad string) {
				c.Channels = []Channel{{
					Platform: PlatformTwitch,
					Name:     "examplechannel",
					Enabled:  true,
					Quality:  []string{"best" + bad},
				}}
			},
		},
	}

	// Whitespace is trimmed rather than refused, so a control character that
	// is also whitespace belongs to the trimming tests instead of these.
	characters := []struct {
		name string
		text string
	}{
		{name: "a NUL", text: "\x00"},
		{name: "an escape", text: "\x1b"},
		{name: "a bell", text: "\a"},
		{name: "a zero-width space", text: "\u200b"},
		{name: "a right-to-left override", text: "\u202e"},
	}

	for _, f := range fields {
		t.Run(f.field, func(t *testing.T) {
			for _, char := range characters {
				t.Run(char.name, func(t *testing.T) {
					cfg := validConfig()
					f.mutate(&cfg, char.text)

					got := problemFields(t, cfg.Validate())
					if !hasField(got, f.field) {
						t.Errorf("Validate() problems = %v, want one naming %q", got, f.field)
					}
				})
			}
		})
	}
}

func TestValidate_RejectsChannelsThatDifferOnlyByWhitespace(t *testing.T) {
	// The daemon watches by name and the database keeps one row per name.
	// Two entries differing only by whitespace are one broadcast captured
	// twice into two files, which the UNIQUE constraint collapses to one row.
	cfg := validConfig()
	cfg.Channels = []Channel{
		{Platform: PlatformTwitch, Name: "examplechannel", Enabled: true},
		{Platform: PlatformTwitch, Name: "  examplechannel\t", Enabled: true},
	}

	fields := problemFields(t, cfg.Validate())
	if !hasField(fields, "channels[1]") {
		t.Errorf("Validate() reported %v, want channels[1] called out as a duplicate", fields)
	}
}

func TestValidate_ReportsEveryProblemAtOnce(t *testing.T) {
	// Reporting one fault per run makes fixing a broken config a sequence
	// of restarts.
	cfg := validConfig()
	cfg.Library.Root = ""
	cfg.Capture.Container = "avi"
	cfg.Naming.Template = "{nope}"

	fields := problemFields(t, cfg.Validate())
	for _, want := range []string{"library.root", "capture.container", "naming.template"} {
		if !hasField(fields, want) {
			t.Errorf("Validate() problems = %v, want one naming %q", fields, want)
		}
	}
}

func TestValidationError_Message(t *testing.T) {
	t.Run("single problem reads as one line", func(t *testing.T) {
		err := &ValidationError{Problems: []Problem{{Field: "library.root", Detail: "is required"}}}
		got := err.Error()
		if strings.Contains(got, "\n") {
			t.Errorf("Error() = %q, want a single line for one problem", got)
		}
		if !strings.Contains(got, "library.root") {
			t.Errorf("Error() = %q, want it to name the field", got)
		}
	})

	t.Run("several problems are listed", func(t *testing.T) {
		err := &ValidationError{Problems: []Problem{
			{Field: "library.root", Detail: "is required"},
			{Field: "capture.container", Detail: "must be one of mkv, mp4, ts"},
		}}
		got := err.Error()
		if !strings.Contains(got, "2 problems") {
			t.Errorf("Error() = %q, want it to count the problems", got)
		}
		for _, want := range []string{"library.root", "capture.container"} {
			if !strings.Contains(got, want) {
				t.Errorf("Error() = %q, want it to name %q", got, want)
			}
		}
	})
}

// ///////////////////////////////////////////////
// Read and Load
// ///////////////////////////////////////////////

func TestRead_FillsDefaults(t *testing.T) {
	// A config naming only a root must be complete, so upgrades that add
	// a field do not break an existing file.
	source := strings.NewReader("[library]\nroot = " + fixtureRootTOML() + "\n")

	cfg, err := Read(source)
	if err != nil {
		t.Fatalf("Read() err = %v, want nil", err)
	}
	if cfg.Library.Root != fixtureRoot() {
		t.Errorf("Library.Root = %q, want %q", cfg.Library.Root, fixtureRoot())
	}
	if cfg.Capture.Container != DefaultConfig().Capture.Container {
		t.Errorf("Capture.Container = %q, want the default", cfg.Capture.Container)
	}
	if cfg.Naming.Template != DefaultConfig().Naming.Template {
		t.Errorf("Naming.Template = %q, want the default", cfg.Naming.Template)
	}
}

func TestRead_AcceptsASectionLeftEmpty(t *testing.T) {
	// Strictness has to stop at keys that do not exist. A header an operator
	// left behind while clearing its keys names a real section, so refusing
	// it would make the file harder to edit rather than safer.
	body := "[library]\nroot = " + fixtureRootTOML() + "\n\n[naming]\n\n[space.purge]\n"

	cfg, err := Read(strings.NewReader(body))
	if err != nil {
		t.Fatalf("Read() err = %v, want nil", err)
	}
	if cfg.Naming.Template != DefaultConfig().Naming.Template {
		t.Errorf("Naming.Template = %q, want the default", cfg.Naming.Template)
	}
	if cfg.Space.Purge.ProtectFor != DefaultConfig().Space.Purge.ProtectFor {
		t.Errorf("Space.Purge.ProtectFor = %s, want the default", cfg.Space.Purge.ProtectFor)
	}
}

func TestRead_ParsesUnits(t *testing.T) {
	source := strings.NewReader(`
[library]
root = ` + fixtureRootTOML() + `

[space]
max_size = "500GB"
min_free = "50GiB"

[space.purge]
protect_for = "10d"
`)

	cfg, err := Read(source)
	if err != nil {
		t.Fatalf("Read() err = %v, want nil", err)
	}
	if cfg.Space.MaxSize != 500*Gigabyte {
		t.Errorf("MaxSize = %d, want %d", cfg.Space.MaxSize.Bytes(), (500 * Gigabyte).Bytes())
	}
	if cfg.Space.MinFree != 50*Gibibyte {
		t.Errorf("MinFree = %d, want %d", cfg.Space.MinFree.Bytes(), (50 * Gibibyte).Bytes())
	}
	if cfg.Space.Purge.ProtectFor != 10*Day {
		t.Errorf("ProtectFor = %v, want %v", cfg.Space.Purge.ProtectFor.Std(), (10 * Day).Std())
	}
}

func TestRead_Rejects(t *testing.T) {
	tests := []struct {
		name   string
		source string
	}{
		{name: "malformed toml", source: "[library\nroot = 1"},
		{name: "wrong type", source: "[capture]\nmax_concurrent = \"three\"\n"},
		{name: "unparseable size", source: "[library]\nroot = \"/x\"\nmax_size = \"loads\"\n"},
		{name: "unparseable duration", source: "[capture]\npoll_interval = \"soon\"\n"},
		{name: "fails validation", source: "[capture]\ncontainer = \"avi\"\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Read(strings.NewReader(tt.source)); err == nil {
				t.Error("Read() err = nil, want an error")
			}
		})
	}
}

func TestRead_RejectsAKeyItDoesNotUnderstand(t *testing.T) {
	// Package doc says validation is strict. An ignored key means an
	// operator who caps the library at 500 GB and mistypes the key runs on
	// the 2 TB default until the disk fills, holding a file that reads as
	// if it said otherwise.
	tests := []struct {
		name      string
		body      string
		wantField string
	}{
		{
			name:      "a misspelled key",
			body:      "[library]\nroot = " + fixtureRootTOML() + "\nmax_sze = \"500GB\"\n",
			wantField: "library.max_sze",
		},
		{
			name:      "a misspelled cap",
			body:      "[library]\nroot = " + fixtureRootTOML() + "\n\n[space]\nmax_sixe = \"3.7TB\"\n",
			wantField: "space.max_sixe",
		},
		{
			name: "a misspelled subtable",
			body: "[library]\nroot = " + fixtureRootTOML() +
				"\n\n[space.recompres]\nenabled = true\n",
			wantField: "space.recompres",
		},
		{
			name:      "a key in the wrong section",
			body:      "[library]\nroot = " + fixtureRootTOML() + "\npoll_interval = \"30s\"\n",
			wantField: "library.poll_interval",
		},
		{
			name:      "a section that does not exist",
			body:      "[library]\nroot = " + fixtureRootTOML() + "\n\n[bogus_section]\nanything = 1\n",
			wantField: "bogus_section.anything",
		},
		{
			// A header carries no key to be undecoded, so a check that only
			// walks keys would pass this and leave every capture setting on
			// its default.
			name:      "a misspelled section holding nothing",
			body:      "[library]\nroot = " + fixtureRootTOML() + "\n\n[capturr]\n",
			wantField: "capturr",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Read(strings.NewReader(tt.body))
			fields := problemFields(t, err)
			if !hasField(fields, tt.wantField) {
				t.Errorf("Read() reported %v, want a problem on %s", fields, tt.wantField)
			}
		})
	}
}

func TestRead_NamesWhereAMovedTableWent(t *testing.T) {
	// The decoder drops a table it has no field for, so a file carrying one
	// would load with every space setting at its default while reading as if
	// it set them. That turns a 512 GB cap into the 2 TB default at the
	// moment the library is fullest.
	tests := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "a transcode table",
			body: "[library]\nroot = " + fixtureRootTOML() + "\n\n[transcode]\nenabled = true\ncodec = \"av1\"\n",
			want: []string{"transcode moved to space.recompress", "stream-dvr config init"},
		},
		{
			name: "a retention table",
			body: "[library]\nroot = " + fixtureRootTOML() + "\n\n[retention]\nprotect_for = \"2w\"\n",
			want: []string{"retention moved to space.purge", "stream-dvr config init"},
		},
		{
			name: "an empty transcode table",
			body: "[library]\nroot = " + fixtureRootTOML() + "\n\n[transcode]\n",
			want: []string{"transcode moved to space.recompress"},
		},
		{
			name: "both tables",
			body: "[library]\nroot = " + fixtureRootTOML() + "\n\n[transcode]\nenabled = true\n\n[retention]\nage_weight = 2.0\n",
			want: []string{"transcode moved to space.recompress", "retention moved to space.purge"},
		},
		{
			// A file carrying both shapes is still the old one. Reading the
			// new keys and dropping the rest would hide the settings that
			// were meant to survive.
			name: "a stale table beside the new ones",
			body: "[library]\nroot = " + fixtureRootTOML() + "\n\n[space]\nmax_size = \"512GiB\"\n\n[transcode]\nenabled = true\n",
			want: []string{"transcode moved to space.recompress"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Read(strings.NewReader(tt.body))
			if err == nil {
				t.Fatal("Read() err = nil, want a refusal naming where the table went")
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Read() err = %q, want it to say %q", err, want)
				}
			}
		})
	}
}

func TestRead_AcceptsEverySettingInItsPlace(t *testing.T) {
	// Every key the generated config documents must parse from the path it
	// documents, or an operator following the file lands on a default.
	body := "schema_version = 1\n\n" +
		"[library]\nroot = " + fixtureRootTOML() + "\n\n" +
		"[space]\nmax_size = \"512GiB\"\nmin_free = \"250GB\"\n\n" +
		"[space.recompress]\nenabled = true\nafter = \"14d\"\ncodec = \"av1\"\nquality = 28\n" +
		"prefer_hardware = false\nmax_concurrent = 2\nkeep_original = true\n\n" +
		"[space.purge]\nwatched_weight = 4.0\nage_weight = 1.5\nrefetchable_weight = 2.5\n" +
		"protect_for = \"2w\"\ntrash_grace = \"3d\"\n\n" +
		"[capture]\npoll_interval = \"45s\"\nquality = [\"1080p60\", \"best\"]\nmin_duration = \"3m\"\n" +
		"max_concurrent = 2\ncontainer = \"mp4\"\n\n" +
		"[naming]\ntemplate = \"{channel}/{date} {title}.{ext}\"\ntimezone = \"UTC\"\n\n" +
		"[notify]\ndesktop = false\nwebhook_url = \"https://ntfy.sh/topic\"\n" +
		"on_recording_start = true\non_failure = false\non_library_full = false\n\n" +
		"[[channels]]\nplatform = \"twitch\"\nname = \"examplechannel\"\nenabled = true\n" +
		"quality = [\"720p60\"]\nbackfill = true\n"

	cfg, err := Read(strings.NewReader(body))
	if err != nil {
		t.Fatalf("Read() err = %v, want nil", err)
	}

	tests := []struct {
		field string
		got   any
		want  any
	}{
		{field: "space.max_size", got: cfg.Space.MaxSize, want: 512 * Gibibyte},
		{field: "space.min_free", got: cfg.Space.MinFree, want: 250 * Gigabyte},
		{field: "space.recompress.enabled", got: cfg.Space.Recompress.Enabled, want: true},
		{field: "space.recompress.after", got: cfg.Space.Recompress.After, want: 14 * Day},
		{field: "space.recompress.codec", got: cfg.Space.Recompress.Codec, want: CodecAV1},
		{field: "space.recompress.quality", got: cfg.Space.Recompress.Quality, want: 28},
		{field: "space.recompress.prefer_hardware", got: cfg.Space.Recompress.PreferHardware, want: false},
		{field: "space.recompress.max_concurrent", got: cfg.Space.Recompress.MaxConcurrent, want: 2},
		{field: "space.recompress.keep_original", got: cfg.Space.Recompress.KeepOriginal, want: true},
		{field: "space.purge.watched_weight", got: cfg.Space.Purge.WatchedWeight, want: 4.0},
		{field: "space.purge.age_weight", got: cfg.Space.Purge.AgeWeight, want: 1.5},
		{field: "space.purge.refetchable_weight", got: cfg.Space.Purge.RefetchableWeight, want: 2.5},
		{field: "space.purge.protect_for", got: cfg.Space.Purge.ProtectFor, want: 2 * Week},
		{field: "space.purge.trash_grace", got: cfg.Space.Purge.TrashGrace, want: 3 * Day},
		{field: "capture.container", got: cfg.Capture.Container, want: "mp4"},
		{field: "naming.timezone", got: cfg.Naming.Timezone, want: "UTC"},
		{field: "notify.on_recording_start", got: cfg.Notify.OnRecordingStart, want: true},
		{field: "channels[0].backfill", got: cfg.Channels[0].Backfill, want: true},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s = %v, want %v", tt.field, tt.got, tt.want)
		}
	}
}

func TestRead_TrimsValuesBeforeTheyAreUsed(t *testing.T) {
	// Read normalizes before it validates, so the value that passes every
	// check is the value the daemon opens, watches, and posts to.
	body := "[library]\nroot = '  " + fixtureRoot() + "  '\n\n[naming]\ntimezone = \"  UTC  \"\n\n" +
		"[notify]\nwebhook_url = \"  https://ntfy.sh/topic  \"\n\n" +
		"[[channels]]\nplatform = \"twitch\"\nname = \"  examplechannel\t\"\nenabled = true\n"

	cfg, err := Read(strings.NewReader(body))
	if err != nil {
		t.Fatalf("Read() err = %v, want nil", err)
	}

	tests := []struct {
		field string
		got   string
		want  string
	}{
		{field: "library.root", got: cfg.Library.Root, want: fixtureRoot()},
		{field: "naming.timezone", got: cfg.Naming.Timezone, want: "UTC"},
		{field: "notify.webhook_url", got: cfg.Notify.WebhookURL, want: "https://ntfy.sh/topic"},
		{field: "channels[0].name", got: cfg.Channels[0].Name, want: "examplechannel"},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s = %q, want %q", tt.field, tt.got, tt.want)
		}
	}
}

func TestLoad_MissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent.toml"))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Load() err = %v, want it to wrap ErrNotFound", err)
	}
}

func TestSave_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.toml")

	want := validConfig()
	want.Space.MaxSize = 500 * Gigabyte
	want.Space.Purge.ProtectFor = 10 * Day
	want.Channels = []Channel{
		{Platform: PlatformTwitch, Name: "examplechannel", Enabled: true, Backfill: true},
		{Platform: PlatformYouTube, Name: "someone", Enabled: false, Quality: []string{"best"}},
	}

	if err := Save(path, want); err != nil {
		t.Fatalf("Save() err = %v, want nil", err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load() err = %v, want nil", err)
	}

	if got.Space.MaxSize != want.Space.MaxSize {
		t.Errorf("MaxSize = %s, want %s", got.Space.MaxSize, want.Space.MaxSize)
	}
	if got.Space.Purge.ProtectFor != want.Space.Purge.ProtectFor {
		t.Errorf("ProtectFor = %s, want %s", got.Space.Purge.ProtectFor, want.Space.Purge.ProtectFor)
	}
	if len(got.Channels) != len(want.Channels) {
		t.Fatalf("Channels = %d entries, want %d", len(got.Channels), len(want.Channels))
	}
	for i := range want.Channels {
		if got.Channels[i].Name != want.Channels[i].Name {
			t.Errorf("Channels[%d].Name = %q, want %q", i, got.Channels[i].Name, want.Channels[i].Name)
		}
		if got.Channels[i].Enabled != want.Channels[i].Enabled {
			t.Errorf("Channels[%d].Enabled = %t, want %t", i, got.Channels[i].Enabled, want.Channels[i].Enabled)
		}
	}
}

func TestValidate_AcceptsASelfHostedReceiver(t *testing.T) {
	// Loopback is deliberately allowed. A receiver on the same machine is
	// the ordinary configuration for a tool that runs on one machine, and
	// refusing it would push operators onto a public service instead.
	for _, address := range []string{
		"http://127.0.0.1:8080/hooks/dvr",
		"http://localhost:8080/hooks/dvr",
		"http://[::1]:8080/hooks/dvr",
	} {
		t.Run(address, func(t *testing.T) {
			cfg := validConfig()
			cfg.Notify.WebhookURL = address

			if err := cfg.Validate(); err != nil {
				t.Errorf("Validate() err = %v, want a self-hosted receiver accepted", err)
			}
		})
	}
}

func TestSave_KeepsTheDocumentation(t *testing.T) {
	// The settings editor writes through Save. An operator whose config
	// lost every comment the first time they changed a setting from the
	// interface would reasonably stop using the interface.
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg := validConfig()
	cfg.Channels = []Channel{{Platform: PlatformTwitch, Name: "examplechannel", Enabled: true}}

	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save() err = %v, want nil", err)
	}

	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	want := []string{
		"# stream-dvr config.",
		"# Cap on total recording bytes.",
		"# Where recordings live.",
	}
	for _, comment := range want {
		if !strings.Contains(string(saved), comment) {
			t.Errorf("the saved config lost %q:\n%s", comment, saved)
		}
	}
}

func TestSave_RoundTripsEveryChannelCount(t *testing.T) {
	// A channel list is the config change an operator makes most often, and
	// the renderer treats an array of tables differently from every other
	// shape in the file.
	tests := []struct {
		name     string
		channels []Channel
	}{
		{name: "no channels at all", channels: nil},
		{
			name:     "one channel",
			channels: []Channel{{Platform: PlatformTwitch, Name: "examplechannel", Enabled: true}},
		},
		{
			name: "three channels with mixed settings",
			channels: []Channel{
				{Platform: PlatformTwitch, Name: "examplechannel", Enabled: true, Backfill: true},
				{Platform: PlatformYouTube, Name: "someone", Quality: []string{"1080p60", "720p", "best"}},
				{Platform: PlatformTwitch, Name: "another", Enabled: true, Quality: []string{"best"}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			want := validConfig()
			want.Channels = tt.channels
			want.Space.MaxSize = 500 * Gigabyte
			want.Space.Purge.ProtectFor = 10 * Day
			want.Capture.Quality = []string{"1080p60", "1080p", "best"}
			want.Notify.WebhookURL = ""

			if err := Save(path, want); err != nil {
				t.Fatalf("Save() err = %v, want nil", err)
			}
			got, err := Load(path)
			if err != nil {
				t.Fatalf("Load() err = %v, want nil", err)
			}

			// A file with no channel list decodes onto DefaultConfig, whose
			// Channels is empty rather than nil. Nothing distinguishes the
			// two at any call site.
			if want.Channels == nil {
				want.Channels = []Channel{}
			}

			if !reflect.DeepEqual(got, want) {
				t.Errorf("the config did not survive the round trip:\ngot  %+v\nwant %+v", got, want)
			}
		})
	}
}

func TestSave_KeepsTheExactSizesTheOperatorSet(t *testing.T) {
	// Save encodes through MarshalText. Writing the rounded display form
	// there means every command that saves the config quietly rewrites the
	// cap: a 1.1 TB setting drifts by 488 MB, and the largest size there is
	// fails to parse back at all.
	tests := []struct {
		name string
		size Size
	}{
		{name: "a round decimal cap", size: 500 * Gigabyte},
		{name: "a round binary cap", size: 512 * Gibibyte},
		{name: "a byte count with no round unit", size: 1023},
		{name: "a terabyte and a byte", size: Terabyte + 1},
		{name: "one byte over a tebibyte", size: Tebibyte + 1},
		{name: "the largest size there is", size: Size(maxInt64)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			cfg := validConfig()
			cfg.Space.MaxSize = tt.size

			if err := Save(path, cfg); err != nil {
				t.Fatalf("Save() err = %v, want nil", err)
			}
			got, err := Load(path)
			if err != nil {
				t.Fatalf("Load() err = %v, want nil", err)
			}
			if got.Space.MaxSize != tt.size {
				t.Errorf("MaxSize came back as %d bytes, want the %d that were saved",
					got.Space.MaxSize.Bytes(), tt.size.Bytes())
			}
		})
	}
}

func TestSave_RestrictsTheFileToItsOwner(t *testing.T) {
	// The config can carry a webhook URL whose path is the only thing
	// authorizing a post to it, and on Windows it is also a value that
	// reaches a subprocess argument list.
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := Save(path, validConfig()); err != nil {
		t.Fatalf("Save() err = %v, want nil", err)
	}

	assertOwnerOnly(t, path)
}

func TestInit_RestrictsTheFileToItsOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := Init(path); err != nil {
		t.Fatalf("Init() err = %v, want nil", err)
	}

	assertOwnerOnly(t, path)
}

func TestSave_SucceedsWithALeftoverStagedFile(t *testing.T) {
	// A save killed between staging and rename leaves a file matching the
	// name the next save stages under. That leftover must not wedge every
	// save after it, which would leave the operator unable to change the
	// config at all. The name here is the pattern fsretry stages with, so it
	// is the collision a real crash leaves rather than an unrelated file.
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path+".tmp2751904", []byte("half a config"), 0o600); err != nil {
		t.Fatalf("writing the leftover staged file: %v", err)
	}

	want := validConfig()
	want.Capture.MaxConcurrent = 7
	if err := Save(path, want); err != nil {
		t.Fatalf("Save() err = %v, want nil", err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load() err = %v, want nil", err)
	}
	if got.Capture.MaxConcurrent != want.Capture.MaxConcurrent {
		t.Errorf("MaxConcurrent = %d, want %d", got.Capture.MaxConcurrent, want.Capture.MaxConcurrent)
	}
}

func TestSave_LeavesNoTemporaryFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := Save(path, validConfig()); err != nil {
		t.Fatalf("Save() err = %v, want nil", err)
	}

	matches, err := filepath.Glob(path + ".tmp*")
	if err != nil {
		t.Fatalf("globbing for a staged file: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("staged files left behind: %v, want none", matches)
	}
}

// ///////////////////////////////////////////////
// Render and Init
// ///////////////////////////////////////////////

func TestRender_ParsesBackWithARootSet(t *testing.T) {
	rendered, err := Render()
	if err != nil {
		t.Fatalf("Render() err = %v, want nil", err)
	}

	source := strings.Replace(string(rendered), `root = ""`, "root = "+fixtureRootTOML(), 1)
	cfg, err := Read(strings.NewReader(source))
	if err != nil {
		t.Fatalf("Read(Render()) err = %v, want nil", err)
	}
	if cfg.Library.Root != fixtureRoot() {
		t.Errorf("Library.Root = %q, want the edited value", cfg.Library.Root)
	}
}

func TestRender_ChannelExampleParsesWhenUncommented(t *testing.T) {
	// The generated file tells the operator to add [[channels]] blocks. A
	// "channels = []" key in the same file makes TOML reject the first one
	// they add, so following the instructions must not break the file.
	rendered, err := Render()
	if err != nil {
		t.Fatalf("Render() err = %v, want nil", err)
	}

	var b strings.Builder
	for line := range strings.SplitSeq(string(rendered), "\n") {
		trimmed := strings.TrimPrefix(strings.TrimPrefix(line, "# "), "#")
		switch {
		case strings.HasPrefix(trimmed, "[[channels]]"),
			strings.HasPrefix(trimmed, "platform ="),
			strings.HasPrefix(trimmed, "name ="),
			strings.HasPrefix(trimmed, "enabled ="),
			strings.HasPrefix(trimmed, "backfill ="):
			b.WriteString(trimmed)
		case strings.HasPrefix(line, `root = ""`):
			b.WriteString("root = " + fixtureRootTOML())
		default:
			b.WriteString(line)
		}
		b.WriteString("\n")
	}

	cfg, err := Read(strings.NewReader(b.String()))
	if err != nil {
		t.Fatalf("Read() after uncommenting the channel example err = %v, want nil", err)
	}
	if len(cfg.Channels) != 1 {
		t.Fatalf("Channels = %d, want the uncommented example to produce 1", len(cfg.Channels))
	}
	if cfg.Channels[0].Platform != PlatformTwitch {
		t.Errorf("Channels[0].Platform = %q, want %q", cfg.Channels[0].Platform, PlatformTwitch)
	}
}

func TestInit(t *testing.T) {
	t.Run("writes a commented config", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "nested", "config.toml")

		if err := Init(path); err != nil {
			t.Fatalf("Init() err = %v, want nil", err)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading written config: %v", err)
		}
		if !strings.Contains(string(content), "# Library directory") {
			t.Error("Init() wrote a config with no field comments")
		}
	})

	t.Run("refuses to overwrite", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := Init(path); err != nil {
			t.Fatalf("first Init() err = %v, want nil", err)
		}
		if err := Init(path); err == nil {
			t.Error("second Init() err = nil, want a refusal so a channel list is never clobbered")
		}
	})
}

// ///////////////////////////////////////////////
// Derived values
// ///////////////////////////////////////////////

func TestConfig_EnabledChannels(t *testing.T) {
	cfg := validConfig()
	cfg.Channels = []Channel{
		{Platform: PlatformTwitch, Name: "on", Enabled: true},
		{Platform: PlatformTwitch, Name: "off", Enabled: false},
	}

	got := cfg.EnabledChannels()
	if len(got) != 1 || got[0].Name != "on" {
		t.Errorf("EnabledChannels() = %v, want only the enabled channel", got)
	}
}

func TestConfig_QualityFor(t *testing.T) {
	cfg := validConfig()

	t.Run("channel override wins", func(t *testing.T) {
		override := []string{"720p"}
		got := cfg.QualityFor(Channel{Quality: override})
		if len(got) != 1 || got[0] != "720p" {
			t.Errorf("QualityFor() = %v, want %v", got, override)
		}
	})

	t.Run("falls back to the capture ladder", func(t *testing.T) {
		got := cfg.QualityFor(Channel{})
		if len(got) != len(cfg.Capture.Quality) {
			t.Errorf("QualityFor() = %v, want the capture ladder %v", got, cfg.Capture.Quality)
		}
	})
}

func TestConfig_Location(t *testing.T) {
	tests := []struct {
		name     string
		timezone string
		want     string
	}{
		{name: "empty means local", timezone: "", want: "Local"},
		{name: "local by name", timezone: "Local", want: "Local"},
		{name: "local is case insensitive", timezone: "local", want: "Local"},
		{name: "named zone", timezone: "UTC", want: "UTC"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Naming.Timezone = tt.timezone

			loc, err := cfg.Location()
			if err != nil {
				t.Fatalf("Location() err = %v, want nil", err)
			}
			if loc.String() != tt.want {
				t.Errorf("Location() = %q, want %q", loc, tt.want)
			}
		})
	}
}

func TestValidate_AllowsBackfillOnAListableChannel(t *testing.T) {
	// The refusal is about a url channel having no listing to search, not
	// about backfill itself. A platform that lists past broadcasts must
	// pass, or the setting is unreachable everywhere.
	for _, platform := range []string{PlatformTwitch, PlatformYouTube} {
		t.Run(platform, func(t *testing.T) {
			cfg := validConfig()
			cfg.Channels = []Channel{{
				Platform: platform,
				Name:     "examplechannel",
				Enabled:  true,
				Backfill: true,
			}}

			if err := cfg.Validate(); err != nil {
				t.Errorf("Validate() err = %v, want backfill allowed on %s", err, platform)
			}
		})
	}
}

// TestValidate_RefusesABackfillBlockAPassCannotRun covers the fields that
// reach code treating them as already sane.
//
// The block is always judged. There is no switch turning it off, so a value
// nothing checked cannot sit in a file until the day a pass is run.
func TestValidate_RefusesABackfillBlockAPassCannotRun(t *testing.T) {
	valid := func(t *testing.T) Config {
		t.Helper()
		cfg := DefaultConfig()
		cfg.Library.Root = t.TempDir()
		return cfg
	}

	t.Run("the defaults are valid", func(t *testing.T) {
		if err := valid(t).Validate(); err != nil {
			t.Fatalf("Validate() err = %v, want nil for the defaults", err)
		}
	})

	cases := []struct {
		name  string
		spoil func(*Config)
		field string
	}{
		{"a negative settle", func(c *Config) { c.Backfill.Settle = -1 }, "backfill.settle"},
		{"no concurrency", func(c *Config) { c.Backfill.MaxConcurrent = 0 }, "backfill.max_concurrent"},
		{"an uncapped attempt count", func(c *Config) { c.Backfill.MaxAttempts = 0 }, "backfill.max_attempts"},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid(t)
			tt.spoil(&cfg)

			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate() err = nil, want %s refused", tt.field)
			}
			if !strings.Contains(err.Error(), tt.field) {
				t.Errorf("err = %v, want it to name %s so the operator knows which key to fix", err, tt.field)
			}
		})
	}
}

// ///////////////////////////////////////////////
// Library root
// ///////////////////////////////////////////////

func TestSetLibraryRoot_MakesTheConfigLoad(t *testing.T) {
	// This is the whole point of the function. A library root written by a
	// command is the one an operator otherwise has to hand-edit in between
	// creating a library and being told the library.root is missing.
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := Init(path); err != nil {
		t.Fatalf("Init() err = %v, want nil", err)
	}

	if err := SetLibraryRoot(path, fixtureRoot(), ""); err != nil {
		t.Fatalf("SetLibraryRoot() err = %v, want nil", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() after SetLibraryRoot err = %v, want a config that loads with no hand-edit", err)
	}
	if cfg.Library.Root != fixtureRoot() {
		t.Errorf("Library.Root = %q, want %q", cfg.Library.Root, fixtureRoot())
	}
}

func TestSetLibraryRoot_WritesAConfigWhenNoneStandsThere(t *testing.T) {
	// A first run can create the library before it creates the config, and
	// erroring there would send the operator to run 'config init' and then
	// repeat the command that just worked.
	path := filepath.Join(t.TempDir(), "nested", "config.toml")

	if err := SetLibraryRoot(path, fixtureRoot(), ""); err != nil {
		t.Fatalf("SetLibraryRoot() err = %v, want nil", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() err = %v, want a usable config", err)
	}
	if cfg.Library.Root != fixtureRoot() {
		t.Errorf("Library.Root = %q, want %q", cfg.Library.Root, fixtureRoot())
	}
}

func TestSetLibraryRoot_KeepsWhatTheFileAlreadyHeld(t *testing.T) {
	// The rewrite renders the whole file. An operator who set a cap and
	// listed channels before pointing at a library must not lose either.
	path := filepath.Join(t.TempDir(), "config.toml")
	seed := DefaultConfig()
	seed.Space.MaxSize = 500 * Gigabyte
	seed.Notify.WebhookURL = "https://example.com/hooks/dvr"
	seed.Channels = []Channel{
		{Platform: PlatformTwitch, Name: "examplechannel", Enabled: true, Backfill: true},
		{Platform: PlatformYouTube, Name: "someone", Enabled: false},
	}
	if err := Save(path, seed); err != nil {
		t.Fatalf("Save() err = %v, want nil", err)
	}

	if err := SetLibraryRoot(path, fixtureRoot(), ""); err != nil {
		t.Fatalf("SetLibraryRoot() err = %v, want nil", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() err = %v, want nil", err)
	}
	if cfg.Space.MaxSize != seed.Space.MaxSize {
		t.Errorf("Space.MaxSize = %s, want the %s the file held", cfg.Space.MaxSize, seed.Space.MaxSize)
	}
	if cfg.Notify.WebhookURL != seed.Notify.WebhookURL {
		t.Errorf("Notify.WebhookURL = %q, want %q", cfg.Notify.WebhookURL, seed.Notify.WebhookURL)
	}
	if len(cfg.Channels) != len(seed.Channels) {
		t.Fatalf("Channels = %d entries, want the %d the file held", len(cfg.Channels), len(seed.Channels))
	}
	for i := range seed.Channels {
		if cfg.Channels[i].Name != seed.Channels[i].Name {
			t.Errorf("Channels[%d].Name = %q, want %q", i, cfg.Channels[i].Name, seed.Channels[i].Name)
		}
	}
}

func TestSetLibraryRoot_LeavesAFileItCannotRewriteAlone(t *testing.T) {
	// The rewrite renders from the decoded struct, so anything the decoder
	// dropped is gone from the file. An operator who mistyped a key would
	// lose the setting they meant and the evidence of the typo in one write,
	// and the config would then read as if they had never set it.
	tests := []struct {
		name    string
		content string
		names   string
	}{
		{
			name:    "a key this build does not define",
			content: "[space]\nmax_sizes = '500GB'\n",
			names:   "max_sizes",
		},
		{
			name:    "a table whose settings moved",
			content: "[transcode]\ncodec = 'hevc'\n",
			names:   "space.recompress",
		},
		{
			name:    "a file that is not TOML",
			content: "this is not a config {{{\n",
			names:   "parsing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatalf("seeding %s: %v", path, err)
			}

			err := SetLibraryRoot(path, fixtureRoot(), "")
			if err == nil {
				t.Fatal("SetLibraryRoot() err = nil, want a refusal rather than a silent rewrite")
			}
			if !strings.Contains(err.Error(), tt.names) {
				t.Errorf("err = %v, want it to name %s so the operator knows what to fix", err, tt.names)
			}

			after, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatalf("reading %s: %v", path, readErr)
			}
			if string(after) != tt.content {
				t.Errorf("the refused file was rewritten anyway:\n%s", after)
			}
		})
	}
}

func TestLibraryRoot_ReportsWhatTheConfigNames(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{name: "no file at all", content: "", want: ""},
		{name: "a config with no root", content: "[library]\nroot = ''\n", want: ""},
		{name: "a config naming a library", content: "[library]\nroot = " + fixtureRootTOML() + "\n", want: fixtureRoot()},
		{
			// Read trims before it validates, so a caller comparing this
			// against a path has to be given the trimmed form too.
			name:    "a root an editor padded",
			content: "[library]\nroot = '  " + fixtureRoot() + "  '\n",
			want:    fixtureRoot(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if tt.content != "" {
				if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
					t.Fatalf("seeding %s: %v", path, err)
				}
			}

			got, err := LibraryRoot(path)
			if err != nil {
				t.Fatalf("LibraryRoot() err = %v, want nil", err)
			}
			if got != tt.want {
				t.Errorf("LibraryRoot() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLibraryRoot_ReportsWhateverWouldStopTheWrite(t *testing.T) {
	// A caller asks this first so it can refuse before it acts. A getter
	// that answered "" for a file the write would refuse would let it act
	// and fail afterwards, having already made a library.
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("nonsense = true\n"), 0o600); err != nil {
		t.Fatalf("seeding %s: %v", path, err)
	}

	if _, err := LibraryRoot(path); err == nil {
		t.Error("LibraryRoot() err = nil, want the refusal SetLibraryRoot would give")
	}
}

func TestSetLibraryRoot_KeepsWhatTheOperatorWroteInTheFile(t *testing.T) {
	// A config is a file somebody edits by hand. Notes, ticket numbers and
	// commented-out settings are how an operator leaves themselves the next
	// step, and the shipped template teaches that style with its own
	// commented alternatives. A command that regenerated the file would
	// delete every one of them and say nothing.
	path := filepath.Join(t.TempDir(), "config.toml")
	original := "" +
		"schema_version = 1\n" +
		"\n" +
		"[library]\n" +
		"# MY NOTE: this volume is the slow one, see OPS-4417\n" +
		"root = ''\n" +
		"\n" +
		"[space]\n" +
		"# try this next month\n" +
		"# min_free = '400GB'\n" +
		"max_size = '500GB'\n" +
		"\n" +
		"[[channels]]\n" +
		"platform = 'twitch'\n" +
		"name = 'examplechannel'\n" +
		"enabled = true\n" +
		"# name = 'thebackupchannel'\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatalf("seeding %s: %v", path, err)
	}

	if err := SetLibraryRoot(path, fixtureRoot(), ""); err != nil {
		t.Fatalf("SetLibraryRoot() err = %v, want nil", err)
	}

	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	for _, kept := range []string{
		"MY NOTE", "OPS-4417", "try this next month",
		"# min_free = '400GB'", "# name = 'thebackupchannel'",
	} {
		if !strings.Contains(string(saved), kept) {
			t.Errorf("the rewrite deleted %q:\n%s", kept, saved)
		}
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() err = %v, want the patched file to load", err)
	}
	if cfg.Library.Root != fixtureRoot() {
		t.Errorf("Library.Root = %q, want %q", cfg.Library.Root, fixtureRoot())
	}
	if cfg.Space.MaxSize != 500*Gigabyte {
		t.Errorf("Space.MaxSize = %s, want the 500GB the file set", cfg.Space.MaxSize)
	}
	if len(cfg.Channels) != 1 || cfg.Channels[0].Name != "examplechannel" {
		t.Errorf("Channels = %+v, want the one the file listed", cfg.Channels)
	}
}

func TestSetLibraryRoot_ChangesOnlyTheRootLine(t *testing.T) {
	// The patch is a line edit, so the proof is that one line differs.
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := Init(path); err != nil {
		t.Fatalf("Init() err = %v, want nil", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	if err := SetLibraryRoot(path, fixtureRoot(), ""); err != nil {
		t.Fatalf("SetLibraryRoot() err = %v, want nil", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	was := strings.Split(string(before), "\n")
	now := strings.Split(string(after), "\n")
	if len(was) != len(now) {
		t.Fatalf("the file went from %d lines to %d, want a single line replaced", len(was), len(now))
	}
	changed := 0
	for i := range was {
		if was[i] != now[i] {
			changed++
		}
	}
	if changed != 1 {
		t.Errorf("%d lines differ, want exactly the root's own line", changed)
	}
}

func TestSetLibraryRoot_RendersTheFileWhenNoRootLineExists(t *testing.T) {
	// A file that never named the root has no line to patch. Rendering it is
	// the fallback, and the value still has to land.
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("schema_version = 1\n"), 0o600); err != nil {
		t.Fatalf("seeding %s: %v", path, err)
	}

	if err := SetLibraryRoot(path, fixtureRoot(), ""); err != nil {
		t.Fatalf("SetLibraryRoot() err = %v, want nil", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() err = %v, want nil", err)
	}
	if cfg.Library.Root != fixtureRoot() {
		t.Errorf("Library.Root = %q, want %q", cfg.Library.Root, fixtureRoot())
	}
}

func TestSetLibraryRoot_RefusesARootTheConfigWouldNotAccept(t *testing.T) {
	// Writing a value Load then refuses would leave the operator with a
	// config no command can read and nothing saying which line to fix.
	tests := []struct {
		name string
		root string
	}{
		{name: "an invisible character", root: "/srv/vo\u200bds"},
		{name: "a terminal escape", root: "/srv/\x1b[2Jvods"},
		{name: "a newline", root: "/srv/vods\nroot = '/etc'"},
		{name: "a relative path", root: "vods"},
		{name: "nothing at all", root: "   "},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := Init(path); err != nil {
				t.Fatalf("Init() err = %v, want nil", err)
			}

			if err := SetLibraryRoot(path, tt.root, ""); err == nil {
				t.Fatal("SetLibraryRoot() err = nil, want a root the config refuses rejected")
			}

			got, err := LibraryRoot(path)
			if err != nil {
				t.Fatalf("LibraryRoot() err = %v", err)
			}
			if got != "" {
				t.Errorf("Library.Root = %q, want the refused root never written", got)
			}
		})
	}
}

func TestSetLibraryRoot_KeepsACommentOnTheRootsOwnLine(t *testing.T) {
	// The root's line is the one an operator annotates, because it is where
	// they say which volume this is. Replacing the whole line takes the note
	// with it, and decodesTo cannot object because a comment is not part of
	// what a config decodes to.
	path := filepath.Join(t.TempDir(), "config.toml")
	seed := "[library]\nroot = '/old'  # the NAS volume, not the SSD, see OPS-4417\n"
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatalf("seeding %s: %v", path, err)
	}

	if err := SetLibraryRoot(path, fixtureRoot(), "/old"); err != nil {
		t.Fatalf("SetLibraryRoot() err = %v, want nil", err)
	}

	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if !strings.Contains(string(saved), "OPS-4417") {
		t.Errorf("the note on the root's line was deleted:\n%s", saved)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() err = %v, want the patched file to load", err)
	}
	if cfg.Library.Root != fixtureRoot() {
		t.Errorf("Library.Root = %q, want %q", cfg.Library.Root, fixtureRoot())
	}
}

func TestTrailingComment(t *testing.T) {
	// A library path may hold a "#", and half a path is not a comment.
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "no comment", body: `root = '/srv/vods'`, want: ""},
		{name: "a comment after the value", body: `root = '/srv/vods' # note`, want: "# note"},
		{name: "a hash inside a literal string", body: `root = '/srv/#1/vods'`, want: ""},
		{name: "a hash inside a basic string", body: `root = "/srv/#1/vods"`, want: ""},
		{name: "a hash inside a string and a real comment", body: `root = '/srv/#1' # note`, want: "# note"},
		{name: "an escaped quote before a hash", body: `root = "/srv/\"x" # note`, want: "# note"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := trailingComment(tt.body); got != tt.want {
				t.Errorf("trailingComment(%q) = %q, want %q", tt.body, got, tt.want)
			}
		})
	}
}

func TestSetLibraryRoot_PatchesEvenWithANotANumberWeight(t *testing.T) {
	// A purge weight is a float64 and Validate accepts a NaN, since NaN < 0
	// is false. Comparing the decoded structs would answer "different" for a
	// config nothing touched, so every write on such a file would fall back
	// to the full render and lose every comment in it, permanently.
	path := filepath.Join(t.TempDir(), "config.toml")
	seed := "[library]\nroot = ''\n\n[space.purge]\nwatched_weight = nan\n\n# OPERATOR NOTE keep-me\n"
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatalf("seeding %s: %v", path, err)
	}

	if err := SetLibraryRoot(path, fixtureRoot(), ""); err != nil {
		t.Fatalf("SetLibraryRoot() err = %v, want nil", err)
	}

	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if !strings.Contains(string(saved), "OPERATOR NOTE keep-me") {
		t.Errorf("a NaN weight forced the full render and deleted the note:\n%s", saved)
	}
}

func TestSetLibraryRoot_RefusesAConfigThatMovedUnderneathIt(t *testing.T) {
	// Two commands that both found the config free would both create a
	// library, and the second write would silently orphan the first.
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := Init(path); err != nil {
		t.Fatalf("Init() err = %v, want nil", err)
	}
	if err := SetLibraryRoot(path, fixtureRoot(), ""); err != nil {
		t.Fatalf("seeding the root: %v", err)
	}

	// A second command that read the config while it was still empty.
	other := filepath.Join(fixtureRoot(), "..", "second")
	err := SetLibraryRoot(path, filepath.Clean(other), "")
	if err == nil {
		t.Fatal("SetLibraryRoot() err = nil, want a refusal for a config that changed")
	}
	for _, want := range []string{fixtureRoot(), "run the command again"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to name %q", err, want)
		}
	}

	got, readErr := LibraryRoot(path)
	if readErr != nil {
		t.Fatalf("LibraryRoot() err = %v", readErr)
	}
	if got != fixtureRoot() {
		t.Errorf("Library.Root = %q, want the first write kept at %q", got, fixtureRoot())
	}
}

func TestSave_FollowsASymlinkedConfig(t *testing.T) {
	// An operator keeping their config in a dotfiles checkout points a link
	// at it. A rename over the link leaves a regular file in its place, and
	// the real config is never edited again however many times they run this.
	//
	// Staged inside home, because that is the boundary a link is followed
	// within: a link anywhere else is one somebody with write access to
	// that directory could have planted. A dotfiles checkout lives in home
	// by definition, so this is the supported case rather than a
	// convenient one.
	dir := homeTempDir(t)
	real := filepath.Join(dir, "dotfiles", "config.toml")
	if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
		t.Fatalf("creating the dotfiles directory: %v", err)
	}
	if err := os.WriteFile(real, []byte("[library]\nroot = ''\n"), 0o600); err != nil {
		t.Fatalf("seeding %s: %v", real, err)
	}

	link := filepath.Join(dir, "config.toml")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("this platform refuses a symlink without extra privilege: %v", err)
	}

	if err := SetLibraryRoot(link, fixtureRoot(), ""); err != nil {
		t.Fatalf("SetLibraryRoot() err = %v, want nil", err)
	}

	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("reading %s: %v", link, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("the link was replaced by a regular file, so the real config is orphaned")
	}

	behind, err := Load(real)
	if err != nil {
		t.Fatalf("Load(%q) err = %v, want the real file written", real, err)
	}
	if behind.Library.Root != fixtureRoot() {
		t.Errorf("the file behind the link has root %q, want %q", behind.Library.Root, fixtureRoot())
	}
}

func TestValidate_RefusesANonFinitePurgeWeight(t *testing.T) {
	// TOML has a literal for each of these, and both answer false to a
	// `< 0` test. A weight that is not a number makes every candidate
	// score alike, so the ranking falls through to its size tie-break and
	// the purge pane lists the largest recording first, silently, on the
	// one screen that removes recordings.
	tests := []struct {
		name  string
		value string
	}{
		{name: "nan", value: "nan"},
		{name: "positive infinity", value: "inf"},
		{name: "negative infinity", value: "-inf"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Read(strings.NewReader(
				"schema_version = 1\n" +
					"[library]\n" +
					`root = "/srv/vods"` + "\n" +
					"[space.purge]\n" +
					"age_weight = " + tt.value + "\n"))
			if err == nil {
				t.Fatalf("Read() accepted age_weight = %s, want a refusal", tt.value)
			}
			if !strings.Contains(err.Error(), "space.purge.age_weight") {
				t.Errorf("Read() err = %v, want it to name the field", err)
			}
		})
	}
}

func TestUnderHome_BoundsWhereALinkMayBeFollowed(t *testing.T) {
	// A link is a value somebody else can set: on Linux and macOS write
	// access to a directory is enough to plant one. Following it anywhere
	// lets whoever planted it choose the file the next save overwrites,
	// with the operator's channel names and library root inside it. Home is
	// the boundary because the case this exists for is a dotfiles checkout,
	// and an attacker who can already write inside home has the account.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory on this machine: %v", err)
	}

	tests := []struct {
		name string
		path string
		want bool
	}{
		{name: "a file in home", path: filepath.Join(home, "config.toml"), want: true},
		{name: "a dotfiles checkout in home", path: filepath.Join(home, "dotfiles", "stream-dvr.toml"), want: true},
		{name: "home itself", path: home, want: true},
		{name: "a sibling of home", path: filepath.Join(filepath.Dir(home), "someone-else", "x.toml"), want: false},
		{name: "a path that walks out of home", path: filepath.Join(home, "..", "elsewhere.toml"), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := underHome(tt.path); got != tt.want {
				t.Errorf("underHome(%q) = %t, want %t", tt.path, got, tt.want)
			}
		})
	}
}

func TestFollowLink_RefusesALinkToSomethingThatIsNotAFile(t *testing.T) {
	// Writing through a link to a directory is not a save the operator
	// asked for, whatever else is true about where it points.
	dir := t.TempDir()
	target := filepath.Join(dir, "somewhere")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("staging the target: %v", err)
	}

	link := filepath.Join(dir, "config.toml")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("this platform will not create a symlink here: %v", err)
	}

	if got := followLink(link); got != link {
		t.Errorf("followLink() = %q, want the link's own path %q", got, link)
	}
}

// homeTempDir returns a temporary directory inside the operator's home,
// for a case whose behaviour depends on being there.
//
// t.TempDir() answers wherever TMPDIR points, which is inside home on some
// machines and not on others, and a test that changes its verdict with an
// environment variable states nothing.
func homeTempDir(t *testing.T) string {
	t.Helper()

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory on this machine: %v", err)
	}
	dir, err := os.MkdirTemp(home, "stream-dvr-test-")
	if err != nil {
		t.Skipf("cannot create a directory in home: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
