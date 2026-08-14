package generate

// Default is the package-level registry cmd/generate runs. Its init
// function is where every [OutputEntry] is registered.
//
// Example:
//
//	func init() {
//	    generate.Default.Register(generate.OutputEntry{
//	        Path:     "config.default.toml",
//	        Inputs:   []string{"internal/config/*.go"},
//	        Template: true,
//	        Generate: generate.TOMLConfig{
//	            ProjectName: "stream-dvr",
//	            Defaults:    config.DefaultConfig(),
//	            Docs:        config.Docs,
//	        }.Generate,
//	    })
//	}
var Default = &Registry{}
