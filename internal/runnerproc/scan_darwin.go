//go:build darwin

package runnerproc

import (
	"bytes"
	"encoding/binary"
	"path/filepath"
	"sort"

	procfind "github.com/soulteary/procfind-kit"
	"golang.org/x/sys/unix"
)

// macOS has no procfs, so argv comes from sysctl. Tests that point procRoot at a fake procfs keep
// using the procfs scanner.
func scanner() processScanner {
	if procRoot != defaultProcRoot {
		return procfsScanner()
	}
	return sysctlScanner{}
}

type sysctlScanner struct{}

func (s sysctlScanner) Find(dir string, spec procfind.Spec) []int {
	return s.FindMany([]string{dir}, spec)[dir]
}

func (s sysctlScanner) Running(dir string, spec procfind.Spec) bool {
	return len(s.Find(dir, spec)) > 0
}

func (s sysctlScanner) FindMany(dirs []string, spec procfind.Spec) map[string][]int {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil
	}
	table := make(map[int][]string, len(procs))
	for i := range procs {
		pid := int(procs[i].Proc.P_pid)
		if pid <= 0 {
			continue
		}
		// Another user's process cannot be read, and is not ours to find.
		if raw, err := unix.SysctlRaw("kern.procargs2", pid); err == nil {
			if argv := parseProcArgs(raw); len(argv) > 0 {
				table[pid] = argv
			}
		}
	}
	return matchProcesses(table, dirs, spec)
}

// parseProcArgs reads a kern.procargs2 buffer: argc, the executable path, NUL padding, then argv
// followed by the environment, which is why argc decides where to stop.
func parseProcArgs(raw []byte) []string {
	if len(raw) < 4 {
		return nil
	}
	argc := int(binary.LittleEndian.Uint32(raw))
	rest := raw[4:]
	end := bytes.IndexByte(rest, 0)
	if end < 0 {
		return nil
	}
	rest = bytes.TrimLeft(rest[end:], "\x00")
	argv := make([]string, 0, argc)
	for len(argv) < argc && len(rest) > 0 {
		end = bytes.IndexByte(rest, 0)
		if end < 0 {
			argv = append(argv, string(rest))
			break
		}
		argv = append(argv, string(rest[:end]))
		rest = rest[end+1:]
	}
	return argv
}

var scriptShells = map[string]bool{"sh": true, "bash": true, "dash": true, "zsh": true, "ksh": true}

// matchProcesses applies the rules procfind applies to procfs: only argv[0], or argv[1] behind a
// shell, can claim a process, so `tail -f run.sh` never counts. Scripts come before executables.
func matchProcesses(table map[int][]string, dirs []string, spec procfind.Spec) map[string][]int {
	out := make(map[string][]int, len(dirs))
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			abs = dir
		}
		var scripts, executables []int
		for pid, argv := range table {
			switch {
			case matchesAny(argv[0], abs, spec.Executables):
				executables = append(executables, pid)
			case matchesAny(argv[0], abs, spec.Scripts),
				len(argv) > 1 && scriptShells[filepath.Base(argv[0])] && matchesAny(argv[1], abs, spec.Scripts):
				scripts = append(scripts, pid)
			}
		}
		if len(scripts)+len(executables) == 0 {
			continue
		}
		sort.Ints(scripts)
		sort.Ints(executables)
		out[dir] = append(scripts, executables...)
	}
	return out
}

func matchesAny(path, dir string, names []string) bool {
	for _, name := range names {
		if path == filepath.Join(dir, name) {
			return true
		}
	}
	return false
}
