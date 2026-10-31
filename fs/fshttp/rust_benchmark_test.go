//go:build rusthttp && cgo && linux

package fshttp

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
)

// BenchmarkHTTPTransport compares identical rclone wrappers and server payloads.
// The reported allocations include Go allocations only; measure process RSS for
// the Rust allocator and runtime as well.
func BenchmarkHTTPTransport(b *testing.B) {
	for _, size := range []int{0, 4 << 10, 1 << 20} {
		payload := bytes.Repeat([]byte("x"), size)
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", strconv.Itoa(size))
			_, _ = w.Write(payload)
		}))
		b.Run(fmt.Sprintf("bytes=%d", size), func(b *testing.B) {
			for _, engine := range []string{"go", "rust"} {
				b.Run(engine, func(b *testing.B) {
					for _, parallel := range []bool{false, true} {
						mode := "serial"
						if parallel {
							mode = "parallel"
						}
						b.Run(mode, func(b *testing.B) {
							enabled := ""
							if engine == "rust" {
								enabled = "1"
							}
							b.Setenv("RCLONE_EXPERIMENTAL_RUST_HTTP", enabled)
							ctx, _ := fs.AddConfig(context.Background())
							transport := NewTransportCustom(ctx, nil)
							b.Cleanup(transport.CloseIdleConnections)
							if engine == "rust" {
								if _, ok := transport.engine.(*rustRoutingTransport); !ok {
									b.Fatal("Rust engine was not selected; benchmark would measure Go fallback")
								}
							} else if transport.engine != nil {
								b.Fatal("Go baseline unexpectedly selected another engine")
							}
							request, err := http.NewRequest(http.MethodGet, ts.URL, nil)
							if err != nil {
								b.Fatal(err)
							}
							if engine == "rust" && !transport.engine.(*rustRoutingTransport).canUseRust(request) {
								b.Fatal("request would fall back to Go; Rust measurement is invalid")
							}
							// Warm the pool before timing either engine.
							if err := benchmarkHTTPRoundTrip(transport, request, size); err != nil {
								b.Fatal(err)
							}
							b.ReportAllocs()
							b.SetBytes(int64(size))
							b.ResetTimer()
							if parallel {
								var firstErr error
								var errOnce sync.Once
								b.RunParallel(func(pb *testing.PB) {
									for pb.Next() {
										if err := benchmarkHTTPRoundTrip(transport, request, size); err != nil {
											errOnce.Do(func() { firstErr = err })
											return
										}
									}
								})
								if firstErr != nil {
									b.Fatal(firstErr)
								}
							} else {
								for range b.N {
									if err := benchmarkHTTPRoundTrip(transport, request, size); err != nil {
										b.Fatal(err)
									}
								}
							}
						})
					}
				})
			}
		})
		ts.Close()
	}
}

func benchmarkHTTPRoundTrip(transport http.RoundTripper, template *http.Request, size int) error {
	// rclone's wrapper sets headers, so each concurrent operation owns a request.
	request := template.Clone(template.Context())
	resp, err := transport.RoundTrip(request)
	if err != nil {
		return err
	}
	n, readErr := io.Copy(io.Discard, resp.Body)
	closeErr := resp.Body.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	if resp.StatusCode != http.StatusOK || n != int64(size) {
		return fmt.Errorf("unexpected response: status=%d bytes=%d, want status=200 bytes=%d", resp.StatusCode, n, size)
	}
	return nil
}

// BenchmarkHTTP2TimeoutRecovery measures when a stalled transfer worker becomes
// usable while another stream keeps its HTTP/2 connection active for 400 ms.
func BenchmarkHTTP2TimeoutRecovery(b *testing.B) {
	const healthyChunks = 16
	ts, certPEM := newRustHTTP2TestServer(b, http2TimeoutRecoveryHandler(healthyChunks))
	defer ts.Close()
	certPath := filepath.Join(b.TempDir(), "http2-ca.pem")
	if err := os.WriteFile(certPath, certPEM, 0600); err != nil {
		b.Fatal(err)
	}
	b.Setenv("SSL_CERT_FILE", certPath)
	for _, engine := range []string{"go", "rust"} {
		b.Run(engine, func(b *testing.B) {
			var transport http.RoundTripper
			if engine == "rust" {
				transport = testRustTransportConfig(b, rustShortTimeout)
			} else {
				roots := x509.NewCertPool()
				if !roots.AppendCertsFromPEM(certPEM) {
					b.Fatal("fixture CA was not parsed")
				}
				ctx, ci := fs.AddConfig(context.Background())
				rustShortTimeout(ci)
				goTransport := NewTransportCustom(ctx, func(t *http.Transport) {
					t.TLSClientConfig.RootCAs = roots
					t.ForceAttemptHTTP2 = true
				})
				b.Cleanup(goTransport.CloseIdleConnections)
				transport = goTransport
			}
			var recovery time.Duration
			b.ResetTimer()
			for range b.N {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				healthy, err := transport.RoundTrip(rustRequest(b, http.MethodGet, ts.URL+"/healthy").WithContext(ctx))
				if err != nil {
					cancel()
					b.Fatal(err)
				}
				type readResult struct {
					data []byte
					err  error
				}
				healthyDone := make(chan readResult, 1)
				go func() {
					data, err := io.ReadAll(healthy.Body)
					_ = healthy.Body.Close()
					healthyDone <- readResult{data: data, err: err}
				}()
				stalled, err := transport.RoundTrip(rustRequest(b, http.MethodGet, ts.URL+"/stall").WithContext(ctx))
				if err != nil {
					cancel()
					_ = healthy.Body.Close()
					b.Fatal(err)
				}
				if healthy.ProtoMajor != 2 || stalled.ProtoMajor != 2 ||
					healthy.Header.Get("X-Connection") != stalled.Header.Get("X-Connection") {
					cancel()
					_ = stalled.Body.Close()
					b.Fatal("fixture did not multiplex both requests over one HTTP/2 connection")
				}
				started := time.Now()
				_, err = io.ReadAll(stalled.Body)
				_ = stalled.Body.Close()
				var networkError net.Error
				if !errors.As(err, &networkError) || !networkError.Timeout() {
					cancel()
					b.Fatalf("expected a transport timeout, got %v", err)
				}
				if ctx.Err() != nil {
					cancel()
					b.Fatal("context deadline hid the transport timeout")
				}
				next, err := transport.RoundTrip(rustRequest(b, http.MethodGet, ts.URL+"/available").WithContext(ctx))
				if err != nil {
					cancel()
					b.Fatal(err)
				}
				data, readErr := io.ReadAll(next.Body)
				_ = next.Body.Close()
				recovery += time.Since(started)
				if readErr != nil || string(data) != "available" {
					cancel()
					b.Fatalf("worker could not perform the next request: %q, %v", data, readErr)
				}
				select {
				case result := <-healthyDone:
					if result.err != nil || string(result.data) != strings.Repeat("x", healthyChunks) {
						cancel()
						b.Fatalf("healthy sibling failed: %q, %v", result.data, result.err)
					}
				case <-ctx.Done():
					cancel()
					b.Fatal("healthy sibling remained blocked")
				}
				cancel()
			}
			b.ReportMetric(float64(recovery.Nanoseconds())/float64(b.N), "recovery_ns/op")
		})
	}
}
