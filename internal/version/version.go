package version

var (
	Name = "dmdul"
	// Version is the fallback for builds without -ldflags injection; release
	// builds override it with the git tag (see docs/development.md).
	Version   = "v0.7.3"
	Commit    = "unknown"
	BuildTime = "unknown"
)

func String() string {
	if BuildTime != "" && BuildTime != "unknown" {
		return Name + " " + Version + " (" + Commit + ", built " + BuildTime + ")"
	}
	return Name + " " + Version + " (" + Commit + ")"
}
