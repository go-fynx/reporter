package reporter

import "syscall"

// dupTo makes newfd refer to the same file as oldfd. Linux on arm64 has no
// dup2 system call, so this uses dup3.
func dupTo(oldfd, newfd int) error {
	return syscall.Dup3(oldfd, newfd, 0)
}
