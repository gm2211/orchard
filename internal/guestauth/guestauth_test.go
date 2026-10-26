package guestauth_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"testing"

	"github.com/cirruslabs/orchard/internal/guestauth"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

// login runs one SSH handshake against a guest that accepts only guestPassword, and returns the
// passwords it was offered.
func login(t *testing.T, guestPassword, vmPassword string) ([]string, error) {
	t.Helper()

	_, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := ssh.NewSignerFromKey(key)
	require.NoError(t, err)

	var offered []string
	config := &ssh.ServerConfig{
		PasswordCallback: func(_ ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			offered = append(offered, string(password))
			if string(password) == guestPassword {
				return nil, nil
			}
			return nil, errors.New("wrong password")
		},
	}
	config.AddHostKey(signer)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	go func() {
		server, err := listener.Accept()
		if err != nil {
			return
		}
		conn, chans, reqs, err := ssh.NewServerConn(server, config)
		if err != nil {
			return
		}
		go ssh.DiscardRequests(reqs)
		go func() {
			for c := range chans {
				_ = c.Reject(ssh.Prohibited, "")
			}
		}()
		_ = conn.Wait()
	}()

	client, err := net.Dial("tcp", listener.Addr().String())
	require.NoError(t, err)
	defer client.Close()
	conn, _, _, err := ssh.NewClientConn(client, "", &ssh.ClientConfig{
		User:            guestauth.DefaultUser,
		Auth:            []ssh.AuthMethod{guestauth.PasswordAuth(vmPassword)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // in-process test guest
	})
	if conn != nil {
		_ = conn.Close()
	}
	return offered, err
}

func TestPasswordAuth(t *testing.T) {
	// Before the startup script swaps it, the guest still has the image password.
	offered, err := login(t, guestauth.DefaultPassword, "per-vm")
	require.NoError(t, err)
	require.Equal(t, []string{"per-vm", guestauth.DefaultPassword}, offered)

	// After it, the VM's own gets in on the first try.
	offered, err = login(t, "per-vm", "per-vm")
	require.NoError(t, err)
	require.Equal(t, []string{"per-vm"}, offered)

	// The image password is offered once, not twice.
	offered, err = login(t, guestauth.DefaultPassword, guestauth.DefaultPassword)
	require.NoError(t, err)
	require.Equal(t, []string{guestauth.DefaultPassword}, offered)

	_, err = login(t, "something-else", "per-vm")
	require.Error(t, err)
}
