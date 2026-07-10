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
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/cedws/iapc/iap"
)

type tunnel interface {
	io.ReadWriteCloser
	Sent() uint64
	Received() uint64
}

type dialFunc func(ctx context.Context) (tunnel, error)

// Listen serves on the given address, proxying connections through IAP tunnels
// dialed with opts. If proxyDomains is non-empty, incoming connections are
// parsed as HTTP proxy requests (CONNECT or absolute-URI) and only requests
// whose destination matches one of the domains go through the tunnel; all
// other destinations are dialed directly.
func Listen(ctx context.Context, listen string, opts []iap.DialOption, proxyDomains []string) error {
	dial := func(ctx context.Context) (tunnel, error) {
		return iap.Dial(ctx, opts...)
	}
	if err := testDial(ctx, dial); err != nil {
		return fmt.Errorf("error testing connection: %w", err)
	}

	handler := func(ctx context.Context, conn net.Conn) {
		handleClient(ctx, dial, conn)
	}
	if len(proxyDomains) > 0 {
		matcher := newDomainMatcher(proxyDomains)
		dialer := &net.Dialer{Timeout: 30 * time.Second}
		handler = func(ctx context.Context, conn net.Conn) {
			handleClientSelective(ctx, dial, dialer.DialContext, matcher, firstRequestTimeout, conn)
		}
	}
	return listenLoop(ctx, listen, handler)
}

func testDial(ctx context.Context, dial dialFunc) error {
	tun, err := dial(ctx)
	if tun != nil {
		defer tun.Close()
	}
	return err
}

func listenLoop(ctx context.Context, addr string, handler func(context.Context, net.Conn)) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("error starting listener: %w", err)
	}

	// Close listener when context is cancelled
	go func() {
		<-ctx.Done()
		listener.Close()
	}()

	slog.Info("Listening", "addr", listener.Addr())
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				slog.Info("Listener closed, stopping accept loop")
				return nil // listener was closed due to context cancellation
			}
			return fmt.Errorf("error accepting connection: %w", err)
		}
		go handler(ctx, conn)
	}
}

func handleClient(ctx context.Context, dial dialFunc, conn net.Conn) {
	slog.Info("Client connected", "client", conn.RemoteAddr())

	tun, err := dial(ctx)
	if err != nil {
		slog.Error("Error dialing IAP", "err", err)
		conn.Close()
		return
	}

	slog.Info("Dialed IAP", "client", conn.RemoteAddr())

	splice(conn, conn, tun)
	slog.Info("Client disconnected", "client", conn.RemoteAddr(), "sentbytes", tun.Sent(), "recvbytes", tun.Received())
}

// splice bidirectionally copies between the client connection and an upstream
// connection until both directions are done. clientIn is the reader for the
// client→upstream direction; it may wrap conn to include already-buffered
// bytes.
func splice(conn net.Conn, clientIn io.Reader, upstream io.ReadWriteCloser) {
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		n, err := io.Copy(conn, upstream)
		// upstream.Close() from the other goroutine interrupts this copy; that
		// is normal teardown, not an error worth logging. The IAP tunnel's
		// websocket reports the interruption as either net.ErrClosed or a
		// wrapped context.Canceled, depending on an internal race.
		if err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, context.Canceled) {
			slog.Error("copy upstream→client", "client", conn.RemoteAddr(), "bytes", n, "err", err)
		}
		conn.Close()
	}()

	go func() {
		defer wg.Done()
		n, err := io.Copy(upstream, clientIn)
		// conn.Close() from the other goroutine interrupts this copy; that is
		// normal teardown, not an error worth logging. The IAP tunnel's
		// websocket reports the interruption as either net.ErrClosed or a
		// wrapped context.Canceled, depending on an internal race.
		if err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, context.Canceled) {
			slog.Error("copy client→upstream", "client", conn.RemoteAddr(), "bytes", n, "err", err)
		}
		upstream.Close()
	}()

	wg.Wait()
}
