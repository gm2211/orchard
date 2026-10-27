package tart //nolint:testpackage // exercises the unexported dialer with a fake tart command

import (
	"bufio"
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const testNetwork = "tcp"

// fakeTart stands in for "tart exec -i <vm> <command>...": it runs the command on this host,
// with PATH set to relayPath when that's not empty.
func fakeTart(t *testing.T, relayPath string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "tart")
	script := "#!/bin/sh\n" +
		"[ \"$1 $2\" = \"exec -i\" ] || { echo \"unexpected arguments: $*\" >&2; exit 2; }\n" +
		"shift 3\n"
	if relayPath != "" {
		script += "PATH=" + relayPath + "; export PATH\n"
	}
	script += "exec \"$@\"\n"
	//nolint:gosec // the fake tart has to be executable
	require.NoError(t, os.WriteFile(path, []byte(script), 0o700))

	return path
}

// listenLoopback listens on a free 127.0.0.1 port.
func listenLoopback(t *testing.T) (net.Listener, int) {
	t.Helper()

	var listenConfig net.ListenConfig

	listener, err := listenConfig.Listen(context.Background(), testNetwork, "127.0.0.1:0")
	require.NoError(t, err)

	addr, ok := listener.Addr().(*net.TCPAddr)
	require.True(t, ok)

	return listener, addr.Port
}

// echoServer answers each line on 127.0.0.1 with "echo: <line>".
func echoServer(t *testing.T) int {
	t.Helper()

	listener, port := listenLoopback(t)
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}

			go func() {
				defer conn.Close()

				scanner := bufio.NewScanner(conn)
				for scanner.Scan() {
					if _, err := io.WriteString(conn, "echo: "+scanner.Text()+"\n"); err != nil {
						return
					}
				}
			}()
		}
	}()

	return port
}

// pathWithout links only the named programs into a fresh directory, for a relay with no nc.
func pathWithout(t *testing.T, programs ...string) string {
	t.Helper()

	dir := t.TempDir()
	for _, program := range programs {
		resolved, err := exec.LookPath(program)
		if err != nil {
			t.Skipf("%s not available: %v", program, err)
		}
		require.NoError(t, os.Symlink(resolved, filepath.Join(dir, program)))
	}

	return dir
}

func TestGuestAgentDialerRelaysToGuestLoopback(t *testing.T) {
	cases := map[string]func(t *testing.T) string{
		"nc": func(t *testing.T) string {
			if _, err := exec.LookPath("nc"); err != nil {
				t.Skip("nc not available")
			}

			return ""
		},
		"bash fallback": func(t *testing.T) string {
			return pathWithout(t, "bash", "cat")
		},
	}

	for name, relayPath := range cases {
		t.Run(name, func(t *testing.T) {
			port := echoServer(t)
			tart := fakeTart(t, relayPath(t))

			// The host part is ignored: the relay always dials the guest's loopback
			conn, err := dialGuestAgent(context.Background(), tart, "vm-1", testNetwork,
				net.JoinHostPort("192.168.64.49", strconv.Itoa(port)))
			require.NoError(t, err)
			defer conn.Close()

			require.NoError(t, conn.SetDeadline(time.Now().Add(10*time.Second)))

			reader := bufio.NewReader(conn)
			for _, line := range []string{"hello", "world"} {
				_, err := io.WriteString(conn, line+"\n")
				require.NoError(t, err)

				reply, err := reader.ReadString('\n')
				require.NoError(t, err)
				require.Equal(t, "echo: "+line+"\n", reply)
			}

			require.Equal(t, "vm-1:"+strconv.Itoa(port), conn.RemoteAddr().String())
		})
	}
}

func TestGuestAgentDialerReportsRelayFailure(t *testing.T) {
	// Nothing listens on this port once the listener is closed
	listener, port := listenLoopback(t)
	require.NoError(t, listener.Close())

	tart := fakeTart(t, pathWithout(t, "bash", "cat"))

	conn, err := dialGuestAgent(context.Background(), tart, "vm-1", testNetwork,
		net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	require.NoError(t, err)
	defer conn.Close()

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(10*time.Second)))

	_, err = conn.Read(make([]byte, 1))
	require.ErrorIs(t, err, ErrGuestAgentTransport)
	require.ErrorContains(t, err, "to vm-1 port "+strconv.Itoa(port))
}

func TestGuestAgentDialerRejectsBadAddresses(t *testing.T) {
	for _, tc := range []struct{ network, addr, want string }{
		{"udp", "127.0.0.1:22", "unsupported network"},
		{testNetwork, "127.0.0.1", "missing port"},
		{testNetwork, "127.0.0.1:0", "invalid port"},
		{testNetwork, "127.0.0.1:22;reboot", "invalid port"},
	} {
		_, err := dialGuestAgent(context.Background(), "/nonexistent/tart", "vm-1", tc.network, tc.addr)
		require.ErrorIs(t, err, ErrGuestAgentTransport)
		require.Contains(t, err.Error(), tc.want, "%s %s", tc.network, tc.addr)
	}
}

func TestGuestAgentConnCloseEndsRelay(t *testing.T) {
	port := echoServer(t)
	tart := fakeTart(t, pathWithout(t, "bash", "cat"))

	conn, err := dialGuestAgent(context.Background(), tart, "vm-1", testNetwork,
		net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	require.NoError(t, err)

	relay, ok := conn.(*guestAgentConn)
	require.True(t, ok)

	require.NoError(t, conn.Close())
	require.NoError(t, conn.Close())

	select {
	case <-relay.done:
	case <-time.After(10 * time.Second):
		t.Fatal("tart exec still running after Close")
	}
}
