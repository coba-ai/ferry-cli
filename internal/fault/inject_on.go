//go:build faultinject

package fault

// Enabled reports whether this binary was built with the injector compiled
// in. A test that needs a fault point skips when it is false rather than
// asserting nothing.
const Enabled = true

// Armed reports whether FERRY_CLI_FAULT names this point.
//
// An unknown name is a loud panic rather than a silent no-op: a test that
// misspells a fault point would otherwise assert the *absence* of the fault
// and pass, which is the failure mode this whole unit is built to refuse.
func Armed(name string) bool {
	requested := Requested()
	if requested == "" {
		return false
	}

	if !Known(requested) {
		panic("fault: " + EnvVar + "=" + requested + " is not a declared fault point; " +
			"the declared points are in fault.Points()")
	}

	return requested == name
}

// Die simulates a process death at this point.
func Die(name string) {
	if Armed(name) {
		panic(Crash{Point: name})
	}
}

// Panic simulates a bug in the CLI's own code at this point.
//
// Deliberately not a [Crash]: a crash models the process being killed and is
// reported by nobody, while this models a panic the CLI must recover from and
// answer for. C17 says the answer is exit 6 once a money request may have
// left, and telling the two apart is what AC69(c) asserts.
func Panic(name string) {
	if Armed(name) {
		panic("fault: injected panic at " + name)
	}
}
