package plugexec

import "os"

// interrupt terminates the plugin. Windows has no SIGTERM — os.Process.Signal
// accepts only Kill there — so the escalation collapses to its last step: stdin
// was already closed, which is the cancel a cooperative plugin acts on, and a
// plugin that ignored it is killed rather than asked twice.
func interrupt(p *os.Process) error {
	return p.Kill()
}
