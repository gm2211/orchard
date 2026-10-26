// Package guestauth holds the SSH login every path into a guest uses: the worker's startup and
// shutdown scripts, the controller's exec, and `orchard ssh vm`.
package guestauth

import "golang.org/x/crypto/ssh"

// DefaultUser and DefaultPassword are the credentials Tart's and Vetu's base images boot with.
const (
	DefaultUser     = "admin"
	DefaultPassword = "admin"
)

// PasswordAuth answers with the VM's own password, then, if the guest refuses it, with the image's
// default one. That lets a VM's startup script replace a well-known image password with the VM's
// own: a login before the swap (the startup script's first connection, an exec that races it)
// still gets in on the image password, and every later one on the new one. Workers that do this
// advertise v1.WorkerCapabilityGuestPasswordRotation.
func PasswordAuth(password string) ssh.AuthMethod {
	passwords := []string{password}
	if password != DefaultPassword {
		passwords = append(passwords, DefaultPassword)
	}
	attempt := 0

	return ssh.RetryableAuthMethod(ssh.PasswordCallback(func() (string, error) {
		next := passwords[min(attempt, len(passwords)-1)]
		attempt++

		return next, nil
	}), len(passwords))
}
