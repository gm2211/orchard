package base_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cirruslabs/orchard/internal/dialer"
	"github.com/cirruslabs/orchard/internal/worker/vmmanager/base"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

// guestSSH is a minimal guest sshd: it accepts one user with one password, runs the "login
// shell" by reading the script from stdin, and records which passwords it was offered.
type guestSSH struct {
	listener net.Listener
	mu       sync.Mutex
	password string
	offered  []string
	scripts  []string
}

func newGuestSSH(t *testing.T, password string) *guestSSH {
	t.Helper()

	_, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := ssh.NewSignerFromKey(key)
	require.NoError(t, err)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	guest := &guestSSH{listener: listener, password: password}
	config := &ssh.ServerConfig{
		PasswordCallback: func(meta ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			guest.mu.Lock()
			defer guest.mu.Unlock()
			guest.offered = append(guest.offered, string(password))
			if meta.User() == "admin" && string(password) == guest.password {
				return nil, nil
			}
			return nil, fmt.Errorf("wrong password")
		},
	}
	config.AddHostKey(signer)

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go guest.serve(conn, config)
		}
	}()

	return guest
}

func (guest *guestSSH) serve(conn net.Conn, config *ssh.ServerConfig) {
	_, chans, reqs, err := ssh.NewServerConn(conn, config)
	if err != nil {
		_ = conn.Close()
		return
	}
	go ssh.DiscardRequests(reqs)

	for newChannel := range chans {
		channel, requests, err := newChannel.Accept()
		if err != nil {
			continue
		}
		go func() {
			for req := range requests {
				_ = req.Reply(req.Type == "shell", nil)
				if req.Type != "shell" {
					continue
				}
				// Shell writes the script and then "exit", the way it would end a login shell.
				var script []byte
				buf := make([]byte, 4096)
				for !strings.HasSuffix(string(script), "\nexit\n") {
					n, err := channel.Read(buf)
					script = append(script, buf[:n]...)
					if err != nil {
						break
					}
				}
				guest.mu.Lock()
				guest.scripts = append(guest.scripts, string(script))
				guest.mu.Unlock()
				_, _ = channel.Write([]byte("ran\n"))
				_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
				_ = channel.Close()
			}
		}()
	}
}

func (guest *guestSSH) setPassword(password string) {
	guest.mu.Lock()
	defer guest.mu.Unlock()
	guest.password = password
	guest.offered = nil
}

func (guest *guestSSH) offeredPasswords() []string {
	guest.mu.Lock()
	defer guest.mu.Unlock()
	return append([]string(nil), guest.offered...)
}

func (guest *guestSSH) run(t *testing.T, password string) error {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	toGuest := dialer.DialFunc(func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, guest.listener.Addr().String())
	})
	getIP := func(context.Context) (string, error) { return "192.0.2.1", nil }

	var lines []string
	err := (&base.VM{}).Shell(ctx, "admin", password, "true", nil,
		func(line string) { lines = append(lines, line) }, toGuest, getIP)
	if err == nil {
		require.Contains(t, strings.Join(lines, "\n"), "ran")
	}
	return err
}

func TestShellFallsBackToTheImagePasswordUntilTheGuestRotatesIt(t *testing.T) {
	guest := newGuestSSH(t, base.DefaultGuestPassword)

	// First boot: the guest still has the image password.
	require.NoError(t, guest.run(t, "per-vm-secret"))
	require.Equal(t, []string{"per-vm-secret", base.DefaultGuestPassword}, guest.offeredPasswords())

	// After the startup script rotated it, the VM's own password gets in on the first try.
	guest.setPassword("per-vm-secret")
	require.NoError(t, guest.run(t, "per-vm-secret"))
	require.Equal(t, []string{"per-vm-secret"}, guest.offeredPasswords())
}

func TestShellOffersTheImagePasswordOnlyOnce(t *testing.T) {
	guest := newGuestSSH(t, base.DefaultGuestPassword)

	require.NoError(t, guest.run(t, base.DefaultGuestPassword))
	require.Equal(t, []string{base.DefaultGuestPassword}, guest.offeredPasswords())
}

func TestShellFailsWhenNeitherPasswordWorks(t *testing.T) {
	guest := newGuestSSH(t, "something-else")

	require.Error(t, guest.run(t, "per-vm-secret"))
	for _, offered := range guest.offeredPasswords() {
		require.Contains(t, []string{"per-vm-secret", base.DefaultGuestPassword}, offered)
	}
}
