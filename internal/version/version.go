// Package version holds the build version, set with -ldflags -X.
package version

var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// String is the one-line form used by --version.
func String() string {
	return Version + " (" + Commit + ", " + Date + ")"
}
