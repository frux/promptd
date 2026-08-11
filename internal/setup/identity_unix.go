//go:build !windows

package setup

import "os"

func currentIdentity() (uid, gid, euid int) {
	return os.Getuid(), os.Getgid(), os.Geteuid()
}

func hasRootPrivileges() bool {
	return os.Geteuid() == 0
}
