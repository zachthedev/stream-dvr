package config

import (
	"testing"
	"time"
)

// ///////////////////////////////////////////////
// ParseSize
// ///////////////////////////////////////////////

func TestParseSize(t *testing.T) {
	tests := []struct {
		name string
		text string
		want Size
	}{
		{name: "bare number is bytes", text: "1024", want: 1024},
		{name: "zero", text: "0", want: 0},
		{name: "explicit bytes", text: "512B", want: 512},
		{name: "decimal kilobyte is 1000", text: "1KB", want: 1000},
		{name: "binary kibibyte is 1024", text: "1KiB", want: 1024},
		{name: "megabyte", text: "5MB", want: 5 * Megabyte},
		{name: "mebibyte", text: "5MiB", want: 5 * Mebibyte},
		{name: "gigabyte", text: "100GB", want: 100 * Gigabyte},
		{name: "gibibyte", text: "100GiB", want: 100 * Gibibyte},
		{name: "terabyte", text: "2TB", want: 2 * Terabyte},
		{name: "tebibyte", text: "2TiB", want: 2 * Tebibyte},
		{name: "lower case suffix", text: "2tb", want: 2 * Terabyte},
		{name: "mixed case binary suffix", text: "2tIb", want: 2 * Tebibyte},
		{name: "space before the unit", text: "2 TB", want: 2 * Terabyte},
		{name: "surrounding whitespace", text: "  2TB  ", want: 2 * Terabyte},
		{name: "fractional value", text: "1.5GB", want: Size(1.5 * float64(Gigabyte))},
		{name: "short unit", text: "4G", want: 4 * Gigabyte},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseSize(tt.text)
			if err != nil {
				t.Fatalf("ParseSize(%q) err = %v, want nil", tt.text, err)
			}
			if got != tt.want {
				t.Errorf("ParseSize(%q) = %d, want %d", tt.text, got, tt.want)
			}
		})
	}
}

func TestParseSize_Rejects(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{name: "empty", text: ""},
		{name: "whitespace only", text: "   "},
		{name: "unit with no number", text: "GB"},
		{name: "not a number", text: "manyTB"},
		{name: "negative", text: "-5GB"},
		{name: "negative bare", text: "-1"},
		{name: "unknown unit", text: "5PB"},
		// ParseFloat accepts these and reports no error, and the conversion
		// that follows turns them into the most negative int64 there is.
		{name: "nan", text: "nan"},
		{name: "nan upper case", text: "NaN"},
		{name: "nan with a unit", text: "nanKB"},
		{name: "nan with an upper case unit", text: "NANTB"},
		{name: "infinity", text: "inf"},
		{name: "infinity with a unit", text: "infGB"},
		{name: "one past the largest size", text: "9223372036854775808"},
		{name: "far past the largest size", text: "10000000TB"},
		{name: "an exponent past the largest size", text: "1e30"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := ParseSize(tt.text); err == nil {
				t.Errorf("ParseSize(%q) = %d, want an error", tt.text, got)
			}
		})
	}
}

func TestParseSize_HoldsTheLargestSizeExactly(t *testing.T) {
	// float64 carries 53 bits of mantissa, so a byte count near the top of
	// the range parsed as a float is rounded before it is stored.
	tests := []struct {
		text string
		want Size
	}{
		{text: "9223372036854775807", want: Size(maxInt64)},
		{text: "9223372036854775806", want: Size(maxInt64 - 1)},
		{text: "9007199254740993", want: 9007199254740993},
	}

	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			got, err := ParseSize(tt.text)
			if err != nil {
				t.Fatalf("ParseSize(%q) err = %v, want nil", tt.text, err)
			}
			if got != tt.want {
				t.Errorf("ParseSize(%q) = %d, want %d", tt.text, int64(got), int64(tt.want))
			}
		})
	}
}

// ///////////////////////////////////////////////
// Size.String
// ///////////////////////////////////////////////

func TestSize_String(t *testing.T) {
	tests := []struct {
		name string
		size Size
		want string
	}{
		{name: "zero", size: 0, want: "0B"},
		{name: "raw bytes", size: 512, want: "512B"},
		{
			// 2TB divides evenly by a kibibyte too, and reporting it as
			// "1953125000KiB" is exact but unreadable.
			name: "round decimal value uses a decimal unit",
			size: 2 * Terabyte,
			want: "2TB",
		},
		{name: "hundred gigabytes", size: 100 * Gigabyte, want: "100GB"},
		{name: "binary value uses a binary unit", size: 4 * Gibibyte, want: "4GiB"},
		{name: "mebibyte", size: 512 * Mebibyte, want: "512MiB"},
		{name: "kibibyte", size: 1024, want: "1KiB"},
		{name: "exact multiple keeps a whole number", size: 500 * Megabyte, want: "500MB"},
		{name: "inexact value gets decimals with zeros trimmed", size: 1500, want: "1.5KB"},
		{name: "inexact value rounds to two places", size: 1536, want: "1.54KB"},
		{
			// An arbitrary free-space reading divides evenly only by a
			// kibibyte, and reporting it that way is exact and useless.
			name: "arbitrary byte count uses its own magnitude",
			size: 712_543_928_320,
			want: "712.54GB",
		},
		{name: "value just over a gigabyte", size: 1050 * Megabyte, want: "1.05GB"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.size.String(); got != tt.want {
				t.Errorf("Size(%d).String() = %q, want %q", int64(tt.size), got, tt.want)
			}
		})
	}
}

func TestSize_RoundTrip(t *testing.T) {
	sizes := []Size{
		0, 1, 512, 1000, 1024,
		5 * Megabyte, 512 * Mebibyte,
		100 * Gigabyte, 4 * Gibibyte,
		2 * Terabyte, 1 * Tebibyte,
	}

	for _, want := range sizes {
		t.Run(want.String(), func(t *testing.T) {
			got, err := ParseSize(want.String())
			if err != nil {
				t.Fatalf("ParseSize(%q) err = %v, want nil", want.String(), err)
			}
			if got != want {
				t.Errorf("round trip of %d rendered %q and parsed back to %d", int64(want), want.String(), int64(got))
			}
		})
	}
}

func TestSize_TextMarshaling(t *testing.T) {
	var got Size
	if err := got.UnmarshalText([]byte("2TB")); err != nil {
		t.Fatalf("UnmarshalText() err = %v, want nil", err)
	}
	if got != 2*Terabyte {
		t.Errorf("UnmarshalText(2TB) = %d, want %d", int64(got), int64(2*Terabyte))
	}
	if got.Bytes() != 2_000_000_000_000 {
		t.Errorf("Bytes() = %d, want 2000000000000", got.Bytes())
	}

	text, err := got.MarshalText()
	if err != nil {
		t.Fatalf("MarshalText() err = %v, want nil", err)
	}
	if string(text) != "2TB" {
		t.Errorf("MarshalText() = %q, want %q", text, "2TB")
	}
}

func TestSize_UnmarshalText_Rejects(t *testing.T) {
	var got Size
	if err := got.UnmarshalText([]byte("nonsense")); err == nil {
		t.Error("UnmarshalText(nonsense) err = nil, want an error")
	}
}

func TestSize_MarshalText_IsExactWhereStringIsPretty(t *testing.T) {
	// Save encodes through MarshalText, so a rounded form there rewrites
	// the operator's setting every time the config is written back.
	// Durations round-trip exactly, which is what makes the asymmetry
	// invisible until a cap drifts.
	tests := []struct {
		name string
		size Size
		want string
	}{
		{name: "a round decimal cap", size: 500 * Gigabyte, want: "500GB"},
		{name: "a round binary cap", size: 512 * Gibibyte, want: "512GiB"},
		{name: "nothing", size: 0, want: "0B"},
		{name: "just under a kilobyte", size: 1023, want: "1023B"},
		{name: "a terabyte and a byte", size: Terabyte + 1, want: "1000000000001B"},
		{name: "the largest size there is", size: Size(maxInt64), want: "9223372036854775807B"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, err := tt.size.MarshalText()
			if err != nil {
				t.Fatalf("MarshalText() err = %v, want nil", err)
			}
			if string(text) != tt.want {
				t.Errorf("MarshalText() = %q, want %q", text, tt.want)
			}

			back, err := ParseSize(string(text))
			if err != nil {
				t.Fatalf("ParseSize(%q) err = %v, want nil", text, err)
			}
			if back != tt.size {
				t.Errorf("round trip of %d came back as %d", int64(tt.size), int64(back))
			}
		})
	}
}

// ///////////////////////////////////////////////
// ParseDuration
// ///////////////////////////////////////////////

func TestParseDuration(t *testing.T) {
	tests := []struct {
		name string
		text string
		want Duration
	}{
		{name: "seconds", text: "30s", want: Duration(30 * time.Second)},
		{name: "minutes", text: "2m", want: Duration(2 * time.Minute)},
		{name: "hours", text: "3h", want: Duration(3 * time.Hour)},
		{name: "compound", text: "1h30m", want: Duration(90 * time.Minute)},
		{name: "days", text: "14d", want: 14 * Day},
		{name: "weeks", text: "2w", want: 2 * Week},
		{name: "fractional day", text: "0.5d", want: Duration(12 * time.Hour)},
		{name: "upper case", text: "14D", want: 14 * Day},
		{name: "surrounding whitespace", text: "  7d  ", want: 7 * Day},
		{name: "zero", text: "0s", want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseDuration(tt.text)
			if err != nil {
				t.Fatalf("ParseDuration(%q) err = %v, want nil", tt.text, err)
			}
			if got != tt.want {
				t.Errorf("ParseDuration(%q) = %v, want %v", tt.text, got.Std(), tt.want.Std())
			}
		})
	}
}

func TestParseDuration_Rejects(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{name: "empty", text: ""},
		{name: "whitespace only", text: "  "},
		{name: "no unit", text: "30"},
		{name: "not a number", text: "manyd"},
		{name: "negative days", text: "-5d"},
		{name: "negative duration", text: "-30s"},
		{name: "unknown unit", text: "5y"},
		{name: "day unit with no number", text: "d"},
		{name: "week unit with no number", text: "w"},
		// Each of these parses as a float and scales to the most negative
		// int64 there is. Nothing downstream reads that as anything but a
		// duration, so ParseDuration is where it has to be refused.
		{name: "nan days", text: "nand"},
		{name: "nan weeks", text: "nanw"},
		{name: "infinite days", text: "infd"},
		{name: "weeks past the range", text: "1000000w"},
		{name: "an exponent past the range", text: "1e30d"},
		{name: "days past the range", text: "100000000000d"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := ParseDuration(tt.text); err == nil {
				t.Errorf("ParseDuration(%q) = %v, want an error", tt.text, got.Std())
			}
		})
	}
}

// ///////////////////////////////////////////////
// Duration.String
// ///////////////////////////////////////////////

func TestDuration_String(t *testing.T) {
	tests := []struct {
		name string
		dur  Duration
		want string
	}{
		{name: "zero", dur: 0, want: "0s"},
		{name: "weeks preferred over days", dur: 14 * Day, want: "2w"},
		{name: "days", dur: 3 * Day, want: "3d"},
		{name: "sub-day falls back to the standard form", dur: Duration(90 * time.Minute), want: "1h30m0s"},
		{name: "seconds", dur: Duration(30 * time.Second), want: "30s"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.dur.String(); got != tt.want {
				t.Errorf("Duration(%v).String() = %q, want %q", tt.dur.Std(), got, tt.want)
			}
		})
	}
}

func TestDuration_RoundTrip(t *testing.T) {
	durations := []Duration{
		0,
		Duration(30 * time.Second),
		Duration(90 * time.Minute),
		3 * Day,
		2 * Week,
	}

	for _, want := range durations {
		t.Run(want.String(), func(t *testing.T) {
			got, err := ParseDuration(want.String())
			if err != nil {
				t.Fatalf("ParseDuration(%q) err = %v, want nil", want.String(), err)
			}
			if got != want {
				t.Errorf("round trip of %v rendered %q and parsed back to %v", want.Std(), want.String(), got.Std())
			}
		})
	}
}

func TestDuration_TextMarshaling(t *testing.T) {
	var got Duration
	if err := got.UnmarshalText([]byte("14d")); err != nil {
		t.Fatalf("UnmarshalText() err = %v, want nil", err)
	}
	if got != 14*Day {
		t.Errorf("UnmarshalText(14d) = %v, want %v", got.Std(), (14 * Day).Std())
	}

	text, err := got.MarshalText()
	if err != nil {
		t.Fatalf("MarshalText() err = %v, want nil", err)
	}
	if string(text) != "2w" {
		t.Errorf("MarshalText() = %q, want %q", text, "2w")
	}
}

func TestDuration_UnmarshalText_Rejects(t *testing.T) {
	var got Duration
	if err := got.UnmarshalText([]byte("nonsense")); err == nil {
		t.Error("UnmarshalText(nonsense) err = nil, want an error")
	}
}
