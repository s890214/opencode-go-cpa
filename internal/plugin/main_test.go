package plugin

import (
	"fmt"
	"os"
	"testing"
)

// TestMain isolates the package from the developer's real cache directory.
// The plugin persists its catalog snapshot under os.UserCacheDir by default,
// so without this every run would read and write ~/.cache/opencode-go and
// tests would leak state into each other (a warm catalog in one test hides
// "blocked refresh yields an empty routable set" in another).
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "opencode-go-plugin-test-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create temp cache dir: %v\n", err)
		os.Exit(1)
	}
	previous, hadPrevious := os.LookupEnv("XDG_CACHE_HOME")
	if err := os.Setenv("XDG_CACHE_HOME", dir); err != nil {
		fmt.Fprintf(os.Stderr, "set XDG_CACHE_HOME: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	if hadPrevious {
		_ = os.Setenv("XDG_CACHE_HOME", previous)
	} else {
		_ = os.Unsetenv("XDG_CACHE_HOME")
	}
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
