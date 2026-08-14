// Package buildenv declares the settings a build reads from the
// environment.
//
// # Why a package rather than a comment in the Makefile
//
// Because two things have to agree: what the build reads, and what an
// operator is told to set. Written twice, they drift. The copy that goes
// stale is always the documentation, because nothing fails when it does.
// This is the one declaration. cmd/generate renders .env.template from it,
// so the template cannot describe a variable the build does not read.
//
// # What belongs here
//
// A value the build injects into the binary and an operator can choose. Not
// GOOS, GOARCH, or CC, which select a toolchain rather than configure a
// build, and not anything the daemon reads at runtime: that is config.toml.
//
// # What is never here
//
// A value. Every field below is a description, and the generated template
// leaves every assignment commented out. The template is committed and .env
// is not, which works only while the template holds nothing worth keeping.
package buildenv

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// Variable is one build-time setting.
type Variable struct {
	// Name is the environment variable, spelled as the Makefile reads it.
	Name string
	// Purpose says what the build does with the value.
	Purpose string
	// Example is a value of the right shape, shown in a comment. It is a
	// placeholder rather than a real one: this reaches a committed file.
	Example string
	// Absent says what a build without the variable does instead. Every
	// variable here is optional, so this is the field that matters: it is
	// what tells an operator whether to bother setting one.
	Absent string
}

// ///////////////////////////////////////////////
// The schema
// ///////////////////////////////////////////////

// Variables returns every build-time setting, in the order a template
// lists them.
//
// Declared order rather than sorted, so the version and the repository it
// came from stay together and the credential-shaped one is last.
func Variables() []Variable {
	return []Variable{
		{
			Name: "VERSION",
			Purpose: "The version the binary reports. Taken from the nearest git tag " +
				"when unset, which is what a release build wants.",
			Example: "1.2.3",
			Absent:  "the version is derived from git, or reads 0.0.0-dev with the commit when there is no tag",
		},
		{
			Name: "REPO_OWNER",
			Purpose: "The GitHub account this project is published under. It builds the " +
				"links the binary points back at: issues, raw content, update checks.",
			Example: "example-owner",
			Absent:  "read from the git remote, and empty before the repository is published",
		},
		{
			Name:    "REPO_NAME",
			Purpose: "The GitHub repository name, alongside REPO_OWNER.",
			Example: "example-repo",
			Absent:  "read from the git remote, and empty before the repository is published",
		},
	}
}
