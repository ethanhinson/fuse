// Package version exposes the build version of the fuse binary.
package version

// Version is the current fuse build version. It is a var (not a const) so a
// release build can inject the real version via the linker:
//
//	go build -ldflags "-X github.com/ethanhinson/fuse/internal/version.Version=1.2.3"
//
// The default below is the unstamped development value.
var Version = "0.1.0-dev"
