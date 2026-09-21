//go:build !faultinject

package fault

// Enabled is false: this binary has no injector compiled in.
const Enabled = false

// Armed is always false. FERRY_CLI_FAULT is read by [Requested] and ignored
// here, which is the property `fault_release_test.go` asserts: a released
// binary cannot be made to die at a money request by an environment
// variable, and the guarantee is a build-tag one rather than a careful-coding
// one.
func Armed(string) bool { return false }

// Die does nothing.
func Die(string) {}

// Panic does nothing.
func Panic(string) {}
