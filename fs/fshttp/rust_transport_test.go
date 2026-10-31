//go:build rusthttp && cgo && linux

package fshttp

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/accounting"
	"github.com/rclone/rclone/fs/rc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This helper uses the engine directly so a Go fallback cannot hide a failure.
func testRustTransport(t testing.TB) http.RoundTripper {
	return testRustTransportConfig(t, nil)
}

func testRustTransportConfig(t testing.TB, customize func(*fs.ConfigInfo)) http.RoundTripper {
	t.Helper()
	ci := *fs.GetConfig(context.Background())
	ci.Timeout = fs.Duration(5 * time.Second)
	ci.ConnectTimeout = fs.Duration(5 * time.Second)
	if customize != nil {
		customize(&ci)
	}
	transport, err := newRustHTTPTransport(&ci)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, transport.Close()) })
	return transport
}

func rustRequest(t testing.TB, method, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	require.NoError(t, err)
	return req
}

// Rustls rejects a CA certificate presented as a server leaf, including the
// default httptest certificate, so this fixture uses a distinct signed leaf.
func newRustHTTP2TestServer(t testing.TB, handler http.Handler) (*httptest.Server, []byte) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	now := time.Now()
	ca := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "rclone transport test CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		IsCA:     true, BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	ca, err = x509.ParseCertificate(caDER)
	require.NoError(t, err)
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, DNSNames: []string{"localhost"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &leafKey.PublicKey, caKey)
	require.NoError(t, err)
	leaf, err = x509.ParseCertificate(leafDER)
	require.NoError(t, err)
	ts := httptest.NewUnstartedServer(handler)
	ts.EnableHTTP2 = true
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER}, PrivateKey: leafKey, Leaf: leaf}}}
	ts.StartTLS()
	return ts, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
}

func http2TimeoutRecoveryHandler(healthyChunks int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Connection", r.RemoteAddr)
		if r.URL.Path == "/stall" {
			_, _ = io.WriteString(w, "s")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		if r.URL.Path == "/healthy" {
			for range healthyChunks {
				if _, err := io.WriteString(w, "x"); err != nil {
					return
				}
				w.(http.Flusher).Flush()
				select {
				case <-time.After(25 * time.Millisecond):
				case <-r.Context().Done():
					return
				}
			}
			return
		}
		_, _ = io.WriteString(w, "available")
	})
}

func TestRustTransportPreservesRequestAndRepeatedHeaders(t *testing.T) {
	const uri = "/a%2Fb/%7E?q=one%2Ftwo&space=+&empty="
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, uri, r.RequestURI)
		assert.Equal(t, "signed.example", r.Host)
		assert.Equal(t, []string{"first", "second"}, r.Header.Values("X-Repeated"))
		w.Header().Add("X-Repeated", "third")
		w.Header().Add("X-Repeated", "fourth")
		_, _ = io.WriteString(w, "ok")
	}))
	defer ts.Close()
	req := rustRequest(t, http.MethodGet, ts.URL+uri)
	req.Host = "signed.example"
	req.Header.Add("X-Repeated", "first")
	req.Header.Add("X-Repeated", "second")
	resp, err := testRustTransport(t).RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, []string{"third", "fourth"}, resp.Header.Values("X-Repeated"))
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "ok", string(data))
}

func TestRustTransportHTTPStatuses(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusNotFound, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "response")
			}))
			defer ts.Close()
			resp, err := testRustTransport(t).RoundTrip(rustRequest(t, http.MethodGet, ts.URL))
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, status, resp.StatusCode)
			data, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			assert.Equal(t, "response", string(data))
		})
	}
}

func TestRustTransportHEAD(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodHead, r.Method)
		w.Header().Set("Content-Length", "1234")
	}))
	defer ts.Close()
	resp, err := testRustTransport(t).RoundTrip(rustRequest(t, http.MethodHead, ts.URL))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.EqualValues(t, 1234, resp.ContentLength)
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Empty(t, data)
}

func TestRustTransportLeavesRedirectsToClient(t *testing.T) {
	var targetRequests atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			w.Header().Set("Location", "/target")
			w.WriteHeader(http.StatusFound)
			return
		}
		targetRequests.Add(1)
		_, _ = io.WriteString(w, "target")
	}))
	defer ts.Close()
	transport := testRustTransport(t)
	resp, err := transport.RoundTrip(rustRequest(t, http.MethodGet, ts.URL+"/start"))
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusFound, resp.StatusCode)
	assert.EqualValues(t, 0, targetRequests.Load())
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	resp, err = client.Get(ts.URL + "/start")
	require.NoError(t, err)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "target", string(data))
	assert.EqualValues(t, 1, targetRequests.Load())
}

func TestRustTransportStreamsBeforeResponseCompletes(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	finish := func() { releaseOnce.Do(func() { close(release) }) }
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "first")
		w.(http.Flusher).Flush()
		select {
		case <-release:
			_, _ = io.WriteString(w, "second")
		case <-r.Context().Done():
		}
	}))
	defer ts.Close()
	defer finish()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := rustRequest(t, http.MethodGet, ts.URL).WithContext(ctx)
	resp, err := testRustTransport(t).RoundTrip(req)
	require.NoError(t, err, "headers must arrive while the body is incomplete")
	defer resp.Body.Close()
	first := make([]byte, 5)
	_, err = io.ReadFull(resp.Body, first)
	require.NoError(t, err)
	assert.Equal(t, "first", string(first))
	finish()
	remaining, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "second", string(remaining))
}

func TestRustTransportCancelBeforeHeaders(t *testing.T) {
	entered := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
	}))
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	transport := testRustTransport(t)
	result := make(chan error, 1)
	req := rustRequest(t, http.MethodGet, ts.URL).WithContext(ctx)
	go func() {
		resp, err := transport.RoundTrip(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		result <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("request did not reach server")
	}
	cancel()
	select {
	case err := <-result:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not interrupt response headers")
	}
}

func TestRustTransportCancelResponseBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "first")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := testRustTransport(t).RoundTrip(rustRequest(t, http.MethodGet, ts.URL).WithContext(ctx))
	require.NoError(t, err)
	defer resp.Body.Close()
	first := make([]byte, 5)
	_, err = io.ReadFull(resp.Body, first)
	require.NoError(t, err)
	result := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(resp.Body)
		result <- err
	}()
	cancel()
	select {
	case err := <-result:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not interrupt response body")
	}
}

func TestRustTransportBodyCloseCancelsStream(t *testing.T) {
	canceled := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "first")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(canceled)
	}))
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := testRustTransport(t).RoundTrip(rustRequest(t, http.MethodGet, ts.URL).WithContext(ctx))
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.NoError(t, resp.Body.Close())
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("closing the body did not cancel the server stream")
	}
	_, err = resp.Body.Read(make([]byte, 1))
	assert.Error(t, err)
}

func TestRustTransportReusesConnection(t *testing.T) {
	var connections atomic.Int32
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "reusable")
	}))
	ts.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	ts.Start()
	defer ts.Close()
	transport := testRustTransport(t)
	for range 3 {
		resp, err := transport.RoundTrip(rustRequest(t, http.MethodGet, ts.URL))
		require.NoError(t, err)
		_, err = io.Copy(io.Discard, resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
	}
	assert.EqualValues(t, 1, connections.Load(), "completed responses should return the connection to the pool")
}

func TestRustTransportDoesNotRetryConnectionFailure(t *testing.T) {
	var requests atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer ts.Close()
	resp, err := testRustTransport(t).RoundTrip(rustRequest(t, http.MethodGet, ts.URL))
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.Error(t, err)
	assert.EqualValues(t, 1, requests.Load())
}

func TestRustTransportRejectsUntrustedTLS(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "untrusted")
	}))
	defer ts.Close()
	resp, err := testRustTransport(t).RoundTrip(rustRequest(t, http.MethodGet, ts.URL))
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.Error(t, err, "self-signed TLS certificates must be rejected by default")
	assert.False(t, errors.Is(err, context.Canceled))
	assert.NotEmpty(t, strings.TrimSpace(err.Error()))
}

func TestRustTransportSelection(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		t.Setenv("RCLONE_EXPERIMENTAL_RUST_HTTP", "")
		transport := NewTransportCustom(context.Background(), nil)
		t.Cleanup(transport.CloseIdleConnections)
		assert.Nil(t, transport.engine)
	})
	t.Run("enabled", func(t *testing.T) {
		t.Setenv("RCLONE_EXPERIMENTAL_RUST_HTTP", "1")
		transport := NewTransportCustom(context.Background(), nil)
		t.Cleanup(transport.CloseIdleConnections)
		assert.IsType(t, (*rustRoutingTransport)(nil), transport.engine)
	})
	t.Run("custom transport", func(t *testing.T) {
		t.Setenv("RCLONE_EXPERIMENTAL_RUST_HTTP", "1")
		transport := NewTransportCustom(context.Background(), func(inner *http.Transport) {
			inner.DisableKeepAlives = true
		})
		t.Cleanup(transport.CloseIdleConnections)
		assert.Nil(t, transport.engine, "custom Go transport fields must keep their behavior")
	})
}

func TestRustTransportProxyFallback(t *testing.T) {
	t.Setenv("RCLONE_EXPERIMENTAL_RUST_HTTP", "1")
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "http://unresolvable.invalid/resource", r.RequestURI)
		_, _ = io.WriteString(w, "proxied")
	}))
	defer proxy.Close()
	ctx, ci := fs.AddConfig(context.Background())
	ci.HTTPProxy = proxy.URL
	transport := NewTransportCustom(ctx, nil)
	t.Cleanup(transport.CloseIdleConnections)
	resp, err := transport.RoundTrip(rustRequest(t, http.MethodGet, "http://unresolvable.invalid/resource"))
	require.NoError(t, err)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "proxied", string(data))
}

func TestRustTransportRequestBodyFallback(t *testing.T) {
	t.Setenv("RCLONE_EXPERIMENTAL_RUST_HTTP", "1")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPut, r.Method)
		data, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		assert.Equal(t, "upload", string(data))
		_, _ = io.WriteString(w, "stored")
	}))
	defer ts.Close()
	transport := NewTransportCustom(context.Background(), nil)
	t.Cleanup(transport.CloseIdleConnections)
	req, err := http.NewRequest(http.MethodPut, ts.URL, strings.NewReader("upload"))
	require.NoError(t, err)
	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "stored", string(data))
}

func TestRustTransportCustomCAFallback(t *testing.T) {
	t.Setenv("RCLONE_EXPERIMENTAL_RUST_HTTP", "1")
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "trusted")
	}))
	defer ts.Close()
	certPath := filepath.Join(t.TempDir(), "ca.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw})
	require.NoError(t, os.WriteFile(certPath, certPEM, 0600))
	ctx, ci := fs.AddConfig(context.Background())
	ci.CaCert = []string{certPath}
	transport := NewTransportCustom(ctx, nil)
	t.Cleanup(transport.CloseIdleConnections)
	resp, err := transport.RoundTrip(rustRequest(t, http.MethodGet, ts.URL))
	require.NoError(t, err)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "trusted", string(data))
}

func rustShortTimeout(ci *fs.ConfigInfo) {
	ci.ConnectTimeout = fs.Duration(50 * time.Millisecond)
	ci.Timeout = fs.Duration(100 * time.Millisecond)
}

func TestRustTransportIdleTimeoutAndRecovery(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "first")
		if r.URL.Path == "/stall" {
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}
	}))
	defer ts.Close()
	transport := testRustTransportConfig(t, rustShortTimeout)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp, err := transport.RoundTrip(rustRequest(t, http.MethodGet, ts.URL+"/stall").WithContext(ctx))
	require.NoError(t, err)
	started := time.Now()
	_, err = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Error(t, err)
	var networkError net.Error
	require.ErrorAs(t, err, &networkError)
	assert.True(t, networkError.Timeout(), "idle timeout must retain net.Error semantics: %v", err)
	assert.Less(t, time.Since(started), time.Second)
	resp, err = transport.RoundTrip(rustRequest(t, http.MethodGet, ts.URL+"/healthy").WithContext(ctx))
	require.NoError(t, err, "a timed-out response must not poison later requests")
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "first", string(data))
}

func TestRustTransportProgressResetsIdleTimeout(t *testing.T) {
	const chunks = 12
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for range chunks {
			if _, err := io.WriteString(w, "x"); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-time.After(25 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
		}
	}))
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	transport := testRustTransportConfig(t, rustShortTimeout)
	resp, err := transport.RoundTrip(rustRequest(t, http.MethodGet, ts.URL).WithContext(ctx))
	require.NoError(t, err)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "healthy progress must permit a stream longer than the idle timeout")
	assert.Equal(t, strings.Repeat("x", chunks), string(data))
}

func TestRustTransportHeaderTrickleTimesOut(t *testing.T) {
	stop := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, writer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = writer.WriteString("HTTP/1.1 200 OK\r\nX-Slow: ")
		if writer.Flush() != nil {
			return
		}
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if _, err := writer.WriteString("x"); err != nil {
					return
				}
				if writer.Flush() != nil {
					return
				}
			}
		}
	}))
	defer ts.Close()
	defer close(stop)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	transport := testRustTransportConfig(t, rustShortTimeout)
	started := time.Now()
	resp, err := transport.RoundTrip(rustRequest(t, http.MethodGet, ts.URL).WithContext(ctx))
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.Error(t, err)
	var networkError net.Error
	require.ErrorAs(t, err, &networkError)
	assert.True(t, networkError.Timeout(), "header budget must expire despite socket progress: %v", err)
	assert.GreaterOrEqual(t, time.Since(started), 80*time.Millisecond)
	assert.Less(t, time.Since(started), time.Second)
}

func TestRustTransportHTTP2StalledStreamDoesNotBlockOtherStreams(t *testing.T) {
	const healthyChunks = 16
	ts, certPEM := newRustHTTP2TestServer(t, http2TimeoutRecoveryHandler(healthyChunks))
	defer ts.Close()
	certPath := filepath.Join(t.TempDir(), "http2-ca.pem")
	require.NoError(t, os.WriteFile(certPath, certPEM, 0600))
	t.Setenv("SSL_CERT_FILE", certPath)
	transport := testRustTransportConfig(t, rustShortTimeout)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	healthy, err := transport.RoundTrip(rustRequest(t, http.MethodGet, ts.URL+"/healthy").WithContext(ctx))
	require.NoError(t, err)
	defer healthy.Body.Close()
	require.Equal(t, 2, healthy.ProtoMajor, "regression requires multiplexed HTTP/2")
	type readResult struct {
		data []byte
		err  error
	}
	healthyDone := make(chan readResult, 1)
	go func() {
		data, err := io.ReadAll(healthy.Body)
		healthyDone <- readResult{data: data, err: err}
	}()
	stalled, err := transport.RoundTrip(rustRequest(t, http.MethodGet, ts.URL+"/stall").WithContext(ctx))
	require.NoError(t, err)
	defer stalled.Body.Close()
	require.Equal(t, 2, stalled.ProtoMajor)
	require.Equal(t, healthy.Header.Get("X-Connection"), stalled.Header.Get("X-Connection"),
		"stalled and healthy streams must share a connection")
	started := time.Now()
	_, err = io.ReadAll(stalled.Body)
	require.Error(t, err)
	var networkError net.Error
	require.ErrorAs(t, err, &networkError)
	assert.True(t, networkError.Timeout(), "activity on another stream must not hide this stream's timeout: %v", err)
	assert.Less(t, time.Since(started), time.Second)
	select {
	case result := <-healthyDone:
		require.NoError(t, result.err, "one stream timing out must leave its healthy sibling usable")
		assert.Equal(t, strings.Repeat("x", healthyChunks), string(result.data))
	case <-ctx.Done():
		t.Fatal("healthy transfer worker remained blocked after its sibling timed out")
	}
	resp, err := transport.RoundTrip(rustRequest(t, http.MethodGet, ts.URL+"/available").WithContext(ctx))
	require.NoError(t, err, "a transfer worker must be able to start a new request after the timeout")
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "available", string(data))
	assert.Equal(t, healthy.Header.Get("X-Connection"), resp.Header.Get("X-Connection"))
}

func TestRustTransportUnannouncedTrailers(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, writer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = writer.WriteString("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n0\r\nX-Checksum: ready\r\n\r\n")
		_ = writer.Flush()
	}))
	defer ts.Close()
	resp, err := testRustTransport(t).RoundTrip(rustRequest(t, http.MethodGet, ts.URL))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Empty(t, resp.Trailer.Get("X-Checksum"), "trailers become available after EOF")
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(data))
	assert.Equal(t, "ready", resp.Trailer.Get("X-Checksum"))
}

func TestRustTransportExplicitContentLength(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "5")
		_, _ = io.WriteString(w, "hello")
	}))
	defer ts.Close()
	resp, err := testRustTransport(t).RoundTrip(rustRequest(t, http.MethodGet, ts.URL))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.EqualValues(t, 5, resp.ContentLength)
	assert.Equal(t, "5", resp.Header.Get("Content-Length"))
	assert.Empty(t, resp.TransferEncoding)
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(data))
}

func TestRustTransportUpgradeFallback(t *testing.T) {
	t.Setenv("RCLONE_EXPERIMENTAL_RUST_HTTP", "1")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "testproto", r.Header.Get("Upgrade"))
		conn, stream, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		_, _ = stream.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: testproto\r\n\r\n")
		if stream.Flush() != nil {
			return
		}
		data := make([]byte, 4)
		if _, err := io.ReadFull(stream, data); err == nil && string(data) == "ping" {
			_, _ = conn.Write([]byte("pong"))
		}
	}))
	defer ts.Close()
	transport := NewTransportCustom(context.Background(), nil)
	t.Cleanup(transport.CloseIdleConnections)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req := rustRequest(t, http.MethodGet, ts.URL).WithContext(ctx)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "testproto")
	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	stream, ok := resp.Body.(io.ReadWriteCloser)
	require.True(t, ok, "Go fallback must preserve an upgraded duplex connection")
	_, err = stream.Write([]byte("ping"))
	require.NoError(t, err)
	data := make([]byte, 4)
	_, err = io.ReadFull(stream, data)
	require.NoError(t, err)
	assert.Equal(t, "pong", string(data))
}

func TestRustTransportRequestFramingFallback(t *testing.T) {
	for _, header := range []string{"Content-Length", "Transfer-Encoding"} {
		t.Run(header, func(t *testing.T) {
			t.Setenv("RCLONE_EXPERIMENTAL_RUST_HTTP", "1")
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.EqualValues(t, 0, r.ContentLength)
				assert.Empty(t, r.TransferEncoding)
				assert.Empty(t, r.Header.Get(header), "framing is derived from the request body, not the header map")
				_, _ = io.WriteString(w, "bodyless")
			}))
			defer ts.Close()
			transport := NewTransportCustom(context.Background(), nil)
			t.Cleanup(transport.CloseIdleConnections)
			req := rustRequest(t, http.MethodGet, ts.URL)
			if header == "Content-Length" {
				req.Header.Set(header, "123")
			} else {
				req.Header.Set(header, "chunked")
			}
			resp, err := transport.RoundTrip(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			data, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			assert.Equal(t, "bodyless", string(data))
		})
	}
}

func TestRustTransportCloseIdleConnectionsPreservesActiveResponse(t *testing.T) {
	continueBody := make(chan struct{})
	var continueOnce sync.Once
	finish := func() { continueOnce.Do(func() { close(continueBody) }) }
	var connections atomic.Int32
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "first")
		if r.URL.Path == "/active" {
			w.(http.Flusher).Flush()
			select {
			case <-continueBody:
				_, _ = io.WriteString(w, "second")
			case <-r.Context().Done():
			}
		}
	}))
	ts.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	ts.Start()
	defer ts.Close()
	defer finish()
	transport := testRustTransport(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp, err := transport.RoundTrip(rustRequest(t, http.MethodGet, ts.URL+"/active").WithContext(ctx))
	require.NoError(t, err)
	defer resp.Body.Close()
	first := make([]byte, 5)
	_, err = io.ReadFull(resp.Body, first)
	require.NoError(t, err)
	transport.(interface{ CloseIdleConnections() }).CloseIdleConnections()
	finish()
	remaining, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "retiring a pool must preserve active response ownership")
	assert.Equal(t, "second", string(remaining))
	require.NoError(t, resp.Body.Close())
	resp, err = transport.RoundTrip(rustRequest(t, http.MethodGet, ts.URL+"/next").WithContext(ctx))
	require.NoError(t, err)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "first", string(data))
	assert.EqualValues(t, 2, connections.Load(), "the next request should use a fresh pool")
}

func TestRustTransportGoFallbackKeepsRustEligible(t *testing.T) {
	t.Setenv("RCLONE_EXPERIMENTAL_RUST_HTTP", "1")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			data, err := io.ReadAll(r.Body)
			assert.NoError(t, err)
			assert.Equal(t, "upload", string(data))
		}
		_, _ = io.WriteString(w, r.Method)
	}))
	defer ts.Close()
	transport := NewTransportCustom(context.Background(), nil)
	t.Cleanup(transport.CloseIdleConnections)
	routing, ok := transport.engine.(*rustRoutingTransport)
	require.True(t, ok)
	get := rustRequest(t, http.MethodGet, ts.URL)
	require.True(t, routing.canUseRust(get), "default TLS=%t unchanged settings=%t HTTP2=%+v protocols=%v",
		rustDefaultTLS(transport.TLSClientConfig, routing.alpn), goSettings(transport.Transport) == routing.settings,
		transport.HTTP2, transport.Protocols)
	post, err := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader("upload"))
	require.NoError(t, err)
	require.False(t, routing.canUseRust(post))
	resp, err := transport.RoundTrip(post)
	require.NoError(t, err)
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.MethodPost, string(data))
	require.True(t, routing.canUseRust(get), "Go's lazy initialization must not disable the Rust path")
	resp, err = transport.RoundTrip(get)
	require.NoError(t, err)
	defer resp.Body.Close()
	data, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, http.MethodGet, string(data))
	assert.True(t, routing.canUseRust(get))
}

func TestRustTransportBodyCloseInterruptsDynamicBandwidthWait(t *testing.T) {
	t.Setenv("RCLONE_EXPERIMENTAL_RUST_HTTP", "1")
	limits, err := rc.Calls.Get("core/bwlimit").Fn(context.Background(), rc.Params{})
	require.NoError(t, err)
	previous := fs.BwPair{
		Tx: fs.SizeSuffix(limits["bytesPerSecondTx"].(int64)),
		Rx: fs.SizeSuffix(limits["bytesPerSecondRx"].(int64)),
	}
	t.Cleanup(func() { accounting.TokenBucket.SetBwLimit(previous) })
	accounting.TokenBucket.SetBwLimit(fs.BwPair{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", 4096))
	}))
	defer ts.Close()
	transport := NewTransportCustom(context.Background(), nil)
	t.Cleanup(transport.CloseIdleConnections)
	routing, ok := transport.engine.(*rustRoutingTransport)
	require.True(t, ok)
	req := rustRequest(t, http.MethodGet, ts.URL)
	require.True(t, routing.canUseRust(req), "default TLS=%t unchanged settings=%t HTTP2=%+v protocols=%v",
		rustDefaultTLS(transport.TLSClientConfig, routing.alpn), goSettings(transport.Transport) == routing.settings,
		transport.HTTP2, transport.Protocols)
	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	accounting.TokenBucket.SetBwLimit(fs.BwPair{Rx: 1})
	assert.False(t, routing.canUseRust(req), "new requests must use Go while a transport limit is active")
	readResult := make(chan error, 1)
	go func() {
		_, err := resp.Body.Read(make([]byte, 512))
		readResult <- err
	}()
	select {
	case err := <-readResult:
		t.Fatalf("the active response did not wait for the new bandwidth limit: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	closeResult := make(chan error, 1)
	go func() { closeResult <- resp.Body.Close() }()
	select {
	case err := <-closeResult:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("closing the body was blocked behind its bandwidth wait")
	}
	select {
	case err := <-readResult:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("closing the body did not release the transfer worker's bandwidth wait")
	}
}

func TestRustTransportGzipPolicy(t *testing.T) {
	const payload = "compress me"
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	_, err := io.WriteString(writer, payload)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	for _, test := range []struct {
		name         string
		rangeHeader  string
		encoding     string
		wantEncoding string
		noGzip       bool
		wantBody     []byte
		uncompressed bool
	}{
		{name: "automatic", wantEncoding: "gzip", wantBody: []byte(payload), uncompressed: true},
		{name: "range", rangeHeader: "bytes=0-3", wantBody: []byte("part")},
		{name: "explicit", encoding: "gzip", wantEncoding: "gzip", wantBody: compressed.Bytes()},
		{name: "disabled", noGzip: true, wantBody: []byte(payload)},
	} {
		t.Run(test.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, test.wantEncoding, r.Header.Get("Accept-Encoding"))
				if r.Header.Get("Range") != "" {
					w.WriteHeader(http.StatusPartialContent)
					_, _ = io.WriteString(w, "part")
					return
				}
				if r.Header.Get("Accept-Encoding") == "gzip" {
					w.Header().Set("Content-Encoding", "gzip")
					_, _ = w.Write(compressed.Bytes())
					return
				}
				_, _ = io.WriteString(w, payload)
			}))
			defer ts.Close()
			transport := testRustTransportConfig(t, func(ci *fs.ConfigInfo) { ci.NoGzip = test.noGzip })
			req := rustRequest(t, http.MethodGet, ts.URL)
			if test.rangeHeader != "" {
				req.Header.Set("Range", test.rangeHeader)
			}
			if test.encoding != "" {
				req.Header.Set("Accept-Encoding", test.encoding)
			}
			resp, err := transport.RoundTrip(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			data, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			assert.Equal(t, test.wantBody, data)
			assert.Equal(t, test.uncompressed, resp.Uncompressed)
			if test.uncompressed {
				assert.Empty(t, resp.Header.Get("Content-Encoding"))
				assert.Empty(t, resp.Header.Get("Content-Length"))
				assert.EqualValues(t, -1, resp.ContentLength)
			}
		})
	}
}

func TestRustTransportCancelWithoutReadingBody(t *testing.T) {
	canceled := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "first")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(canceled)
	}))
	defer ts.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp, err := testRustTransport(t).RoundTrip(rustRequest(t, http.MethodGet, ts.URL).WithContext(ctx))
	require.NoError(t, err)
	defer resp.Body.Close()
	cancel()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("canceling the parent context did not release an unread response")
	}
}

func TestRustTransportLegacyCancelFallback(t *testing.T) {
	t.Setenv("RCLONE_EXPERIMENTAL_RUST_HTTP", "1")
	transport := NewTransportCustom(context.Background(), nil)
	t.Cleanup(transport.CloseIdleConnections)
	routing, ok := transport.engine.(*rustRoutingTransport)
	require.True(t, ok)
	req := rustRequest(t, http.MethodGet, "http://127.0.0.1:1/")
	require.True(t, routing.canUseRust(req))
	req.Cancel = make(chan struct{})
	assert.False(t, routing.canUseRust(req), "the legacy cancellation channel must retain Go transport semantics")
}

func TestRustTransportCopyPreservesPrefixBeforeTruncation(t *testing.T) {
	prefix := bytes.Repeat([]byte("valid-prefix-"), 8192)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, writer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = fmt.Fprintf(writer, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n", len(prefix)+1000)
		_, _ = writer.Write(prefix)
		_ = writer.Flush()
	}))
	defer ts.Close()
	resp, err := testRustTransport(t).RoundTrip(rustRequest(t, http.MethodGet, ts.URL))
	require.NoError(t, err)
	defer resp.Body.Close()
	_, ok := resp.Body.(io.WriterTo)
	require.True(t, ok, "the regression must exercise the response body's WriterTo path")
	var output bytes.Buffer
	n, err := io.Copy(&output, resp.Body)
	require.Error(t, err, "a truncated response must not appear complete")
	assert.EqualValues(t, len(prefix), n, "valid bytes must survive an error encountered while batching frames")
	assert.Equal(t, prefix, output.Bytes())
}

type rustShortWriter struct {
	accepted []byte
}

func (w *rustShortWriter) Write(p []byte) (int, error) {
	n := len(p) / 2
	w.accepted = append(w.accepted, p[:n]...)
	return n, nil
}

func TestRustTransportCopyReportsShortWrite(t *testing.T) {
	const payload = "short-write-payload"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, payload)
	}))
	defer ts.Close()
	resp, err := testRustTransport(t).RoundTrip(rustRequest(t, http.MethodGet, ts.URL))
	require.NoError(t, err)
	defer resp.Body.Close()
	_, ok := resp.Body.(io.WriterTo)
	require.True(t, ok, "the regression must exercise the response body's WriterTo path")
	output := &rustShortWriter{}
	n, err := io.Copy(output, resp.Body)
	require.ErrorIs(t, err, io.ErrShortWrite)
	assert.EqualValues(t, len(output.accepted), n, "the count must contain only bytes accepted by the writer")
	assert.Less(t, n, int64(len(payload)))
	assert.Equal(t, payload[:int(n)], string(output.accepted))
}
