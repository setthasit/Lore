package plugexec

import "os"

// Windows has no SIGTERM; os.Process.Signal accepts only Kill there.
func interrupt(p *os.Process) error {
	return p.Kill()
}
