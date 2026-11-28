package testsupport

import "os"

// IsolateHome points HOME at an empty directory for the lifetime of a test
// binary, so the caller's ~/.config/orca-factory files (profiles.toml,
// driver.toml) never steer a unit test. Call the returned cleanup after tests.
func IsolateHome() func() {
	home, err := os.MkdirTemp("", "driver-home-tests-")
	if err != nil {
		panic(err)
	}
	cleanup := func() { _ = os.RemoveAll(home) }
	if err := os.Setenv("HOME", home); err != nil {
		cleanup()
		panic(err)
	}
	return cleanup
}
