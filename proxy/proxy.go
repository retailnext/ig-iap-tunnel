// Copyright (c) 2026, RetailNext, Inc.
// This material contains trade secrets and confidential information of
// RetailNext, Inc.  Any use, reproduction, disclosure or dissemination
// is strictly prohibited without the explicit written permission
// of RetailNext, Inc.
// All rights reserved.

package proxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"

	"github.com/cedws/iapc/iap"
)

type tunnel interface {
	io.ReadWriteCloser
	Sent() uint64
	Received() uint64
}

type dialFunc func(ctx context.Context) (tunnel, error)

func Listen(ctx context.Context, listen string, opts []iap.DialOption) error {
	dial := func(ctx context.Context) (tunnel, error) {
		return iap.Dial(ctx, opts...)
	}
	if err := testDial(ctx, dial); err != nil {
		return fmt.Errorf("error testing connection: %w", err)
	}
	return listenLoop(ctx, listen, dial)
}

func testDial(ctx context.Context, dial dialFunc) error {
	tun, err := dial(ctx)
	if tun != nil {
		defer tun.Close()
	}
	return err
}

func listenLoop(ctx context.Context, addr string, dial dialFunc) error {
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
		go handleClient(ctx, dial, conn)
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

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		n, err := io.Copy(conn, tun)
		if err != nil {
			slog.Error("copy tun→conn", "client", conn.RemoteAddr(), "bytes", n, "err", err)
		}
		conn.Close()
	}()

	go func() {
		defer wg.Done()
		n, err := io.Copy(tun, conn)
		if err != nil {
			slog.Error("copy conn→tun", "client", conn.RemoteAddr(), "bytes", n, "err", err)
		}
		tun.Close()
	}()

	wg.Wait()
	slog.Info("Client disconnected", "client", conn.RemoteAddr(), "sentbytes", tun.Sent(), "recvbytes", tun.Received())
}
