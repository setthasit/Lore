package plugexec

import (
	"os"
	"runtime"
)

// The Windows loader and runtime read these to find the system and temporary
// directories; a process started without them fails like a buggy plugin.
var windowsEssentials = []string{"SYSTEMROOT", "SystemRoot", "WINDIR", "windir", "TEMP", "TMP", "PATHEXT", "COMSPEC", "NUMBER_OF_PROCESSORS"}

func minimalEnv() []string {
	if runtime.GOOS != "windows" {
		// exec.Cmd reads a non-nil empty slice as "no variables"; nil means inherit the host's.
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
