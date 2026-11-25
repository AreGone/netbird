//go:build !windows

package client

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// syncBuffer collects the session output while the test polls it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestSSHClient_PTYSessionForwardsWindowResize(t *testing.T) {
	server, _, client := setupTestSSHServerAndClient(t)
	t.Cleanup(func() {
		assert.NoError(t, client.Close())
		assert.NoError(t, server.Stop())
	})

	// The client reads the window size from stdin, so stand in a local PTY for the
	// user's terminal: the slave end is the client's stdin, the master end is where
	// the test types.
	master, slave, err := pty.Open()
	require.NoError(t, err, "Local PTY must open")
	require.NoError(t, pty.Setsize(master, &pty.Winsize{Rows: 24, Cols: 80}))

	stdoutReader, stdoutWriter, err := os.Pipe()
	require.NoError(t, err)

	origStdin, origStdout := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = slave, stdoutWriter

	output := &syncBuffer{}
	copyDone := make(chan struct{})
	go func() {
		defer close(copyDone)
		_, _ = io.Copy(output, stdoutReader)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	terminalDone := make(chan struct{})
	go func() {
		defer close(terminalDone)
		// A command rather than a login shell: login needs a full host setup that
		// root in a CI container does not have, and both paths share the terminal setup.
		if err := client.ExecuteCommandWithPTY(ctx, "sh"); err != nil {
			t.Logf("PTY session ended: %v", err)
		}
	}()

	t.Cleanup(func() {
		_, _ = master.WriteString("exit\n")
		select {
		case <-terminalDone:
		case <-time.After(5 * time.Second):
			cancel()
			<-terminalDone
		}
		cancel()
		os.Stdin, os.Stdout = origStdin, origStdout
		_ = stdoutWriter.Close()
		<-copyDone
		_ = stdoutReader.Close()
		_ = slave.Close()
		_ = master.Close()
		if t.Failed() {
			t.Logf("session output: %q", output.String())
		}
	})

	remoteSizeIs := func(size string) func() bool {
		return func() bool {
			if _, err := master.WriteString("stty size\n"); err != nil {
				return false
			}
			time.Sleep(100 * time.Millisecond)
			return strings.Contains(output.String(), size)
		}
	}

	require.Eventually(t, remoteSizeIs("24 80"), 10*time.Second, 200*time.Millisecond,
		"Remote PTY should start with the local terminal size")

	// Resizing a terminal window changes the PTY size and signals the process.
	// The kernel only signals the foreground process group of the terminal, which
	// the test binary is not part of, so deliver the signal directly.
	require.NoError(t, pty.Setsize(master, &pty.Winsize{Rows: 43, Cols: 132}))
	require.NoError(t, syscall.Kill(syscall.Getpid(), syscall.SIGWINCH))

	assert.Eventually(t, remoteSizeIs("43 132"), 10*time.Second, 200*time.Millisecond,
		"Remote PTY should follow the local terminal resize")
}
