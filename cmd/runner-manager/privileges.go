package main

import (
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"syscall"
)

const (
	appUID     = 1001
	appGID     = 1001
	appHome    = "/app"
	dockerSock = "/var/run/docker.sock"
)

// privilegeOps holds the system calls dropPrivileges makes, so its order can be tested.
type privilegeOps struct {
	geteuid   func() int
	owner     func(path string) (uid, gid int, ok bool)
	lchown    func(path string, uid, gid int) error
	setgroups func(gids []int) error
	setgid    func(gid int) error
	setuid    func(uid int) error
	setenv    func(key, value string) error
}

var systemPrivilegeOps = privilegeOps{
	geteuid:   os.Geteuid,
	owner:     ownerOf,
	lchown:    os.Lchown,
	setgroups: syscall.Setgroups,
	setgid:    syscall.Setgid,
	setuid:    syscall.Setuid,
	setenv:    os.Setenv,
}

// ownerOf returns the uid and gid of a path, or false when they cannot be read.
func ownerOf(path string) (uid, gid int, ok bool) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, 0, false
	}
	st, isStat := info.Sys().(*syscall.Stat_t)
	if !isStat {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}

// chownTree gives a directory to the app user. A directory the app user already owns is left
// alone, so a restart does not walk a large _work tree.
func chownTree(ops privilegeOps, root string) error {
	uid, _, ok := ops.owner(root)
	if !ok || uid == appUID {
		return nil
	}
	return filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return ops.lchown(path, appUID, appGID)
	})
}

// dropPrivileges switches a process started as root to the app user.
//
// Bind-mounted host directories usually arrive owned by root, which a non-root process cannot
// write to, and actions/runner's config.sh refuses to run as root. Starting as root long enough
// to fix ownership and join the socket's group solves both, without knowing the host's docker
// group id. A process that does not start as root is left as it is.
func dropPrivileges(ops privilegeOps, socket string, dirs ...string) error {
	if ops.geteuid() != 0 {
		return nil
	}
	for _, dir := range dirs {
		if err := chownTree(ops, dir); err != nil {
			return fmt.Errorf("change owner of %s: %w", dir, err)
		}
	}
	groups := []int{appGID}
	if _, gid, ok := ops.owner(socket); ok && gid != 0 && gid != appGID {
		groups = append(groups, gid)
	}
	// Order matters: once the uid changes, the groups can no longer be changed.
	if err := ops.setgroups(groups); err != nil {
		return fmt.Errorf("set groups: %w", err)
	}
	if err := ops.setgid(appGID); err != nil {
		return fmt.Errorf("set gid: %w", err)
	}
	if err := ops.setuid(appUID); err != nil {
		return fmt.Errorf("set uid: %w", err)
	}
	if err := ops.setenv("HOME", appHome); err != nil {
		return fmt.Errorf("set HOME: %w", err)
	}
	log.Printf("started as root, now running as uid %d (groups %v)", appUID, groups)
	return nil
}

// managerDirs returns the directories the Manager must be able to write to.
func managerDirs(configPath string) []string {
	base := os.Getenv("RUNNERS_BASE_PATH")
	if base == "" {
		base = "/app/runners"
	}
	return []string{filepath.Dir(configPath), base}
}
