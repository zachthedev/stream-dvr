package generate

import (
	"encoding/json"
	"fmt"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// JSONConfig emits an indented JSON document from any marshalable Go value.
//
// JSON carries no comment syntax, so there is no Docs field and no banner. A
// document that needs documenting gets a schema beside it from [JSONSchema].
type JSONConfig struct {
	// ProjectName mirrors the field the TOML and dotenv generators put in
	// their banners. JSON carries no comment syntax, so Generate never
	// reads it.
	ProjectName string
	// Defaults is the struct or map to serialize.
	Defaults any
	// Indent overrides the default two-space indentation. "\t" indents
	// with tabs.
	Indent string
}

// ///////////////////////////////////////////////
// JSONConfig methods
// ///////////////////////////////////////////////

// Generate returns the JSON bytes, ending in a single trailing newline. It
// takes an [OutputEntry] to match the Generate signature and reads nothing
// from it, since a JSON document has nowhere to carry a banner.
func (c JSONConfig) Generate(_ OutputEntry) ([]byte, error) {
	indent := c.Indent
	if indent == "" {
		indent = "  "
	}
	data, err := json.MarshalIndent(c.Defaults, "", indent)
	if err != nil {
		return nil, fmt.Errorf("generate: marshaling JSON: %w", err)
	}
	return append(data, '\n'), nil
}
