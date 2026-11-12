package reporter

import "syscall"

// dupTo makes newfd refer to the same file as oldfd.
func dupTo(oldfd, newfd int) error {
	return syscall.Dup2(oldfd, newfd)
}
