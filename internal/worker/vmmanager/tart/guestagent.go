package tart

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/cirruslabs/orchard/internal/dialer"
)

// guestAgentRelay runs inside the guest and joins its standard streams to 127.0.0.1:$1. It
// prefers nc and falls back to bash's /dev/tcp, so neither has to be installed for the other.
const guestAgentRelay = `p="$1"
if command -v nc >/dev/null 2>&1; then exec nc 127.0.0.1 "$p"; fi
exec bash -c 'exec 3<>"/dev/tcp/127.0.0.1/$1" || exit 1; cat <&3 & cat >&3; wait' bash "$p"`

// guestAgentStderrLimit caps how much of the relay's standard error a connection keeps for its
// error message.
const guestAgentStderrLimit = 4096

// guestAgentExitWait is how long a read that hit EOF waits for "tart exec" to exit, so a relay
// that failed reports why instead of a bare EOF.
const guestAgentExitWait = time.Second

// ErrGuestAgentTransport wraps every error the guest agent transport reports.
var ErrGuestAgentTransport = errors.New("guest agent transport")

// GuestAgentDialer reaches a port of the Tart VM vmName through "tart exec" and the Tart Guest
// Agent, over the VM's control socket, instead of the network: the worker then needs no macOS
// Local Network permission and the guest's services may listen on loopback only. The host in
// the dialed address is ignored; the port is dialed on the guest's 127.0.0.1.
func GuestAgentDialer(vmName string) dialer.DialFunc {
	return dialer.DialFunc(func(ctx context.Context, network, addr string) (net.Conn, error) {
		return dialGuestAgent(ctx, tartCommandName, vmName, network, addr)
	})
}

func dialGuestAgent(ctx context.Context, tartCommand, vmName, network, addr string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("%w: unsupported network %q", ErrGuestAgentTransport, network)
	}

	_, portRaw, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrGuestAgentTransport, err)
	}

	port, err := strconv.ParseUint(portRaw, 10, 16)
	if err != nil || port == 0 {
		return nil, fmt.Errorf("%w: invalid port %q", ErrGuestAgentTransport, portRaw)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	stdinRead, stdinWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}

	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		_ = stdinRead.Close()
		_ = stdinWrite.Close()

		return nil, err
	}

	// The relay outlives ctx's cancellation, which only bounds dialing: Close is what ends it
	//nolint:gosec // the command is fixed and the port was parsed as a number
	cmd := exec.CommandContext(context.WithoutCancel(ctx), tartCommand, "exec", "-i", vmName,
		"/bin/sh", "-c", guestAgentRelay, "sh", strconv.FormatUint(port, 10))

	conn := &guestAgentConn{
		vmName: vmName,
		port:   uint16(port),
		stdin:  stdinWrite,
		stdout: stdoutRead,
		done:   make(chan struct{}),
	}

	cmd.Stdin = stdinRead
	cmd.Stdout = stdoutWrite
	cmd.Stderr = &conn.stderr
	// Don't let a grandchild that kept standard error open hold up reaping "tart exec"
	cmd.WaitDelay = guestAgentExitWait

	startErr := cmd.Start()

	// The child holds its own copies now
	_ = stdinRead.Close()
	_ = stdoutWrite.Close()

	if startErr != nil {
		_ = stdinWrite.Close()
		_ = stdoutRead.Close()

		return nil, fmt.Errorf("%w: failed to run %s exec: %w", ErrGuestAgentTransport, tartCommand, startErr)
	}

	conn.process = cmd.Process

	go func() {
		conn.waitErr = cmd.Wait()
		close(conn.done)
	}()

	return conn, nil
}

// guestAgentConn is a connection to a guest port relayed by "tart exec".
type guestAgentConn struct {
	vmName string
	port   uint16

	stdin   *os.File
	stdout  *os.File
	stderr  limitedBuffer
	process *os.Process

	done    chan struct{}
	waitErr error

	closeOnce sync.Once
}

func (conn *guestAgentConn) Read(b []byte) (int, error) {
	n, err := conn.stdout.Read(b)
	if errors.Is(err, io.EOF) {
		if relayErr := conn.relayError(); relayErr != nil {
			return n, relayErr
		}
	}

	return n, err
}

// relayError describes why "tart exec" failed, once it has exited non-zero.
func (conn *guestAgentConn) relayError() error {
	select {
	case <-conn.done:
	case <-time.After(guestAgentExitWait):
		return nil
	}

	if conn.waitErr == nil {
		return nil
	}

	if stderr := bytes.TrimSpace(conn.stderr.Bytes()); len(stderr) > 0 {
		return fmt.Errorf("%w to %s port %d: %w: %s", ErrGuestAgentTransport, conn.vmName, conn.port, conn.waitErr, stderr)
	}

	return fmt.Errorf("%w to %s port %d: %w", ErrGuestAgentTransport, conn.vmName, conn.port, conn.waitErr)
}

func (conn *guestAgentConn) Write(b []byte) (int, error) {
	return conn.stdin.Write(b)
}

func (conn *guestAgentConn) Close() error {
	conn.closeOnce.Do(func() {
		_ = conn.stdin.Close()
		_ = conn.process.Kill()
		_ = conn.stdout.Close()
	})

	return nil
}

func (conn *guestAgentConn) LocalAddr() net.Addr {
	return guestAgentAddr("worker")
}

func (conn *guestAgentConn) RemoteAddr() net.Addr {
	return guestAgentAddr(net.JoinHostPort(conn.vmName, strconv.Itoa(int(conn.port))))
}

func (conn *guestAgentConn) SetDeadline(t time.Time) error {
	return errors.Join(conn.stdout.SetReadDeadline(t), conn.stdin.SetWriteDeadline(t))
}

func (conn *guestAgentConn) SetReadDeadline(t time.Time) error {
	return conn.stdout.SetReadDeadline(t)
}

func (conn *guestAgentConn) SetWriteDeadline(t time.Time) error {
	return conn.stdin.SetWriteDeadline(t)
}

type guestAgentAddr string

func (addr guestAgentAddr) Network() string { return "guest-agent" }
func (addr guestAgentAddr) String() string  { return string(addr) }

// limitedBuffer keeps the first guestAgentStderrLimit bytes written to it.
type limitedBuffer struct {
	mtx sync.Mutex
	buf bytes.Buffer
}

func (lb *limitedBuffer) Write(p []byte) (int, error) {
	lb.mtx.Lock()
	defer lb.mtx.Unlock()

	if room := guestAgentStderrLimit - lb.buf.Len(); room > 0 {
		lb.buf.Write(p[:min(len(p), room)])
	}

	return len(p), nil
}

func (lb *limitedBuffer) Bytes() []byte {
	lb.mtx.Lock()
	defer lb.mtx.Unlock()

	return bytes.Clone(lb.buf.Bytes())
}
