//go:build windows

package setup

func currentIdentity() (uid, gid, euid int) {
	return -1, -1, -1
}

func hasRootPrivileges() bool {
	return false
}
