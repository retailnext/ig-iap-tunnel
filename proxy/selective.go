// Copyright (c) 2026, RetailNext, Inc.
// This material contains trade secrets and confidential information of
// RetailNext, Inc.  Any use, reproduction, disclosure or dissemination
// is strictly prohibited without the explicit written permission
// of RetailNext, Inc.
// All rights reserved.

package proxy

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// firstRequestTimeout bounds how long a client may take to send its first
// proxy request before the connection is dropped.
const firstRequestTimeout = 10 * time.Second

type directDialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// domainMatcher holds lowercased domains. Wildcards, leading dots, and
// trailing dots are rejected at flag parsing; every entry matches itself and
// its subdomains.
type domainMatcher []string

func newDomainMatcher(domains []string) domainMatcher {
	var m domainMatcher
	for _, d := range domains {
		d = strings.ToLower(strings.TrimSpace(d))
		if d != "" {
			m = append(m, d)
		}
	}
	return m
}

// matches reports whether host equals one of the domains or is a subdomain of
// one of them.
func (m domainMatcher) matches(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, d := range m {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// recordingReader captures bytes read through it while `on` is true, so the
// exact raw bytes consumed during request parsing can be replayed upstream.
type recordingReader struct {
	r   io.Reader
	buf bytes.Buffer
	on  bool
}

func (r *recordingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if r.on && n > 0 {
		r.buf.Write(p[:n])
	}
	return n, err
}

// handleClientSelective parses the first HTTP proxy request on conn to decide
// routing. Requests for destinations matching the configured domains ALWAYS
// go through the IAP tunnel (iap.Dial via dialFunc) to the proxy server on
// the remote instance, with the raw bytes replayed so that proxy server sees
// the original request; the proxy server is never contacted directly. Only
// requests for non-matching destinations bypass IAP: those are served locally
// by dialing the destination host itself. Routing is decided once per client
// connection, by its first request.
//
// Non-HTTP traffic cannot be routed in this mode (there is no destination
// domain to inspect) and always fails in one of two ways:
//   - client-speaks-first protocols (e.g. raw SSH): the first bytes fail to
//     parse as an HTTP request and the connection is closed immediately.
//   - server-speaks-first protocols (e.g. SMTP): the client sends nothing
//     while waiting for a server banner, so the read times out after
//     firstReqTimeout and the connection is closed.
//
// Any TCP protocol wrapped in CONNECT by the client still works.
func handleClientSelective(ctx context.Context, dial dialFunc, directDial directDialFunc, matcher domainMatcher, firstReqTimeout time.Duration, conn net.Conn) {
	slog.Info("Client connected", "client", conn.RemoteAddr())

	// Bound the wait for the first request so clients that never send one
	// (e.g. server-speaks-first protocols) don't hold connections open forever.
	if err := conn.SetReadDeadline(time.Now().Add(firstReqTimeout)); err != nil {
		slog.Error("Error setting read deadline", "client", conn.RemoteAddr(), "err", err)
		conn.Close()
		return
	}

	rec := &recordingReader{r: conn, on: true}
	br := bufio.NewReader(rec)
	req, err := http.ReadRequest(br)
	if err != nil {
		// Non-HTTP client-speaks-first traffic fails parsing here; silent
		// clients arrive here via the read deadline.
		slog.Error("Error parsing proxy request", "client", conn.RemoteAddr(), "err", err)
		conn.Close()
		return
	}
	rec.on = false

	// The proxied connection may legitimately idle; remove the deadline.
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		slog.Error("Error clearing read deadline", "client", conn.RemoteAddr(), "err", err)
		conn.Close()
		return
	}

	host := hostOnly(req.Host)
	if matcher.matches(host) {
		handleTunnel(ctx, dial, conn, rec.buf.Bytes(), host)
		return
	}

	slog.Info("Routing directly", "client", conn.RemoteAddr(), "host", host)
	if req.Method == http.MethodConnect {
		serveConnectLocally(ctx, directDial, conn, br, req)
	} else {
		serveHTTPLocally(ctx, directDial, conn, br, req)
	}
	slog.Info("Client disconnected", "client", conn.RemoteAddr())
}

// handleTunnel dials the IAP tunnel, replays the already-consumed raw request
// bytes, then splices the rest of the connection. The remote proxy server
// receives exactly the byte stream the client sent.
func handleTunnel(ctx context.Context, dial dialFunc, conn net.Conn, consumed []byte, host string) {
	slog.Info("Routing via IAP tunnel", "client", conn.RemoteAddr(), "host", host)

	tun, err := dial(ctx)
	if err != nil {
		slog.Error("Error dialing IAP", "client", conn.RemoteAddr(), "err", err)
		conn.Close()
		return
	}
	if _, err := tun.Write(consumed); err != nil {
		slog.Error("Error replaying request to tunnel", "client", conn.RemoteAddr(), "err", err)
		conn.Close()
		tun.Close()
		return
	}

	// consumed already contains everything read from conn (including any
	// bufio read-ahead), so the remaining stream is read from conn directly.
	splice(conn, conn, tun)
	slog.Info("Client disconnected", "client", conn.RemoteAddr(), "sentbytes", tun.Sent(), "recvbytes", tun.Received())
}

// serveConnectLocally handles a CONNECT request for a NON-matching domain:
// it dials the destination host (not the proxy server) from this machine,
// replies 200 to the client, then splices.
func serveConnectLocally(ctx context.Context, directDial directDialFunc, conn net.Conn, br *bufio.Reader, req *http.Request) {
	// A valid host:port never contains CR/LF; strip them so a malicious
	// request line cannot inject log lines.
	target := strings.ReplaceAll(strings.ReplaceAll(req.Host, "\n", ""), "\r", "")
	if _, _, err := net.SplitHostPort(target); err != nil {
		target = net.JoinHostPort(target, "443")
	}

	upstream, err := directDial(ctx, "tcp", target)
	if err != nil {
		slog.Error("Error dialing directly", "client", conn.RemoteAddr(), "target", target, "err", err)
		_, _ = io.WriteString(conn, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		conn.Close()
		return
	}

	if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		slog.Error("Error writing CONNECT response", "client", conn.RemoteAddr(), "err", err)
		conn.Close()
		upstream.Close()
		return
	}

	// br may have buffered bytes past the CONNECT request; read from it, not conn.
	splice(conn, br, upstream)
}

// serveHTTPLocally handles a plain (absolute-URI) HTTP request for a
// NON-matching domain by forwarding it to the origin server in origin-form.
// Connection: close is forced because later requests on a reused proxy
// connection could target a different host, which this pinned upstream
// connection could not route.
func serveHTTPLocally(ctx context.Context, directDial directDialFunc, conn net.Conn, br *bufio.Reader, req *http.Request) {
	target := req.URL.Host
	if target == "" {
		target = req.Host
	}
	// A valid host:port never contains CR/LF; strip them so a malicious
	// request line cannot inject log lines.
	target = strings.ReplaceAll(strings.ReplaceAll(target, "\n", ""), "\r", "")
	if _, _, err := net.SplitHostPort(target); err != nil {
		port := "80"
		if req.URL.Scheme == "https" {
			port = "443"
		}
		target = net.JoinHostPort(target, port)
	}

	upstream, err := directDial(ctx, "tcp", target)
	if err != nil {
		slog.Error("Error dialing directly", "client", conn.RemoteAddr(), "target", target, "err", err)
		_, _ = io.WriteString(conn, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		conn.Close()
		return
	}

	req.Close = true
	req.Header.Del("Proxy-Connection")
	req.Header.Del("Proxy-Authorization")
	if err := req.Write(upstream); err != nil {
		slog.Error("Error forwarding request", "client", conn.RemoteAddr(), "target", target, "err", err)
		conn.Close()
		upstream.Close()
		return
	}

	splice(conn, br, upstream)
}

// hostOnly strips the port from a host:port string if present.
func hostOnly(hostport string) string {
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		return host
	}
	return hostport
}
