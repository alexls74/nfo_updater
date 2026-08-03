package version

// Version и BuildDate подставляются при сборке через -ldflags.
var (
	Version   = "dev"
	BuildDate = "unknown"
)
