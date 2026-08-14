package space

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"zach.tools/go/stream-dvr/internal/config"
)

// ///////////////////////////////////////////////
// Admit
// ///////////////////////////////////////////////

func TestAdmit(t *testing.T) {
	limits := Limits{MaxSize: 2 * config.Terabyte, MinFree: 100 * config.Gigabyte}

	tests := []struct {
		name     string
		usage    Usage
		estimate int64
		wantErr  bool
		wantWhat string
	}{
		{
			name:     "comfortably fits",
			usage:    Usage{LibraryBytes: 500 * config.Gigabyte.Bytes(), FreeBytes: 900 * config.Gigabyte.Bytes()},
			estimate: 20 * config.Gigabyte.Bytes(),
		},
		{
			// A recording sized to consume the last byte of the cap leaves
			// nothing to finish inside, so the watermark stops it on its
			// first check. Refusing once beats a broadcast recorded as
			// fragments.
			name:     "exactly fills the cap",
			usage:    Usage{LibraryBytes: 1990 * config.Gigabyte.Bytes(), FreeBytes: 900 * config.Gigabyte.Bytes()},
			estimate: 10 * config.Gigabyte.Bytes(),
			wantErr:  true,
			wantWhat: "max_size",
		},
		{
			name:     "one byte past the cap",
			usage:    Usage{LibraryBytes: 1990 * config.Gigabyte.Bytes(), FreeBytes: 900 * config.Gigabyte.Bytes()},
			estimate: 10*config.Gigabyte.Bytes() + 1,
			wantErr:  true,
			wantWhat: "max_size",
		},
		{
			name:     "would eat the free-space floor",
			usage:    Usage{LibraryBytes: 100 * config.Gigabyte.Bytes(), FreeBytes: 110 * config.Gigabyte.Bytes()},
			estimate: 20 * config.Gigabyte.Bytes(),
			wantErr:  true,
			wantWhat: "min_free",
		},
		{
			name:     "already below the floor",
			usage:    Usage{LibraryBytes: 100 * config.Gigabyte.Bytes(), FreeBytes: 50 * config.Gigabyte.Bytes()},
			estimate: 1,
			wantErr:  true,
			wantWhat: "min_free",
		},
		{
			name:     "already over the cap",
			usage:    Usage{LibraryBytes: 3 * config.Terabyte.Bytes(), FreeBytes: 900 * config.Gigabyte.Bytes()},
			estimate: 1,
			wantErr:  true,
			wantWhat: "max_size",
		},
		{
			name:     "zero estimate fits while there is headroom",
			usage:    Usage{LibraryBytes: 500 * config.Gigabyte.Bytes(), FreeBytes: 900 * config.Gigabyte.Bytes()},
			estimate: 0,
		},
		{
			// Size is not what makes this one impossible. A library already
			// inside the margin is one the watermark stops whatever the
			// capture costs.
			name:     "zero estimate at the watermark",
			usage:    Usage{LibraryBytes: 1990 * config.Gigabyte.Bytes(), FreeBytes: 101 * config.Gigabyte.Bytes()},
			estimate: 0,
			wantErr:  true,
			wantWhat: "max_size",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Admit(limits, tt.usage, tt.estimate)

			if !tt.wantErr {
				if err != nil {
					t.Errorf("Admit() err = %v, want nil", err)
				}
				return
			}

			var refusal *RefusalError
			if !errors.As(err, &refusal) {
				t.Fatalf("Admit() err = %v, want a *RefusalError", err)
			}
			// The message must name which bound was hit, or the operator
			// cannot tell whether to free space or raise the cap.
			if !strings.Contains(refusal.Error(), tt.wantWhat) {
				t.Errorf("RefusalError = %q, want it to name %q", refusal, tt.wantWhat)
			}
			if refusal.Have < 0 {
				t.Errorf("RefusalError.Have = %d, want it clamped at zero", refusal.Have)
			}
		})
	}
}

func TestAdmit_DisabledLimits(t *testing.T) {
	// Zero disables a limit, which must not read as a limit of zero.
	huge := int64(500) * config.Terabyte.Bytes()

	tests := []struct {
		name   string
		limits Limits
		usage  Usage
	}{
		{
			name:   "no cap",
			limits: Limits{MaxSize: 0, MinFree: 1},
			usage:  Usage{LibraryBytes: huge, FreeBytes: huge},
		},
		{
			name:   "no floor",
			limits: Limits{MaxSize: config.Size(huge), MinFree: 0},
			usage:  Usage{LibraryBytes: 0, FreeBytes: 1},
		},
		{
			name:   "neither",
			limits: Limits{},
			usage:  Usage{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := Admit(tt.limits, tt.usage, 10*config.Gigabyte.Bytes()); err != nil {
				t.Errorf("Admit() err = %v, want nil when the limit is disabled", err)
			}
		})
	}
}

func TestAdmit_RejectsANegativeEstimate(t *testing.T) {
	if err := Admit(Limits{}, Usage{}, -1); err == nil {
		t.Error("Admit() err = nil, want a rejection for a negative estimate")
	}
}

func TestAdmit_RefusesWhatTheWatermarkWouldStop(t *testing.T) {
	// Admitting a recording the watermark stops on its first check spends a
	// whole broadcast as a run of fragments too short to keep, each one
	// leaving a failed row and an orphan file. Admission and the watermark
	// have to agree about how much headroom a capture needs.
	tests := []struct {
		name    string
		limits  Limits
		usage   Usage
		wantErr bool
	}{
		{
			name:    "inside the band the watermark calls critical",
			limits:  Limits{MaxSize: 2 * config.Terabyte},
			usage:   Usage{LibraryBytes: 2*config.Terabyte.Bytes() - 30*config.Gigabyte.Bytes()},
			wantErr: true,
		},
		{
			name:   "past the band, with room to finish in",
			limits: Limits{MaxSize: 2 * config.Terabyte},
			usage:  Usage{LibraryBytes: 2*config.Terabyte.Bytes() - 80*config.Gigabyte.Bytes()},
		},
		{
			name:    "the floor's own critical band",
			limits:  Limits{MinFree: 100 * config.Gigabyte},
			usage:   Usage{FreeBytes: 122*config.Gigabyte.Bytes() + 600*config.Megabyte.Bytes()},
			wantErr: true,
		},
		{
			name:   "past the floor's band",
			limits: Limits{MinFree: 100 * config.Gigabyte},
			usage:  Usage{FreeBytes: 180 * config.Gigabyte.Bytes()},
		},
	}

	// A six-hour broadcast at the default rate, which is what the daemon
	// asks for on every admission.
	estimate := Estimate(DefaultBitrate, 6*time.Hour)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Admit(tt.limits, tt.usage, estimate)

			if tt.wantErr {
				if _, ok := errors.AsType[*RefusalError](err); !ok {
					t.Fatalf("Admit() err = %v, want a refusal: the watermark stops this capture at once", err)
				}
				return
			}
			if err != nil {
				t.Errorf("Admit() err = %v, want nil: there is room to record and finish", err)
			}
		})
	}
}

func TestAdmit_AgreesWithTheWatermark(t *testing.T) {
	// The property behind the table above: nothing Admit lets in may already
	// be at the level that stops a capture. The library necessarily walks
	// this whole range as it fills, so the band is traversed rather than
	// stumbled into.
	limits := Limits{MaxSize: 2 * config.Terabyte}
	estimate := Estimate(DefaultBitrate, 6*time.Hour)

	for held := int64(0); held <= limits.MaxSize.Bytes(); held += 10 * config.Gigabyte.Bytes() {
		usage := Usage{LibraryBytes: held, FreeBytes: 10 * config.Terabyte.Bytes()}
		if Admit(limits, usage, estimate) != nil {
			continue
		}
		if level := Watch(limits, usage); level == LevelCritical {
			t.Fatalf("Admit() admitted a capture with %s held, which Watch() calls %q",
				config.Size(held), level)
		}
	}
}

// ///////////////////////////////////////////////
// Watch
// ///////////////////////////////////////////////

func TestWatch(t *testing.T) {
	limits := Limits{MaxSize: 2 * config.Terabyte, MinFree: 100 * config.Gigabyte}

	tests := []struct {
		name  string
		usage Usage
		want  Level
	}{
		{
			name:  "plenty of room",
			usage: Usage{LibraryBytes: 500 * config.Gigabyte.Bytes(), FreeBytes: 900 * config.Gigabyte.Bytes()},
			want:  LevelOK,
		},
		{
			name:  "approaching the cap",
			usage: Usage{LibraryBytes: 1900 * config.Gigabyte.Bytes(), FreeBytes: 900 * config.Gigabyte.Bytes()},
			want:  LevelLow,
		},
		{
			name:  "about to breach the cap",
			usage: Usage{LibraryBytes: 1990 * config.Gigabyte.Bytes(), FreeBytes: 900 * config.Gigabyte.Bytes()},
			want:  LevelCritical,
		},
		{
			name:  "approaching the floor",
			usage: Usage{LibraryBytes: 100 * config.Gigabyte.Bytes(), FreeBytes: 105 * config.Gigabyte.Bytes()},
			want:  LevelLow,
		},
		{
			name:  "about to breach the floor",
			usage: Usage{LibraryBytes: 100 * config.Gigabyte.Bytes(), FreeBytes: 100 * config.Gigabyte.Bytes()},
			want:  LevelCritical,
		},
		{
			// The more severe of the two limits decides, or a healthy cap
			// would mask a disk about to fill.
			name:  "cap is fine but the volume is not",
			usage: Usage{LibraryBytes: 10 * config.Gigabyte.Bytes(), FreeBytes: 100 * config.Gigabyte.Bytes()},
			want:  LevelCritical,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Watch(limits, tt.usage); got != tt.want {
				t.Errorf("Watch() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWatch_DisabledLimitsNeverEscalate(t *testing.T) {
	if got := Watch(Limits{}, Usage{LibraryBytes: 0, FreeBytes: 0}); got != LevelOK {
		t.Errorf("Watch() with no limits = %q, want %q", got, LevelOK)
	}
}

func TestWatch_SmallCapUsesTheByteFloor(t *testing.T) {
	// A percentage margin on a small cap would leave a window too thin to
	// finalize a recording inside, so the margin has a byte floor.
	limits := Limits{MaxSize: config.Gigabyte}
	usage := Usage{LibraryBytes: 700 * config.Megabyte.Bytes()}

	if got := Watch(limits, usage); got != LevelCritical {
		t.Errorf("Watch() = %q, want %q once headroom is under the byte floor", got, LevelCritical)
	}
}

func TestWatch_SmallCapStillPassesThroughLow(t *testing.T) {
	// Flooring only the critical margin puts it above a percentage warning
	// for every cap under about five gigabytes, so the ladder loses its
	// lower rung and the library steps from ok straight to stopping the
	// recording it is in the middle of.
	tests := []struct {
		name string
		cap  config.Size
	}{
		{name: "one gigabyte", cap: config.Gigabyte},
		{name: "two gigabytes", cap: 2 * config.Gigabyte},
		{name: "five gigabytes", cap: 5 * config.Gigabyte},
		{name: "a hundred gigabytes", cap: 100 * config.Gigabyte},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			limits := Limits{MaxSize: tt.cap}

			var reached []Level
			for used := int64(0); used <= tt.cap.Bytes(); used += tt.cap.Bytes() / 200 {
				level := Watch(limits, Usage{LibraryBytes: used})
				if len(reached) == 0 || reached[len(reached)-1] != level {
					reached = append(reached, level)
				}
			}

			// A cap too small to hold a broadcast is allowed to start at
			// low. What it may not do is skip the rung.
			if !slices.Contains(reached, LevelLow) {
				t.Errorf("Watch() walked %v as the library filled, want it to pass through %q",
					reached, LevelLow)
			}
			if !slices.Contains(reached, LevelCritical) {
				t.Errorf("Watch() walked %v as the library filled, want it to reach %q",
					reached, LevelCritical)
			}
			for i, level := range reached {
				if i > 0 && level == LevelCritical && reached[i-1] == LevelOK {
					t.Errorf("Watch() walked %v, stepping from %q to %q with no warning in between",
						reached, LevelOK, LevelCritical)
				}
			}
		})
	}
}

// ///////////////////////////////////////////////
// Estimation
// ///////////////////////////////////////////////

func TestEstimate(t *testing.T) {
	tests := []struct {
		name     string
		rate     int64
		duration time.Duration
		want     int64
	}{
		{name: "known rate", rate: 800_000, duration: time.Hour, want: 800_000 * 3600},
		{name: "zero duration", rate: 800_000, duration: 0, want: 0},
		{name: "negative duration", rate: 800_000, duration: -time.Hour, want: 0},
		{
			// A channel with no history must still produce a usable figure,
			// or the admission check would always pass.
			name: "unknown rate falls back", rate: 0, duration: time.Hour,
			want: DefaultBitrate * 3600,
		},
		{name: "negative rate falls back", rate: -5, duration: time.Hour, want: DefaultBitrate * 3600},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Estimate(tt.rate, tt.duration); got != tt.want {
				t.Errorf("Estimate(%d, %s) = %d, want %d", tt.rate, tt.duration, got, tt.want)
			}
		})
	}
}

func TestBitrate(t *testing.T) {
	// The values divide evenly so the expected rate is checkable by eye
	// rather than trusted from a calculator.
	tests := []struct {
		name     string
		bytes    int64
		duration time.Duration
		want     int64
	}{
		{name: "one byte per second", bytes: 3600, duration: time.Hour, want: 1},
		{name: "ten bytes per second", bytes: 600, duration: time.Minute, want: 10},
		{name: "a megabyte per second", bytes: 60 * 1_000_000, duration: time.Minute, want: 1_000_000},
		{name: "zero bytes", bytes: 0, duration: time.Hour, want: 0},
		{name: "zero duration", bytes: 1000, duration: 0, want: 0},
		{name: "negative duration", bytes: 1000, duration: -time.Second, want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Bitrate(tt.bytes, tt.duration)
			if got != tt.want {
				t.Errorf("Bitrate(%d, %s) = %d, want %d", tt.bytes, tt.duration, got, tt.want)
			}
		})
	}
}

func TestBitrate_RealRecordingIsPlausible(t *testing.T) {
	// A 4h36m capture of 13.45 GB. The assertion is the order of magnitude
	// a 1080p60 stream produces, not an exact quotient, so it
	// documents the shape without restating the arithmetic under test.
	got := Bitrate(13_450_000_000, 4*time.Hour+36*time.Minute)

	if got < 700_000 || got > 900_000 {
		t.Errorf("Bitrate() = %d bytes/s, want roughly 800 KB/s for a 1080p60 stream", got)
	}
}

func TestEstimate_RoundTripsWithBitrate(t *testing.T) {
	// The daemon derives a rate from a finished recording and uses it to
	// size the next one, so the two must agree.
	duration := 4 * time.Hour
	bytes := int64(11_500_000_000)

	rate := Bitrate(bytes, duration)
	estimate := Estimate(rate, duration)

	drift := estimate - bytes
	if drift < 0 {
		drift = -drift
	}
	// Integer truncation of the rate loses under a second's worth.
	if drift > rate {
		t.Errorf("round trip drifted by %d bytes, want at most one second's worth (%d)", drift, rate)
	}
}

func TestWatch_ASmallCapStillAdmitsRecordings(t *testing.T) {
	// The floors are absolute and headroom under a size cap is bounded by
	// the cap itself, so an uncapped floor larger than the whole budget
	// makes an empty library critical: nothing is ever admitted, the
	// refusal names a figure larger than the budget the operator set, and
	// the trash is released early on a library holding nothing.
	tests := []struct {
		name string
		cap  config.Size
	}{
		{name: "one hundred megabytes", cap: 100 * config.Megabyte},
		{name: "five hundred megabytes", cap: 500 * config.Megabyte},
		{name: "one gigabyte", cap: config.Gigabyte},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			limits := Limits{MaxSize: tt.cap}

			if got := Watch(limits, Usage{}); got == LevelCritical {
				t.Errorf("Watch() on an empty library = %q, want room to record", got)
			}
			if err := Admit(limits, Usage{}, config.Megabyte.Bytes()); err != nil {
				t.Errorf("Admit() on an empty library refused: %v", err)
			}
		})
	}
}
