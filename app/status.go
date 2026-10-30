package app

import (
	"os"
	osuser "os/user"
	"path/filepath"
	"strconv"
	"strings"
)

// DaemonPID returns the pid of the "traffic-manager daemon" process that is
// writing to dbPath, or 0 if none is found. It scans /proc/*/cmdline and
// matches each daemon's effective database path (its -db flag, or the
// euid-dependent default) against dbPath.
func DaemonPID(dbPath string) int {
	absWant, err := filepath.Abs(dbPath)
	if err != nil {
		absWant = dbPath
	}

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
		if !isDaemonCmd(argv) {
			continue
		}
		if daemonDBPath(pid, argv) == absWant {
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
	if base != "traffic-manager" {
		return false
	}
	return argv[1] == "daemon"
}

// daemonDBPath computes the db path a daemon process writes to:
// its -db flag if present, otherwise the default for its euid.
func daemonDBPath(pid int, argv []string) string {
	for i, a := range argv {
		if a == "-db" && i+1 < len(argv) {
			if abs, err := filepath.Abs(argv[i+1]); err == nil {
				return abs
			}
			return argv[i+1]
		}
		if p, ok := strings.CutPrefix(a, "-db="); ok {
			if abs, err := filepath.Abs(p); err == nil {
				return abs
			}
			return p
		}
	}
	return defaultDBPathForUID(processUID(pid))
}

// processUID reads the real uid of a process from /proc/<pid>/status.
func processUID(pid int) int {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return -1
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "Uid:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				uid, _ := strconv.Atoi(fields[1])
				return uid
			}
		}
	}
	return -1
}

// defaultDBPathForUID mirrors storage.DefaultPath for another uid:
// root -> /var/lib, others -> ~/.local/share (home from /etc/passwd).
func defaultDBPathForUID(uid int) string {
	if uid == 0 {
		return "/var/lib/traffic-manager/traffic.db"
	}
	if u, err := osuser.LookupId(strconv.Itoa(uid)); err == nil && u.HomeDir != "" {
		return filepath.Join(u.HomeDir, ".local", "share", "traffic-manager", "traffic.db")
	}
	return ""
}
