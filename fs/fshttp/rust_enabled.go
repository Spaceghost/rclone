//go:build rusthttp && cgo && linux

package fshttp

/*
#cgo CFLAGS: -I${SRCDIR}/rust/include
#cgo LDFLAGS: -L${SRCDIR}/rust/target/release -lrclone_http
#cgo linux LDFLAGS: -ldl -lpthread -lm
#include "rclone_http.h"
*/
import "C"

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"os"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/accounting"
	"github.com/rclone/rclone/lib/buildinfo"
	"golang.org/x/net/http/httpguts"
)

func init() {
	buildinfo.Tags = append(buildinfo.Tags, "rusthttp")
}

type rustRoutingTransport struct {
	goTransport *http.Transport
	rust        *rustHTTPTransport
	settings    rustGoSettings
	alpn        []string
}

type rustGoSettings struct {
	dial, dialContext, proxy                   uintptr
	tlsTimeout, headerTimeout, idleTimeout     time.Duration
	expectTimeout                              time.Duration
	maxIdle, maxIdlePerHost, maxConnsPerHost   int
	maxHeaderBytes                             int64
	readBuffer, writeBuffer                    int
	noCompression, forceHTTP2, tlsNextProtoNil bool
	tlsNextProtoLen                            int
	http2NextProto                             uintptr
}

func goSettings(t *http.Transport) rustGoSettings {
	return rustGoSettings{
		dial: reflect.ValueOf(t.Dial).Pointer(), dialContext: reflect.ValueOf(t.DialContext).Pointer(),
		proxy:      reflect.ValueOf(t.Proxy).Pointer(),
		tlsTimeout: t.TLSHandshakeTimeout, headerTimeout: t.ResponseHeaderTimeout,
		idleTimeout: t.IdleConnTimeout, expectTimeout: t.ExpectContinueTimeout,
		maxIdle: t.MaxIdleConns, maxIdlePerHost: t.MaxIdleConnsPerHost, maxConnsPerHost: t.MaxConnsPerHost,
		maxHeaderBytes: t.MaxResponseHeaderBytes, readBuffer: t.ReadBufferSize, writeBuffer: t.WriteBufferSize,
		noCompression: t.DisableCompression, forceHTTP2: t.ForceAttemptHTTP2, tlsNextProtoNil: t.TLSNextProto == nil,
		tlsNextProtoLen: len(t.TLSNextProto), http2NextProto: reflect.ValueOf(t.TLSNextProto["h2"]).Pointer(),
	}
}

var rustTLSFields = func() []int {
	typeOf := reflect.TypeFor[tls.Config]()
	var fields []int
	for i := range typeOf.NumField() {
		if typeOf.Field(i).IsExported() && typeOf.Field(i).Name != "NextProtos" {
			fields = append(fields, i)
		}
	}
	return fields
}()

func rustDefaultTLS(config *tls.Config, alpn []string) bool {
	if config == nil {
		return true
	}
	value := reflect.ValueOf(config).Elem()
	if !slices.Equal(config.NextProtos, alpn) {
		return false
	}
	// Internal TLS locks and session state are owned by net/http.
	for _, field := range rustTLSFields {
		if !value.Field(field).IsZero() {
			return false
		}
	}
	return true
}

func selectRustHTTPTransport(ctx context.Context, transport *http.Transport, customized bool) http.RoundTripper {
	if os.Getenv("RCLONE_EXPERIMENTAL_RUST_HTTP") != "1" || customized {
		return nil
	}
	ci := fs.GetConfig(ctx)
	if ci.InsecureSkipVerify || len(ci.CaCert) != 0 || ci.ClientCert != "" || ci.ClientKey != "" ||
		ci.BindAddr != nil || ci.TrafficClass != 0 || len(ci.BwLimit) != 0 ||
		ci.DisableHTTPKeepAlives || ci.Dump&fs.DumpTrace != 0 {
		return nil
	}
	// net/http sets its HTTP/2 and ALPN defaults lazily. Initialize those before
	// sharing the exported configuration with the eligibility checks.
	transport.CloseIdleConnections()
	rust, err := newRustHTTPTransport(ci)
	if err != nil {
		fs.Logf(nil, "Rust HTTP initialization failed; using Go HTTP: %v", err)
		return nil
	}
	fs.Debugf(nil, "Experimental Rust HTTP enabled for eligible GET and HEAD requests")
	return &rustRoutingTransport{goTransport: transport, rust: rust, settings: goSettings(transport),
		alpn: slices.Clone(transport.TLSClientConfig.NextProtos)}
}

func (t *rustRoutingTransport) canUseRust(req *http.Request) bool {
	if req.URL == nil || req.Cancel != nil || (req.Method != http.MethodGet && req.Method != http.MethodHead) ||
		(req.Body != nil && req.Body != http.NoBody) || req.ContentLength > 0 || len(req.Trailer) != 0 ||
		len(req.TransferEncoding) != 0 || req.Close || req.URL.User != nil || req.URL.Opaque != "" ||
		req.Header.Get("Upgrade") != "" || httpguts.HeaderValuesContainsToken(req.Header.Values("Connection"), "upgrade") ||
		req.Header.Get("Content-Length") != "" ||
		req.Header.Get("Transfer-Encoding") != "" || req.Header.Get("Trailer") != "" ||
		req.URL.Fragment != "" || (req.URL.Scheme != "http" && req.URL.Scheme != "https") ||
		(req.Host != "" && req.Host != req.URL.Host) || httptrace.ContextClientTrace(req.Context()) != nil ||
		accounting.TokenBucket.TransportBandwidthLimited() {
		return false
	}
	// Callers can customize the embedded Go transport before its first use.
	if !rustDefaultTLS(t.goTransport.TLSClientConfig, t.alpn) || goSettings(t.goTransport) != t.settings ||
		t.goTransport.DisableKeepAlives || t.goTransport.DialTLSContext != nil || t.goTransport.DialTLS != nil ||
		(t.goTransport.HTTP2 != nil && !reflect.ValueOf(t.goTransport.HTTP2).Elem().IsZero()) ||
		t.goTransport.Protocols != nil {
		return false
	}
	if t.goTransport.Proxy != nil {
		proxy, err := t.goTransport.Proxy(req)
		if proxy != nil || err != nil {
			return false
		}
	}
	for _, character := range req.URL.Host {
		if character > 127 || character == '%' {
			return false
		}
	}
	return true
}

func (t *rustRoutingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !t.canUseRust(req) {
		return t.goTransport.RoundTrip(req)
	}
	// A failed exchange must never be replayed through the other engine.
	return t.rust.RoundTrip(req)
}

func (t *rustRoutingTransport) CloseIdleConnections() {
	t.rust.CloseIdleConnections()
}

type rustHTTPTransport struct {
	mu     sync.Mutex
	client C.uint64_t
	ci     fs.ConfigInfo
	closed bool
}

func newRustHTTPTransport(ci *fs.ConfigInfo) (*rustHTTPTransport, error) {
	t := &rustHTTPTransport{ci: *ci}
	if _, err := t.clientHandle(); err != nil {
		return nil, err
	}
	runtime.SetFinalizer(t, func(t *rustHTTPTransport) { _ = t.Close() })
	return t, nil
}

func rustMilliseconds(d fs.Duration) C.uint64_t {
	if d <= 0 {
		return 0
	}
	return C.uint64_t(max(1, time.Duration(d).Milliseconds()))
}

func (t *rustHTTPTransport) clientHandle() (C.uint64_t, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.clientHandleLocked()
}

func (t *rustHTTPTransport) clientHandleLocked() (C.uint64_t, error) {
	if t.closed {
		return 0, errors.New("Rust HTTP transport is closed")
	}
	if t.client == 0 {
		var errorBuffer [256]C.char
		var http1Only C.uint8_t
		if t.ci.DisableHTTP2 {
			http1Only = 1
		}
		code := C.rclone_http_client_new(rustMilliseconds(t.ci.ConnectTimeout), rustMilliseconds(t.ci.Timeout),
			C.uint32_t(2*(t.ci.Checkers+t.ci.Transfers+1)), http1Only, &t.client,
			&errorBuffer[0], C.size_t(len(errorBuffer)))
		if code != C.RCLONE_HTTP_OK {
			return 0, rustError("initialization", code, &errorBuffer[0], nil)
		}
	}
	return t.client, nil
}

// Close releases the client pool; outstanding responses remain independently owned.
func (t *rustHTTPTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	if t.client != 0 {
		C.rclone_http_client_free(t.client)
		t.client = 0
	}
	return nil
}

// CloseIdleConnections retires the pool without canceling active response streams.
func (t *rustHTTPTransport) CloseIdleConnections() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.client != 0 {
		C.rclone_http_client_free(t.client)
		t.client = 0
	}
}

func rustString(s string) *C.uint8_t {
	return (*C.uint8_t)(unsafe.Pointer(unsafe.StringData(s)))
}

func (t *rustHTTPTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	if req.URL == nil || (req.Method != http.MethodGet && req.Method != http.MethodHead) ||
		(req.Body != nil && req.Body != http.NoBody) || req.ContentLength > 0 {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, errors.New("Rust HTTP supports bodyless GET and HEAD requests")
	}
	var headers strings.Builder
	for name, values := range req.Header {
		if !httpguts.ValidHeaderFieldName(name) {
			return nil, errors.New("Rust HTTP request has an invalid header name")
		}
		if strings.EqualFold(name, "Host") || strings.EqualFold(name, "Content-Length") ||
			strings.EqualFold(name, "Transfer-Encoding") || strings.EqualFold(name, "Trailer") {
			continue
		}
		for _, value := range values {
			if !httpguts.ValidHeaderFieldValue(value) {
				return nil, fmt.Errorf("Rust HTTP request has an invalid value for header %q", name)
			}
			headers.WriteString(name)
			headers.WriteByte(':')
			headers.WriteString(textproto.TrimString(value))
			headers.WriteString("\r\n")
		}
	}
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	if !httpguts.ValidHostHeader(host) {
		return nil, errors.New("Rust HTTP request has an invalid Host header")
	}
	headers.WriteString("Host:")
	headers.WriteString(host)
	headers.WriteString("\r\n")
	requestedGzip := !t.ci.NoGzip && req.Header.Get("Accept-Encoding") == "" &&
		req.Header.Get("Range") == "" && req.Method != http.MethodHead
	if requestedGzip {
		headers.WriteString("Accept-Encoding:gzip\r\n")
	}
	if req.Close {
		headers.WriteString("Connection:close\r\n")
	}
	wireHeaders, rawURL := headers.String(), req.URL.String()
	var request C.uint64_t
	var errorBuffer [256]C.char
	// Client handles cannot be retired while request_start obtains its own owner.
	t.mu.Lock()
	client, err := t.clientHandleLocked()
	if err == nil {
		code := C.rclone_http_request_start(client, rustString(req.Method), C.size_t(len(req.Method)),
			rustString(rawURL), C.size_t(len(rawURL)), rustString(wireHeaders), C.size_t(len(wireHeaders)),
			&request, &errorBuffer[0], C.size_t(len(errorBuffer)))
		if code != C.RCLONE_HTTP_OK {
			err = rustError("request", code, &errorBuffer[0], req.Context())
		}
	}
	t.mu.Unlock()
	runtime.KeepAlive(t)
	if err != nil {
		return nil, err
	}
	bodyContext, cancelBody := context.WithCancel(req.Context())
	body := &rustResponseBody{request: request, ctx: bodyContext, cancel: cancelBody}
	body.stop = context.AfterFunc(bodyContext, func() { C.rclone_http_request_cancel(request) })
	headerBuffer := make([]byte, 4096)
	var status C.uint16_t
	var version C.uint8_t
	var length C.size_t
	for {
		code := C.rclone_http_response_headers(request, &status, &version,
			(*C.uint8_t)(unsafe.Pointer(&headerBuffer[0])), C.size_t(len(headerBuffer)), &length,
			&errorBuffer[0], C.size_t(len(errorBuffer)))
		if code == C.RCLONE_HTTP_BUFFER_TOO_SMALL && length <= 10<<20 {
			headerBuffer = make([]byte, int(length))
			continue
		}
		if code != C.RCLONE_HTTP_OK {
			_ = body.Close()
			return nil, rustError("response headers", code, &errorBuffer[0], req.Context())
		}
		break
	}
	protocol := "HTTP/1.1"
	if version == 1 {
		protocol = "HTTP/1.0"
	} else if version == 3 {
		protocol = "HTTP/2.0"
	}
	wireHead := protocol + " " + strconv.Itoa(int(status)) + " " + http.StatusText(int(status)) + "\r\n" +
		string(headerBuffer[:int(length)]) + "\r\n"
	resp, err := http.ReadResponse(bufio.NewReader(strings.NewReader(wireHead)), req)
	if err != nil {
		_ = body.Close()
		return nil, fmt.Errorf("Rust HTTP response headers: %w", err)
	}
	body.response = resp
	resp.Body = body
	if requestedGzip && strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		resp.Body = &rustGzipBody{ReadCloser: body}
		resp.Header.Del("Content-Encoding")
		resp.Header.Del("Content-Length")
		resp.ContentLength = -1
		resp.Uncompressed = true
	}
	return resp, nil
}

type rustHTTPError struct {
	phase   string
	message string
	code    C.int
}

func (e *rustHTTPError) Error() string   { return "Rust HTTP " + e.phase + ": " + e.message }
func (e *rustHTTPError) Timeout() bool   { return e.code == C.RCLONE_HTTP_TIMEOUT }
func (e *rustHTTPError) Temporary() bool { return e.Timeout() || e.code == C.RCLONE_HTTP_CONNECTION }

func rustError(phase string, code C.int, message *C.char, ctx context.Context) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if code == C.RCLONE_HTTP_CANCELED {
		return context.Canceled
	}
	return &rustHTTPError{phase: phase, code: code, message: C.GoString(message)}
}

type rustResponseBody struct {
	request     C.uint64_t
	ctx         context.Context
	cancel      context.CancelFunc
	stop        func() bool
	once        sync.Once
	response    *http.Response
	errorBuffer [256]C.char
	readLength  C.size_t
	eof         bool
}

var rustCopyBuffers = sync.Pool{New: func() any {
	buffer := make([]byte, 256<<10)
	return &buffer
}}

// WriteTo streams through a reusable buffer to amortize native-call overhead.
func (b *rustResponseBody) WriteTo(writer io.Writer) (int64, error) {
	buffer := rustCopyBuffers.Get().(*[]byte)
	defer rustCopyBuffers.Put(buffer)
	var total int64
	for {
		n, readErr := b.Read(*buffer)
		if n > 0 {
			written, writeErr := writer.Write((*buffer)[:n])
			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
			if written != n {
				return total, io.ErrShortWrite
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				readErr = nil
			}
			return total, readErr
		}
	}
}

func (b *rustResponseBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if b.eof {
		return 0, io.EOF
	}
	code := C.rclone_http_response_read(b.request, (*C.uint8_t)(unsafe.Pointer(&p[0])), C.size_t(len(p)),
		&b.readLength, &b.errorBuffer[0], C.size_t(len(b.errorBuffer)))
	if code != C.RCLONE_HTTP_OK {
		err := rustError("response body", code, &b.errorBuffer[0], b.ctx)
		_ = b.Close()
		return 0, err
	}
	if b.readLength > 0 {
		if err := accounting.TokenBucket.LimitBandwidthContext(b.ctx, accounting.TokenBucketSlotTransportRx, int(b.readLength)); err != nil {
			_ = b.Close()
			return 0, err
		}
		return int(b.readLength), nil
	}
	b.eof = true
	if err := b.readTrailers(); err != nil {
		_ = b.Close()
		return 0, err
	}
	_ = b.Close()
	return 0, io.EOF
}

func (b *rustResponseBody) readTrailers() error {
	buffer := make([]byte, 1024)
	var length C.size_t
	for {
		code := C.rclone_http_response_trailers(b.request, (*C.uint8_t)(unsafe.Pointer(&buffer[0])),
			C.size_t(len(buffer)), &length, &b.errorBuffer[0], C.size_t(len(b.errorBuffer)))
		if code == C.RCLONE_HTTP_BUFFER_TOO_SMALL && length <= 10<<20 {
			buffer = make([]byte, int(length))
			continue
		}
		if code != C.RCLONE_HTTP_OK {
			return rustError("response trailers", code, &b.errorBuffer[0], b.ctx)
		}
		break
	}
	if length != 0 {
		trailers, err := textproto.NewReader(bufio.NewReader(strings.NewReader(string(buffer[:int(length)]) + "\r\n"))).ReadMIMEHeader()
		if err != nil {
			return err
		}
		if b.response.Trailer == nil {
			b.response.Trailer = make(http.Header)
		}
		for name, values := range trailers {
			b.response.Trailer[name] = values
		}
	}
	return nil
}

func (b *rustResponseBody) Close() error {
	b.once.Do(func() {
		b.stop()
		b.cancel()
		C.rclone_http_request_cancel(b.request)
		C.rclone_http_request_free(b.request)
	})
	return nil
}

type rustGzipBody struct {
	io.ReadCloser
	reader *gzip.Reader
	err    error
}

func (b *rustGzipBody) Read(p []byte) (int, error) {
	if b.reader == nil && b.err == nil {
		b.reader, b.err = gzip.NewReader(b.ReadCloser)
	}
	if b.err != nil {
		return 0, b.err
	}
	return b.reader.Read(p)
}
