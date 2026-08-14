package config

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// ///////////////////////////////////////////////
// Helpers
// ///////////////////////////////////////////////

// tomlPaths walks a struct's toml tags and returns every dotted path it
// exposes, including the section names themselves.
func tomlPaths(t *testing.T, target any) []string {
	t.Helper()

	var walk func(rt reflect.Type, prefix string, out *[]string)
	walk = func(rt reflect.Type, prefix string, out *[]string) {
		for field := range rt.Fields() {
			tag, _, _ := strings.Cut(field.Tag.Get("toml"), ",")
			if tag == "" || tag == "-" {
				continue
			}

			path := tag
			if prefix != "" {
				path = prefix + "." + tag
			}
			*out = append(*out, path)

			ft := field.Type
			for ft.Kind() == reflect.Pointer || ft.Kind() == reflect.Slice {
				ft = ft.Elem()
			}
			// Descending is what turns a nested struct into dotted
			// keys. A time-package struct renders as one scalar
			// string, so the guard keeps the walk out of it.
			if ft.Kind() == reflect.Struct && ft.PkgPath() != "time" {
				walk(ft, path, out)
			}
		}
	}

	var paths []string
	walk(reflect.TypeOf(target), "", &paths)
	return paths
}

// ///////////////////////////////////////////////
// Docs
// ///////////////////////////////////////////////

func TestDocs_EveryKeyMatchesAField(t *testing.T) {
	// Docs drives the generated config's comments. A renamed field would
	// otherwise leave a doc entry that silently stops being emitted.
	valid := tomlPaths(t, Config{})

	for key := range Docs {
		if !slices.Contains(valid, key) {
			t.Errorf("Docs has key %q, which is not a config field; valid paths are %v", key, valid)
		}
	}
}

func TestDocs_EveryFieldIsDocumented(t *testing.T) {
	// A field with no comment reaches the operator as a bare key with no
	// explanation of what it does.
	for _, path := range tomlPaths(t, Config{}) {
		if _, ok := Docs[path]; !ok {
			t.Errorf("config field %q has no entry in Docs", path)
		}
	}
}

func TestDocs_CommentsAreNotEmpty(t *testing.T) {
	for key, doc := range Docs {
		if strings.TrimSpace(doc.Comment) == "" {
			t.Errorf("Docs[%q] has an empty comment", key)
		}
	}
}

// ///////////////////////////////////////////////
// Moved tables
// ///////////////////////////////////////////////

func TestMovedTables_NameNothingThisBuildDefinesAndSendItSomewhereReal(t *testing.T) {
	// The refusal tells an operator where to put the settings. A destination
	// that is not a field sends them somewhere the decoder drops, which is
	// the same silent default the refusal exists to prevent. A source that
	// is a field would refuse a config this build can read.
	valid := tomlPaths(t, Config{})

	for _, moved := range movedTables {
		if slices.Contains(valid, moved.From) {
			t.Errorf("movedTables refuses %q, which is a config field", moved.From)
		}
		if !slices.Contains(valid, moved.To) {
			t.Errorf("movedTables sends %q to %q, which is not a config field; valid paths are %v",
				moved.From, moved.To, valid)
		}
	}
}

// ///////////////////////////////////////////////
// Platforms
// ///////////////////////////////////////////////

func TestSupportedPlatforms(t *testing.T) {
	for _, want := range []string{PlatformTwitch, PlatformYouTube, PlatformURL} {
		if !slices.Contains(SupportedPlatforms, want) {
			t.Errorf("SupportedPlatforms = %v, want it to include %q", SupportedPlatforms, want)
		}
	}
	if len(SupportedPlatforms) != 3 {
		t.Errorf("SupportedPlatforms = %v, want exactly the three supported platforms", SupportedPlatforms)
	}
}
