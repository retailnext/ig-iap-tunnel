// Copyright (c) 2026, RetailNext, Inc.
// This material contains trade secrets and confidential information of
// RetailNext, Inc.  Any use, reproduction, disclosure or dissemination
// is strictly prohibited without the explicit written permission
// of RetailNext, Inc.
// All rights reserved.

package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeTunnel wraps a net.Pipe conn to satisfy the tunnel interface.
type fakeTunnel struct {
	net.Conn
}

func (f *fakeTunnel) Sent() uint64     { return 0 }
func (f *fakeTunnel) Received() uint64 { return 0 }

func TestTestDial(t *testing.T) {
	t.Run("success closes tunnel", func(t *testing.T) {
		a, b := net.Pipe()
		defer b.Close()
		dial := func(_ context.Context) (tunnel, error) {
			return &fakeTunnel{a}, nil
		}
		err := testDial(context.Background(), dial)
		assert.NoError(t, err)
	})

	t.Run("error is returned", func(t *testing.T) {
		want := errors.New("connect failed")
		dial := func(_ context.Context) (tunnel, error) {
			return nil, want
		}
		err := testDial(context.Background(), dial)
		assert.ErrorIs(t, err, want)
	})
}

func TestHandleClientProxiesData(t *testing.T) {
	// clientConn is what a local TCP client holds; proxyConn is the accepted side.
	clientConn, proxyConn := net.Pipe()
	// tunLocal is handed to handleClient; tunRemote is the far end.
	tunLocal, tunRemote := net.Pipe()

	dial := func(_ context.Context) (tunnel, error) {
		return &fakeTunnel{tunLocal}, nil
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		handleClient(context.Background(), dial, proxyConn)
	}()

	// Client → tunnel
	_, err := clientConn.Write([]byte("ping"))
	require.NoError(t, err)
	buf := make([]byte, 4)
	_, err = io.ReadFull(tunRemote, buf)
	require.NoError(t, err)
	assert.Equal(t, "ping", string(buf))

	// Tunnel → client
	_, err = tunRemote.Write([]byte("pong"))
	require.NoError(t, err)
	_, err = io.ReadFull(clientConn, buf)
	require.NoError(t, err)
	assert.Equal(t, "pong", string(buf))

	clientConn.Close()
	tunRemote.Close()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handleClient did not return after connections closed")
	}
}

func TestHandleClientDialError(t *testing.T) {
	clientConn, proxyConn := net.Pipe()
	defer clientConn.Close()

	dial := func(_ context.Context) (tunnel, error) {
		return nil, errors.New("dial failed")
	}

	// Should return promptly and close proxyConn without panicking.
	handleClient(context.Background(), dial, proxyConn)

	// proxyConn was closed by handleClient; writes from the client side should fail.
	_, err := clientConn.Write([]byte("x"))
	assert.Error(t, err)
}

func TestListenLoopContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		err := listenLoop(ctx, "127.0.0.1:0", func(_ context.Context) (tunnel, error) {
			return nil, errors.New("unused")
		})
		assert.NoError(t, err)
	}()

	// Give the goroutine time to reach listener.Accept before cancelling.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("listenLoop did not stop after context cancel")
	}
}
