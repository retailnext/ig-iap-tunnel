// Copyright (c) 2026, RetailNext, Inc.
// This material contains trade secrets and confidential information of
// RetailNext, Inc.  Any use, reproduction, disclosure or dissemination
// is strictly prohibited without the explicit written permission
// of RetailNext, Inc.
// All rights reserved.

package proxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDomainMatcher(t *testing.T) {
	m := newDomainMatcher([]string{"Example.com", " internal.net ", "corp.io", "", "10.1.2.3"})

	assert.True(t, m.matches("example.com"))
	assert.True(t, m.matches("EXAMPLE.COM"))
	assert.True(t, m.matches("sub.example.com"))
	assert.True(t, m.matches("deep.sub.example.com"))
	assert.True(t, m.matches("example.com."))
	assert.True(t, m.matches("internal.net"))
	assert.True(t, m.matches("host.internal.net"))
	assert.True(t, m.matches("corp.io"))
	assert.True(t, m.matches("db.corp.io"))
	assert.True(t, m.matches("10.1.2.3"))

	assert.False(t, m.matches("example.org"))
	assert.False(t, m.matches("notexample.com"))
	assert.False(t, m.matches("example.com.evil.net"))
	assert.False(t, m.matches("10.1.2.30"))
}

// startSelective runs handleClientSelective in a goroutine and returns the
// client side of the connection plus a done channel.
func startSelective(t *testing.T, dial dialFunc, directDial directDialFunc, domains []string) (net.Conn, chan struct{}) {
	t.Helper()
	clientConn, proxyConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleClientSelective(context.Background(), dial, directDial, newDomainMatcher(domains), time.Second, proxyConn)
	}()
	return clientConn, done
}

func waitDone(t *testing.T, done chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handleClientSelective did not return")
	}
}

func TestSelectiveConnectViaTunnel(t *testing.T) {
	tunLocal, tunRemote := net.Pipe()
	dial := func(_ context.Context) (tunnel, error) {
		return &fakeTunnel{tunLocal}, nil
	}
	directDial := func(_ context.Context, _, addr string) (net.Conn, error) {
		t.Errorf("unexpected direct dial to %s", addr)
		return nil, errors.New("unexpected")
	}

	clientConn, done := startSelective(t, dial, directDial, []string{"example.com"})

	connectReq := "CONNECT sub.example.com:443 HTTP/1.1\r\nHost: sub.example.com:443\r\n\r\n"
	go func() {
		_, _ = clientConn.Write([]byte(connectReq + "hello"))
	}()

	// The tunnel must receive the exact raw bytes the client sent.
	want := connectReq + "hello"
	buf := make([]byte, len(want))
	_, err := io.ReadFull(tunRemote, buf)
	require.NoError(t, err)
	assert.Equal(t, want, string(buf))

	// And bytes from the tunnel flow back to the client.
	go func() {
		_, _ = tunRemote.Write([]byte("world"))
	}()
	buf = make([]byte, 5)
	_, err = io.ReadFull(clientConn, buf)
	require.NoError(t, err)
	assert.Equal(t, "world", string(buf))

	clientConn.Close()
	tunRemote.Close()
	waitDone(t, done)
}

func TestSelectiveConnectDirect(t *testing.T) {
	dial := func(_ context.Context) (tunnel, error) {
		t.Error("unexpected tunnel dial")
		return nil, errors.New("unexpected")
	}
	upstreamLocal, upstreamRemote := net.Pipe()
	var dialedAddr string
	directDial := func(_ context.Context, _, addr string) (net.Conn, error) {
		dialedAddr = addr
		return upstreamLocal, nil
	}

	clientConn, done := startSelective(t, dial, directDial, []string{"example.com"})

	go func() {
		_, _ = clientConn.Write([]byte("CONNECT other.net:8443 HTTP/1.1\r\nHost: other.net:8443\r\n\r\n"))
	}()

	// Client should get a synthesized 200 before any upstream traffic.
	br := bufio.NewReader(clientConn)
	line, err := br.ReadString('\n')
	require.NoError(t, err)
	assert.Equal(t, "HTTP/1.1 200 Connection Established\r\n", line)
	line, err = br.ReadString('\n')
	require.NoError(t, err)
	assert.Equal(t, "\r\n", line)
	assert.Equal(t, "other.net:8443", dialedAddr)

	// Post-CONNECT bytes flow client → upstream and back.
	go func() {
		_, _ = clientConn.Write([]byte("ping"))
	}()
	buf := make([]byte, 4)
	_, err = io.ReadFull(upstreamRemote, buf)
	require.NoError(t, err)
	assert.Equal(t, "ping", string(buf))

	go func() {
		_, _ = upstreamRemote.Write([]byte("pong"))
	}()
	_, err = io.ReadFull(br, buf)
	require.NoError(t, err)
	assert.Equal(t, "pong", string(buf))

	clientConn.Close()
	upstreamRemote.Close()
	waitDone(t, done)
}

func TestSelectiveConnectDirectDefaultPort(t *testing.T) {
	dial := func(_ context.Context) (tunnel, error) {
		return nil, errors.New("unexpected")
	}
	var dialedAddr string
	directDial := func(_ context.Context, _, addr string) (net.Conn, error) {
		dialedAddr = addr
		return nil, errors.New("refused")
	}

	clientConn, done := startSelective(t, dial, directDial, []string{"example.com"})

	go func() {
		_, _ = clientConn.Write([]byte("CONNECT other.net HTTP/1.1\r\nHost: other.net\r\n\r\n"))
	}()

	// Dial fails, so the client should see a 502.
	resp, err := io.ReadAll(clientConn)
	require.NoError(t, err)
	assert.Contains(t, string(resp), "502 Bad Gateway")
	assert.Equal(t, "other.net:443", dialedAddr)
	waitDone(t, done)
}

func TestSelectiveHTTPDirect(t *testing.T) {
	dial := func(_ context.Context) (tunnel, error) {
		t.Error("unexpected tunnel dial")
		return nil, errors.New("unexpected")
	}
	upstreamLocal, upstreamRemote := net.Pipe()
	var dialedAddr string
	directDial := func(_ context.Context, _, addr string) (net.Conn, error) {
		dialedAddr = addr
		return upstreamLocal, nil
	}

	clientConn, done := startSelective(t, dial, directDial, []string{"example.com"})

	go func() {
		_, _ = clientConn.Write([]byte("GET http://other.net/path?q=1 HTTP/1.1\r\nHost: other.net\r\nProxy-Connection: keep-alive\r\nUser-Agent: test\r\n\r\n"))
	}()

	// The origin server must receive an origin-form request with
	// Connection: close and without proxy headers.
	req, err := http.ReadRequest(bufio.NewReader(upstreamRemote))
	require.NoError(t, err)
	assert.Equal(t, "other.net:80", dialedAddr)
	assert.Equal(t, "GET", req.Method)
	assert.Equal(t, "/path?q=1", req.URL.RequestURI())
	assert.Equal(t, "other.net", req.Host)
	assert.Equal(t, "close", req.Header.Get("Connection"))
	assert.Equal(t, "test", req.Header.Get("User-Agent"))
	assert.Empty(t, req.Header.Get("Proxy-Connection"))

	// Response flows back to the client, then upstream closes.
	response := "HTTP/1.1 200 OK\r\nConnection: close\r\nContent-Length: 2\r\n\r\nok"
	go func() {
		_, _ = upstreamRemote.Write([]byte(response))
		upstreamRemote.Close()
	}()
	got, err := io.ReadAll(clientConn)
	require.NoError(t, err)
	assert.Equal(t, response, string(got))
	waitDone(t, done)
}

func TestSelectiveHTTPViaTunnel(t *testing.T) {
	tunLocal, tunRemote := net.Pipe()
	dial := func(_ context.Context) (tunnel, error) {
		return &fakeTunnel{tunLocal}, nil
	}
	directDial := func(_ context.Context, _, addr string) (net.Conn, error) {
		t.Errorf("unexpected direct dial to %s", addr)
		return nil, errors.New("unexpected")
	}

	clientConn, done := startSelective(t, dial, directDial, []string{"example.com"})

	// Plain HTTP to a matching domain: raw absolute-URI bytes are replayed to
	// the remote proxy untouched.
	rawReq := "GET http://api.example.com/v1 HTTP/1.1\r\nHost: api.example.com\r\n\r\n"
	go func() {
		_, _ = clientConn.Write([]byte(rawReq))
	}()

	buf := make([]byte, len(rawReq))
	_, err := io.ReadFull(tunRemote, buf)
	require.NoError(t, err)
	assert.Equal(t, rawReq, string(buf))

	clientConn.Close()
	tunRemote.Close()
	waitDone(t, done)
}

func TestSelectiveTunnelDialError(t *testing.T) {
	dial := func(_ context.Context) (tunnel, error) {
		return nil, errors.New("dial failed")
	}
	directDial := func(_ context.Context, _, _ string) (net.Conn, error) {
		return nil, errors.New("unexpected")
	}

	clientConn, done := startSelective(t, dial, directDial, []string{"example.com"})

	go func() {
		_, _ = clientConn.Write([]byte("CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n"))
	}()

	// Connection is closed without a response.
	got, err := io.ReadAll(clientConn)
	require.NoError(t, err)
	assert.Empty(t, got)
	waitDone(t, done)
}

func TestSelectiveNonHTTPInput(t *testing.T) {
	dial := func(_ context.Context) (tunnel, error) {
		t.Error("unexpected tunnel dial")
		return nil, errors.New("unexpected")
	}
	directDial := func(_ context.Context, _, addr string) (net.Conn, error) {
		t.Errorf("unexpected direct dial to %s", addr)
		return nil, errors.New("unexpected")
	}

	clientConn, done := startSelective(t, dial, directDial, []string{"example.com"})

	go func() {
		_, _ = clientConn.Write([]byte("\x00\x01not http at all\r\n\r\n"))
	}()

	got, err := io.ReadAll(clientConn)
	require.NoError(t, err)
	assert.Empty(t, got)
	waitDone(t, done)
}

func TestSelectiveFirstRequestTimeout(t *testing.T) {
	dial := func(_ context.Context) (tunnel, error) {
		t.Error("unexpected tunnel dial")
		return nil, errors.New("unexpected")
	}
	directDial := func(_ context.Context, _, _ string) (net.Conn, error) {
		t.Error("unexpected direct dial")
		return nil, errors.New("unexpected")
	}

	// A server-speaks-first client (e.g. SMTP) sends nothing; the handler
	// must give up once the first-request deadline expires.
	clientConn, proxyConn := net.Pipe()
	defer clientConn.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleClientSelective(context.Background(), dial, directDial, newDomainMatcher([]string{"example.com"}), 50*time.Millisecond, proxyConn)
	}()

	waitDone(t, done)
}

func TestRecordingReader(t *testing.T) {
	rec := &recordingReader{r: strings.NewReader("abcdef"), on: true}
	buf := make([]byte, 3)
	_, err := io.ReadFull(rec, buf)
	require.NoError(t, err)
	rec.on = false
	_, err = io.ReadFull(rec, buf)
	require.NoError(t, err)
	assert.Equal(t, "abc", rec.buf.String())
}
