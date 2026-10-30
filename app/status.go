package app

import (
	"os"
	"strconv"
	"strings"
)

// DaemonPID returns the pid of a running "traffic-manager daemon" process,
// or 0 if none is found. It scans /proc/*/cmdline.
func DaemonPID() int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err != nil || len(data) == 0 {
			continue
		}
		argv := strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
		if isDaemonCmd(argv) {
			return pid
		}
	}
	return 0
}

// isDaemonCmd reports whether argv looks like "traffic-manager daemon ...".
func isDaemonCmd(argv []string) bool {
	if len(argv) < 2 {
		return false
	}
	base := argv[0]
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	// installed path or dev binary; also tolerate "sudo traffic-manager daemon"
	if base != "traffic-manager" {
		return false
	}
	return argv[1] == "daemon"
}
