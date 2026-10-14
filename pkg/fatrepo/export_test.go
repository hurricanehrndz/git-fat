package fatrepo

import "testing"

// SetTestHook installs h as testHook for the rest of the test.
func SetTestHook(t testing.TB, h func(event, path string)) {
	testHook = h
	t.Cleanup(func() { testHook = nil })
}
