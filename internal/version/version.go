// Package version exposes build-time version information, populated via
// -ldflags -X at compile time (see Dockerfile). Defaults apply for local builds.
package version

var (
	// Version is the release tag or branch name the binary was built from.
	Version = "dev"
	// Commit is the git commit SHA the binary was built from.
	Commit = ""
)
