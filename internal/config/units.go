package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// Size is a byte count written in a config file as a human-readable string
// such as "2TB" or "500GiB". It is a plain int64 of bytes so budget
// arithmetic needs no conversion.
type Size int64

// Duration is a time span written in a config file as a string such as
// "30s" or "14d". It extends time.Duration's syntax with day and week
// units, which is how an operator writes a purge window.
type Duration time.Duration

// sizeUnit pairs a suffix with its multiplier.
type sizeUnit struct {
	suffix string
	scale  Size
}

// durationUnit pairs a suffix with its multiplier.
type durationUnit struct {
	suffix string
	scale  Duration
}

// ///////////////////////////////////////////////
// Constants
// ///////////////////////////////////////////////

// Byte multiples. Both families are accepted because operators write both,
// and conflating them misstates a 2 TB drive by a tenth.
const (
	Byte Size = 1

	Kilobyte = Byte * 1000
	Megabyte = Kilobyte * 1000
	Gigabyte = Megabyte * 1000
	Terabyte = Gigabyte * 1000

	Kibibyte = Byte * 1024
	Mebibyte = Kibibyte * 1024
	Gibibyte = Mebibyte * 1024
	Tebibyte = Gibibyte * 1024
)

// Hour, Day and Week are the units a default is written in. Calendar
// arithmetic is not implied: a day here is exactly 24 hours.
const (
	Hour = Duration(time.Hour)
	Day  = Duration(24 * time.Hour)
	Week = Duration(7 * 24 * time.Hour)
)

// maxInt64 bounds a parsed size.
const maxInt64 = int64(^uint64(0) >> 1)

// ///////////////////////////////////////////////
// Variables
// ///////////////////////////////////////////////

// sizeUnits lists every accepted suffix, longest first so "TiB" matches
// before "B" and "TB" before "B".
var sizeUnits = []sizeUnit{
	{"TIB", Tebibyte},
	{"GIB", Gibibyte},
	{"MIB", Mebibyte},
	{"KIB", Kibibyte},
	{"TB", Terabyte},
	{"GB", Gigabyte},
	{"MB", Megabyte},
	{"KB", Kilobyte},
	{"T", Terabyte},
	{"G", Gigabyte},
	{"M", Megabyte},
	{"K", Kilobyte},
	{"B", Byte},
}

// unitPairs drive rendering, largest magnitude first. Each pair holds the
// decimal and binary unit of the same magnitude, so a value is always shown
// in a unit close to its own size.
var unitPairs = []struct {
	decimal sizeUnit
	binary  sizeUnit
}{
	{sizeUnit{"TB", Terabyte}, sizeUnit{"TiB", Tebibyte}},
	{sizeUnit{"GB", Gigabyte}, sizeUnit{"GiB", Gibibyte}},
	{sizeUnit{"MB", Megabyte}, sizeUnit{"MiB", Mebibyte}},
	{sizeUnit{"KB", Kilobyte}, sizeUnit{"KiB", Kibibyte}},
}

// extendedUnits are the suffixes time.ParseDuration does not handle.
var extendedUnits = []durationUnit{{"w", Week}, {"d", Day}}

// ///////////////////////////////////////////////
// Size
// ///////////////////////////////////////////////

// ParseSize converts a human-readable byte count.
//
// A bare number is bytes. Suffixes are case-insensitive. KB is 1000 bytes
// and KiB is 1024, matching the units as they are actually defined rather
// than collapsing them.
func ParseSize(text string) (Size, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return 0, fmt.Errorf("size is empty")
	}

	upper := strings.ToUpper(strings.ReplaceAll(trimmed, " ", ""))
	for _, unit := range sizeUnits {
		if !strings.HasSuffix(upper, unit.suffix) {
			continue
		}
		number := strings.TrimSuffix(upper, unit.suffix)
		if number == "" {
			return 0, fmt.Errorf("size %q has a unit but no number", text)
		}
		return scaleValue("size", text, number, unit.scale)
	}

	return scaleValue("size", text, upper, Byte)
}

// scaleValue multiplies a numeric string by a unit. It refuses negatives,
// values that are not finite, and anything int64 cannot hold.
//
// A whole number goes through integer arithmetic. float64 carries 53 bits of
// mantissa, so a byte count near the top of the range parsed as a float is
// already rounded before it is stored, and the largest of them converts back
// to the most negative int64 there is.
//
// ParseFloat reads "nan" and "inf" as numbers and reports no error. Both
// convert to the most negative int64 there is, so the finite check here is
// what keeps a size of "nan" from becoming a byte count.
func scaleValue[T ~int64](kind, original, number string, scale T) (T, error) {
	if whole, err := strconv.ParseInt(number, 10, 64); err == nil {
		switch {
		case whole < 0:
			return 0, fmt.Errorf("%s %q is negative", kind, original)
		case whole > maxInt64/int64(scale):
			return 0, fmt.Errorf("%s %q overflows", kind, original)
		}
		return T(whole) * scale, nil
	}

	value, err := strconv.ParseFloat(number, 64)
	if err != nil {
		return 0, fmt.Errorf("%s %q is not a number: %w", kind, original, err)
	}
	switch {
	case math.IsNaN(value) || math.IsInf(value, 0):
		return 0, fmt.Errorf("%s %q is not a finite number", kind, original)
	case value < 0:
		return 0, fmt.Errorf("%s %q is negative", kind, original)
	}

	// float64(maxInt64) is exactly one past the largest int64, so anything
	// at or above it converts to an implementation-defined value rather
	// than the number that was written.
	scaled := value * float64(scale)
	if scaled >= float64(maxInt64) {
		return 0, fmt.Errorf("%s %q overflows", kind, original)
	}
	// Zero is what these fields mean by "no cap" and "no floor", so a value
	// the operator wrote as a very small budget must not arrive as one.
	// Truncation would answer their request for less protection with none
	// at all, silently, in the direction that removes the guard.
	if value > 0 && T(scaled) == 0 {
		return 0, fmt.Errorf("%s %q is smaller than this can hold, and zero means no limit", kind, original)
	}
	return T(scaled), nil
}

// Bytes returns the size as a byte count.
func (s Size) Bytes() int64 { return int64(s) }

// String renders the size in a unit close to its own magnitude.
//
// Magnitude is chosen first, exactness second. Choosing by exactness alone
// picks whichever unit happens to divide the number, which for an arbitrary
// byte count is the smallest one: a free-space reading of 712543928320
// divides evenly only by a kibibyte, and "695843680KiB" is exact and
// useless. Within the chosen magnitude a decimal unit wins over a binary
// one, and a value that divides neither evenly gets two decimal places.
func (s Size) String() string {
	if s == 0 {
		return "0B"
	}

	for _, pair := range unitPairs {
		if s < pair.decimal.scale {
			continue
		}
		if s%pair.decimal.scale == 0 {
			return fmt.Sprintf("%d%s", s/pair.decimal.scale, pair.decimal.suffix)
		}
		if s >= pair.binary.scale && s%pair.binary.scale == 0 {
			return fmt.Sprintf("%d%s", s/pair.binary.scale, pair.binary.suffix)
		}
		return trimZeros(strconv.FormatFloat(float64(s)/float64(pair.decimal.scale), 'f', 2, 64)) + pair.decimal.suffix
	}
	return fmt.Sprintf("%dB", int64(s))
}

// exact renders the size without rounding: the largest unit that divides it
// evenly, and otherwise a plain byte count.
func (s Size) exact() string {
	if s == 0 {
		return "0B"
	}

	for _, pair := range unitPairs {
		if s >= pair.decimal.scale && s%pair.decimal.scale == 0 {
			return fmt.Sprintf("%d%s", s/pair.decimal.scale, pair.decimal.suffix)
		}
		if s >= pair.binary.scale && s%pair.binary.scale == 0 {
			return fmt.Sprintf("%d%s", s/pair.binary.scale, pair.binary.suffix)
		}
	}
	return fmt.Sprintf("%dB", int64(s))
}

// trimZeros removes a decimal fraction's trailing zeros, and the point
// itself once nothing follows it.
func trimZeros(text string) string {
	if !strings.Contains(text, ".") {
		return text
	}
	return strings.TrimSuffix(strings.TrimRight(text, "0"), ".")
}

// MarshalText implements encoding.TextMarshaler for TOML and JSON.
//
// It writes the exact byte count where String rounds to two decimals for
// display. Save encodes through here, so a rounded form would let every
// command that writes the config quietly change the cap the operator set:
// 1099511627777 would come back as 1100000000000, and the largest size
// there is would not parse at all.
func (s Size) MarshalText() ([]byte, error) {
	return []byte(s.exact()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler for TOML and JSON.
func (s *Size) UnmarshalText(text []byte) error {
	parsed, err := ParseSize(string(text))
	if err != nil {
		return err
	}
	*s = parsed
	return nil
}

// ///////////////////////////////////////////////
// Duration
// ///////////////////////////////////////////////

// ParseDuration converts a duration string, accepting the units
// time.ParseDuration supports plus "d" for days and "w" for weeks.
func ParseDuration(text string) (Duration, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return 0, fmt.Errorf("duration is empty")
	}

	lower := strings.ToLower(strings.ReplaceAll(trimmed, " ", ""))
	for _, unit := range extendedUnits {
		if !strings.HasSuffix(lower, unit.suffix) {
			continue
		}
		number := strings.TrimSuffix(lower, unit.suffix)
		if number == "" {
			return 0, fmt.Errorf("duration %q has a unit but no number", text)
		}
		return scaleValue("duration", text, number, unit.scale)
	}

	parsed, err := time.ParseDuration(lower)
	if err != nil {
		return 0, fmt.Errorf("duration %q is invalid: %w", text, err)
	}
	if parsed < 0 {
		return 0, fmt.Errorf("duration %q is negative", text)
	}
	return Duration(parsed), nil
}

// Std returns the value as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// String renders whole weeks, then whole days, where they divide evenly. A
// purge window of fourteen days reads as "2w" and not "336h0m0s".
func (d Duration) String() string {
	switch {
	case d == 0:
		return "0s"
	case d%Week == 0:
		return fmt.Sprintf("%dw", d/Week)
	case d%Day == 0:
		return fmt.Sprintf("%dd", d/Day)
	default:
		return time.Duration(d).String()
	}
}

// MarshalText implements encoding.TextMarshaler for TOML and JSON.
func (d Duration) MarshalText() ([]byte, error) {
	return []byte(d.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler for TOML and JSON.
func (d *Duration) UnmarshalText(text []byte) error {
	parsed, err := ParseDuration(string(text))
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}
