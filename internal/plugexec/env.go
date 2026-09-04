package plugexec

import (
	"os"
	"runtime"
)

// windowsEssentials are the variables the Windows loader and runtime read to
// find the system directory and a temporary directory. A process started
// without them fails in ways that look like plugin bugs, so they are the one
// exception to an otherwise empty environment; none of them can carry a
// credential.
var windowsEssentials = []string{"SYSTEMROOT", "SystemRoot", "WINDIR", "windir", "TEMP", "TMP", "PATHEXT", "COMSPEC", "NUMBER_OF_PROCESSORS"}

// minimalEnv is the environment a plugin process gets. It is empty rather than
// inherited because secrets travel in the request payload: a plugin that could
// read the host's environment would see every other plugin's credentials, and
// "the plugin sees only what its manifest declared" would be a comment rather
// than a property.
func minimalEnv() []string {
	if runtime.GOOS != "windows" {
		// A non-nil empty slice is what exec reads as "no variables"; nil would
		// mean "inherit the host's".
		return []string{}
	}

	env := make([]string, 0, len(windowsEssentials))
	for _, name := range windowsEssentials {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return env
}
