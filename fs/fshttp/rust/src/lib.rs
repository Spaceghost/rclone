//! Experimental, body-less HTTP transport with a length-delimited C interface.

use bytes::{Buf, Bytes};
use http_body_util::{BodyExt, Empty};
use hyper::body::{Body, Incoming};
use hyper::header::{HeaderMap, HeaderName, HeaderValue};
use hyper::{Method, Request, Response, Uri, Version};
use hyper_rustls::{HttpsConnector, HttpsConnectorBuilder};
use hyper_timeout::TimeoutConnector;
use hyper_util::client::legacy::{connect::HttpConnector, Client};
use hyper_util::rt::{TokioExecutor, TokioTimer};
use std::collections::HashMap;
use std::error::Error as StdError;
use std::ffi::{c_char, OsString};
use std::panic::{catch_unwind, AssertUnwindSafe};
use std::pin::Pin;
use std::ptr;
use std::slice;
use std::sync::{Arc, Mutex, OnceLock};
use std::task::Poll;
use std::time::Duration;
use tokio::runtime::Runtime;
use tokio::sync::Mutex as AsyncMutex;
use tokio::task::JoinHandle;
use tokio_util::sync::CancellationToken;

const OK: i32 = 0;
const ERROR: i32 = 1;
const CANCELED: i32 = 2;
const INVALID: i32 = 3;
const BUFFER_TOO_SMALL: i32 = 4;
const TIMEOUT: i32 = 5;
const TLS: i32 = 6;
const CONNECTION: i32 = 7;

type HttpClient = Client<TimeoutConnector<HttpsConnector<HttpConnector>>, Empty<Bytes>>;

#[derive(Clone, Debug)]
struct Error {
    code: i32,
    message: &'static str,
}

impl Error {
    fn invalid(message: &'static str) -> Self {
        Self {
            code: INVALID,
            message,
        }
    }

    fn canceled() -> Self {
        Self {
            code: CANCELED,
            message: "HTTP request canceled",
        }
    }
}

struct ClientState {
    client: HttpClient,
    header_budget: Option<Duration>,
    idle_timeout: Option<Duration>,
}

struct RequestState {
    cancel: CancellationToken,
    idle_timeout: Option<Duration>,
    state: AsyncMutex<ResponseState>,
}

#[derive(Clone)]
struct Head {
    status: u16,
    version: u8,
    headers: Vec<u8>,
}

struct ResponseState {
    pending: Option<JoinHandle<Result<Response<Incoming>, Error>>>,
    head: Option<Head>,
    body: Option<Incoming>,
    buffered: Bytes,
    trailers: Vec<u8>,
    eof: bool,
    failure: Option<Error>,
}

#[derive(Default)]
struct Registry {
    next: u64,
    clients: HashMap<u64, Arc<ClientState>>,
    requests: HashMap<u64, Arc<RequestState>>,
}

impl Registry {
    fn id(&mut self) -> Result<u64, Error> {
        self.next = self.next.checked_add(1).ok_or(Error {
            code: ERROR,
            message: "HTTP handle space exhausted",
        })?;
        Ok(self.next)
    }
}

fn block_sigpipe_on_runtime_thread() {
    // Native vectored socket writes can raise SIGPIPE. Blocking it only on
    // these persistent I/O threads preserves the embedding process's policy.
    #[cfg(unix)]
    unsafe {
        let mut signals = std::mem::zeroed::<libc::sigset_t>();
        libc::sigemptyset(&mut signals);
        libc::sigaddset(&mut signals, libc::SIGPIPE);
        libc::pthread_sigmask(libc::SIG_BLOCK, &signals, ptr::null_mut());
    }
}

fn runtime() -> Result<&'static Runtime, Error> {
    static RUNTIME: OnceLock<Result<Runtime, std::io::Error>> = OnceLock::new();
    RUNTIME
        .get_or_init(|| {
            tokio::runtime::Builder::new_multi_thread()
                .worker_threads(std::thread::available_parallelism().map_or(1, |count| count.get()))
                .thread_name("rclone-http")
                .on_thread_start(block_sigpipe_on_runtime_thread)
                .enable_all()
                .build()
        })
        .as_ref()
        .map_err(|_| Error {
            code: ERROR,
            message: "Could not start Rust HTTP runtime",
        })
}

fn registry() -> &'static Mutex<Registry> {
    static REGISTRY: OnceLock<Mutex<Registry>> = OnceLock::new();
    REGISTRY.get_or_init(|| Mutex::new(Registry::default()))
}

fn registry_lock() -> Result<std::sync::MutexGuard<'static, Registry>, Error> {
    registry().lock().map_err(|_| Error {
        code: ERROR,
        message: "HTTP handle registry unavailable",
    })
}

fn request_state(id: u64) -> Result<Arc<RequestState>, Error> {
    registry_lock()?
        .requests
        .get(&id)
        .cloned()
        .ok_or(Error::invalid("Invalid HTTP request handle"))
}

fn clear_canceled_request(state: &mut ResponseState) {
    if let Some(pending) = state.pending.take() {
        pending.abort();
    }
    state.body.take();
    state.buffered = Bytes::new();
    state.failure = Some(Error::canceled());
}

fn cancel_request(request: Arc<RequestState>) {
    request.cancel.cancel();
    let needs_cleanup = match request.state.try_lock() {
        Ok(mut state) => {
            clear_canceled_request(&mut state);
            false
        }
        Err(_) => true,
    };
    if needs_cleanup {
        // A header copy may hold the lock without awaiting the cancellation
        // token, so cleanup must also run after that owner releases the lock.
        if let Ok(runtime) = runtime() {
            runtime.spawn(async move {
                let mut state = request.state.lock().await;
                clear_canceled_request(&mut state);
            });
        }
    }
}

fn duration(milliseconds: u64) -> Option<Duration> {
    (milliseconds > 0).then(|| Duration::from_millis(milliseconds))
}

#[derive(PartialEq, Eq)]
struct RootEnvironment {
    file: Option<OsString>,
    directory: Option<OsString>,
}

fn native_tls_config() -> Result<rustls::ClientConfig, Error> {
    static TLS_CONFIG: OnceLock<Mutex<Option<(RootEnvironment, rustls::ClientConfig)>>> =
        OnceLock::new();
    let environment = RootEnvironment {
        file: std::env::var_os("SSL_CERT_FILE"),
        directory: std::env::var_os("SSL_CERT_DIR"),
    };
    let mut cached = TLS_CONFIG
        .get_or_init(|| Mutex::new(None))
        .lock()
        .map_err(|_| Error {
            code: TLS,
            message: "Native TLS configuration cache unavailable",
        })?;
    if let Some((source, config)) = cached.as_ref() {
        if *source == environment {
            return Ok(config.clone());
        }
    }
    let certificates = rustls_native_certs::load_native_certs();
    let mut roots = rustls::RootCertStore::empty();
    roots.add_parsable_certificates(certificates.certs);
    if roots.is_empty() {
        return Err(Error {
            code: TLS,
            message: "Could not load native TLS certificate roots",
        });
    }
    let config = rustls::ClientConfig::builder_with_provider(Arc::new(
        rustls::crypto::ring::default_provider(),
    ))
    .with_safe_default_protocol_versions()
    .map_err(|_| Error {
        code: TLS,
        message: "Could not configure safe TLS protocol versions",
    })?
    .with_root_certificates(roots)
    .with_no_client_auth();
    *cached = Some((environment, config.clone()));
    Ok(config)
}

fn classify(error: &(dyn StdError + 'static), body: bool) -> Error {
    let mut source = Some(error);
    while let Some(error) = source {
        if error
            .downcast_ref::<hyper::Error>()
            .is_some_and(|error| error.is_parse())
        {
            return Error {
                code: ERROR,
                message: "HTTP response headers are invalid or exceed the transport limit",
            };
        }
        if let Some(tls_error) = error.downcast_ref::<rustls::Error>() {
            let message = match tls_error {
                rustls::Error::InvalidCertificate(rustls::CertificateError::UnknownIssuer) => {
                    "TLS certificate issuer is not trusted"
                }
                rustls::Error::InvalidCertificate(_) => "TLS certificate verification failed",
                _ => "TLS handshake failed",
            };
            return Error { code: TLS, message };
        }
        if let Some(error) = error.downcast_ref::<std::io::Error>() {
            match error.kind() {
                std::io::ErrorKind::TimedOut => {
                    return Error {
                        code: TIMEOUT,
                        message: "HTTP socket inactivity timeout",
                    }
                }
                std::io::ErrorKind::ConnectionRefused => {
                    return Error {
                        code: CONNECTION,
                        message: "Server refused the HTTP connection",
                    }
                }
                std::io::ErrorKind::UnexpectedEof => {
                    return Error {
                        code: CONNECTION,
                        message: "Server closed the HTTP connection before completion",
                    }
                }
                std::io::ErrorKind::ConnectionReset
                | std::io::ErrorKind::ConnectionAborted
                | std::io::ErrorKind::BrokenPipe => {
                    return Error {
                        code: CONNECTION,
                        message: "HTTP connection was interrupted",
                    };
                }
                _ => (),
            }
        }
        source = error
            .downcast_ref::<std::io::Error>()
            .and_then(|error| {
                error
                    .get_ref()
                    .map(|inner| inner as &(dyn StdError + 'static))
            })
            .or_else(|| error.source());
    }
    if body {
        Error {
            code: CONNECTION,
            message: "HTTP response body failed before completion",
        }
    } else {
        Error {
            code: CONNECTION,
            message: "HTTP connection or response header exchange failed",
        }
    }
}

fn encode_headers(headers: &HeaderMap) -> Vec<u8> {
    let mut encoded = Vec::new();
    for (name, value) in headers {
        encoded.extend_from_slice(name.as_str().as_bytes());
        encoded.push(b':');
        encoded.extend_from_slice(value.as_bytes());
        encoded.extend_from_slice(b"\r\n");
    }
    encoded
}

fn parse_headers(encoded: &[u8]) -> Result<HeaderMap, Error> {
    let mut headers = HeaderMap::new();
    let mut rest = encoded;
    while !rest.is_empty() {
        let end = rest
            .windows(2)
            .position(|s| s == b"\r\n")
            .ok_or(Error::invalid("HTTP header line lacks CRLF terminator"))?;
        let line = &rest[..end];
        rest = &rest[end + 2..];
        let colon = line
            .iter()
            .position(|b| *b == b':')
            .ok_or(Error::invalid("HTTP header line lacks a colon"))?;
        let name = HeaderName::from_bytes(&line[..colon])
            .map_err(|_| Error::invalid("Invalid HTTP header name"))?;
        let value = HeaderValue::from_bytes(&line[colon + 1..])
            .map_err(|_| Error::invalid("Invalid HTTP header value"))?;
        headers.append(name, value);
    }
    Ok(headers)
}

async fn wait_headers(request: &RequestState, state: &mut ResponseState) -> Result<(), Error> {
    if request.cancel.is_cancelled() {
        return Err(Error::canceled());
    }
    if let Some(error) = &state.failure {
        return Err(error.clone());
    }
    if state.head.is_some() {
        return Ok(());
    }
    let pending = state
        .pending
        .as_mut()
        .ok_or(Error::invalid("HTTP request has no pending response"))?;
    let response = tokio::select! {
        biased;
        _ = request.cancel.cancelled() => Err(Error::canceled()),
        response = pending => response.unwrap_or(Err(Error { code: ERROR, message: "Rust HTTP request task failed" })),
    };
    state.pending.take();
    match response {
        Ok(response) => {
            let (parts, body) = response.into_parts();
            let version = match parts.version {
                Version::HTTP_10 => 1,
                Version::HTTP_11 => 2,
                Version::HTTP_2 => 3,
                _ => return Err(Error::invalid("Unsupported HTTP response version")),
            };
            state.head = Some(Head {
                status: parts.status.as_u16(),
                version,
                headers: encode_headers(&parts.headers),
            });
            state.body = Some(body);
            Ok(())
        }
        Err(error) => {
            state.failure = Some(error.clone());
            Err(error)
        }
    }
}

async fn read_response(request: &RequestState, output: &mut [u8]) -> Result<usize, Error> {
    let mut state = request.state.lock().await;
    wait_headers(request, &mut state).await?;
    let mut written = 0;
    let mut ready_frames = 0;
    let deadline = request
        .idle_timeout
        .map(|timeout| tokio::time::Instant::now() + timeout);
    loop {
        if request.cancel.is_cancelled() {
            state.body.take();
            return if written == 0 {
                Err(Error::canceled())
            } else {
                Ok(written)
            };
        }
        if !state.buffered.is_empty() {
            let count = (output.len() - written).min(state.buffered.len());
            output[written..written + count].copy_from_slice(&state.buffered[..count]);
            state.buffered.advance(count);
            written += count;
            if written == output.len() {
                return Ok(written);
            }
        }
        if state.eof {
            return Ok(written);
        }
        let body = state
            .body
            .as_mut()
            .ok_or(Error::invalid("HTTP response body unavailable"))?;
        let frame = if written > 0 {
            // Coalesce ready frames across the FFI boundary without waiting
            // for more network data after the first bytes become available.
            if ready_frames == 64 {
                return Ok(written);
            }
            ready_frames += 1;
            match std::future::poll_fn(|context| {
                Poll::Ready(Pin::new(&mut *body).poll_frame(context))
            })
            .await
            {
                Poll::Ready(frame) => Ok(frame),
                Poll::Pending => return Ok(written),
            }
        } else {
            tokio::select! {
                biased;
                _ = request.cancel.cancelled() => Err(Error::canceled()),
                frame = async {
                    if let Some(deadline) = deadline {
                        tokio::time::timeout_at(deadline, body.frame()).await.map_err(|_| Error {
                            code: TIMEOUT,
                            message: "HTTP response body stream idle timeout",
                        })
                    } else {
                        Ok(body.frame().await)
                    }
                } => frame,
            }
        };
        match frame {
            Ok(Some(Ok(frame))) => match frame.into_data() {
                Ok(data) => state.buffered = data,
                Err(frame) => {
                    if let Ok(trailers) = frame.into_trailers() {
                        state.trailers = encode_headers(&trailers);
                    }
                }
            },
            Ok(None) => {
                state.eof = true;
                state.body.take();
            }
            result => {
                let error = match result {
                    Err(error) => error,
                    Ok(Some(Err(error))) => classify(&error, true),
                    _ => unreachable!(),
                };
                state.failure = Some(error.clone());
                state.body.take();
                return if written == 0 {
                    Err(error)
                } else {
                    Ok(written)
                };
            }
        }
    }
}

unsafe fn input<'a>(pointer: *const u8, length: usize) -> Result<&'a [u8], Error> {
    if length == 0 {
        Ok(&[])
    } else if pointer.is_null() || length > isize::MAX as usize {
        Err(Error::invalid("Invalid HTTP input buffer"))
    } else {
        Ok(slice::from_raw_parts(pointer, length))
    }
}

unsafe fn output<'a>(pointer: *mut u8, length: usize) -> Result<&'a mut [u8], Error> {
    if length == 0 {
        Ok(&mut [])
    } else if pointer.is_null() || length > isize::MAX as usize {
        Err(Error::invalid("Invalid HTTP output buffer"))
    } else {
        Ok(slice::from_raw_parts_mut(pointer, length))
    }
}

unsafe fn set_output<T: Copy>(pointer: *mut T, value: T) -> Result<(), Error> {
    if pointer.is_null() {
        return Err(Error::invalid("Missing HTTP output pointer"));
    }
    pointer.write(value);
    Ok(())
}

unsafe fn copy_headers(
    encoded: &[u8],
    pointer: *mut u8,
    capacity: usize,
    length: *mut usize,
) -> Result<(), Error> {
    set_output(length, encoded.len())?;
    if encoded.len() > capacity {
        return Err(Error {
            code: BUFFER_TOO_SMALL,
            message: "HTTP header buffer too small",
        });
    }
    output(pointer, capacity)?[..encoded.len()].copy_from_slice(encoded);
    Ok(())
}

unsafe fn ffi<F>(error: *mut c_char, capacity: usize, call: F) -> i32
where
    F: FnOnce() -> Result<(), Error>,
{
    if capacity > 0 && !error.is_null() {
        error.write(0);
    }
    let result = catch_unwind(AssertUnwindSafe(call)).unwrap_or(Err(Error {
        code: ERROR,
        message: "Rust HTTP engine panicked",
    }));
    match result {
        Ok(()) => OK,
        Err(failure) => {
            if capacity > 0 && !error.is_null() {
                let length = failure.message.len().min(capacity - 1);
                ptr::copy_nonoverlapping(failure.message.as_ptr(), error.cast::<u8>(), length);
                error.add(length).write(0);
            }
            failure.code
        }
    }
}

/// Creates a pooled client using verified native TLS roots and one shared runtime.
///
/// # Safety
/// Output pointers and nonempty buffers must satisfy the C header contract.
#[no_mangle]
pub unsafe extern "C" fn rclone_http_client_new(
    connect_timeout_ms: u64,
    idle_timeout_ms: u64,
    max_idle_per_host: u32,
    http1_only: u8,
    out_client: *mut u64,
    error: *mut c_char,
    error_capacity: usize,
) -> i32 {
    ffi(error, error_capacity, || {
        set_output(out_client, 0)?;
        let runtime = runtime()?;
        let _entered = runtime.enter();
        let mut http = HttpConnector::new();
        http.enforce_http(false);
        http.set_nodelay(true);
        http.set_keepalive(Some(Duration::from_secs(30)));
        http.set_connect_timeout(duration(connect_timeout_ms));
        let tls = HttpsConnectorBuilder::new()
            .with_tls_config(native_tls_config()?)
            .https_or_http()
            .enable_http1();
        let connector = if http1_only != 0 {
            tls.wrap_connector(http)
        } else {
            tls.enable_http2().wrap_connector(http)
        };
        let mut connector = TimeoutConnector::new(connector);
        connector.set_connect_timeout(duration(connect_timeout_ms));
        connector.set_read_timeout(duration(idle_timeout_ms));
        connector.set_write_timeout(duration(idle_timeout_ms));
        connector.set_reset_reader_on_write(true);
        let mut builder = Client::builder(TokioExecutor::new());
        builder
            .pool_timer(TokioTimer::new())
            .pool_idle_timeout(Duration::from_secs(60))
            .pool_max_idle_per_host(max_idle_per_host as usize)
            .http1_max_buf_size(10 * 1024 * 1024)
            .http2_max_header_list_size(10 * 1024 * 1024)
            .http2_initial_stream_window_size(4 * 1024 * 1024)
            .http2_initial_connection_window_size(16 * 1024 * 1024)
            .http2_max_frame_size(16 * 1024)
            .http2_adaptive_window(false)
            .retry_canceled_requests(false);
        let client = Arc::new(ClientState {
            client: builder.build(connector),
            header_budget: (idle_timeout_ms > 0)
                .then(|| Duration::from_millis(connect_timeout_ms.saturating_add(idle_timeout_ms))),
            idle_timeout: duration(idle_timeout_ms),
        });
        let mut registry = registry_lock()?;
        let id = registry.id()?;
        registry.clients.insert(id, client);
        set_output(out_client, id)
    })
}

/// Releases a client handle; already-started requests retain their client.
#[no_mangle]
pub extern "C" fn rclone_http_client_free(client: u64) {
    let _ = catch_unwind(|| {
        let client = registry_lock()
            .ok()
            .and_then(|mut registry| registry.clients.remove(&client));
        drop(client);
    });
}

/// Copies and starts one GET/HEAD request, returning a cancellable handle.
///
/// # Safety
/// Output pointers and nonempty buffers must satisfy the C header contract.
#[no_mangle]
pub unsafe extern "C" fn rclone_http_request_start(
    client: u64,
    method: *const u8,
    method_length: usize,
    url: *const u8,
    url_length: usize,
    headers: *const u8,
    headers_length: usize,
    out_request: *mut u64,
    error: *mut c_char,
    error_capacity: usize,
) -> i32 {
    ffi(error, error_capacity, || {
        set_output(out_request, 0)?;
        let client = registry_lock()?
            .clients
            .get(&client)
            .cloned()
            .ok_or(Error::invalid("Invalid HTTP client handle"))?;
        let method = Method::from_bytes(input(method, method_length)?)
            .map_err(|_| Error::invalid("Invalid HTTP method"))?;
        if method != Method::GET && method != Method::HEAD {
            return Err(Error::invalid(
                "Rust transport supports only body-less GET and HEAD requests",
            ));
        }
        let uri = Uri::from_maybe_shared(input(url, url_length)?.to_vec())
            .map_err(|_| Error::invalid("Invalid absolute HTTP request URI"))?;
        if !matches!(uri.scheme_str(), Some("http" | "https")) || uri.authority().is_none() {
            return Err(Error::invalid(
                "HTTP request URI must use an absolute http or https URL",
            ));
        }
        let mut request = Request::new(Empty::<Bytes>::new());
        *request.method_mut() = method;
        *request.uri_mut() = uri;
        *request.headers_mut() = parse_headers(input(headers, headers_length)?)?;
        let cancel = CancellationToken::new();
        let request_cancel = cancel.clone();
        let header_budget = client.header_budget;
        let header_deadline = header_budget.map(|budget| tokio::time::Instant::now() + budget);
        let idle_timeout = client.idle_timeout;
        let pending = runtime()?.spawn(async move {
            let exchange = async {
                let response = client.client.request(request).await;
                response.map_err(|error| {
                    let mut failure = classify(&error, false);
                    if failure.code == TIMEOUT && error.is_connect() {
                        failure.message = "HTTP TCP or TLS connection timed out";
                    }
                    failure
                })
            };
            tokio::select! {
                biased;
                _ = request_cancel.cancelled() => Err(Error::canceled()),
                response = async {
                    if let Some(deadline) = header_deadline {
                        tokio::time::timeout_at(deadline, exchange).await.map_err(|_| Error {
                            code: TIMEOUT,
                            message: "HTTP response header budget exhausted (connect timeout plus header timeout)",
                        })?
                    } else {
                        exchange.await
                    }
                } => response,
            }
        });
        let state = Arc::new(RequestState {
            cancel,
            idle_timeout,
            state: AsyncMutex::new(ResponseState {
                pending: Some(pending),
                head: None,
                body: None,
                buffered: Bytes::new(),
                trailers: Vec::new(),
                eof: false,
                failure: None,
            }),
        });
        let mut registry = registry_lock()?;
        let id = registry.id()?;
        registry.requests.insert(id, state);
        set_output(out_request, id)
    })
}

/// Waits for response headers, which can be queried again with a larger buffer.
///
/// # Safety
/// Output pointers and nonempty buffers must satisfy the C header contract.
#[no_mangle]
pub unsafe extern "C" fn rclone_http_response_headers(
    request: u64,
    status: *mut u16,
    version: *mut u8,
    headers: *mut u8,
    headers_capacity: usize,
    headers_length: *mut usize,
    error: *mut c_char,
    error_capacity: usize,
) -> i32 {
    ffi(error, error_capacity, || {
        set_output(status, 0)?;
        set_output(version, 0)?;
        set_output(headers_length, 0)?;
        let request = request_state(request)?;
        runtime()?.block_on(async {
            let mut state = request.state.lock().await;
            wait_headers(&request, &mut state).await?;
            let head = state
                .head
                .as_ref()
                .ok_or(Error::invalid("HTTP response headers unavailable"))?;
            set_output(status, head.status)?;
            set_output(version, head.version)?;
            copy_headers(&head.headers, headers, headers_capacity, headers_length)
        })
    })
}

/// Reads a bounded part of the response, honoring cancellation and idle timeout.
///
/// # Safety
/// Output pointers and nonempty buffers must satisfy the C header contract.
#[no_mangle]
pub unsafe extern "C" fn rclone_http_response_read(
    request: u64,
    buffer: *mut u8,
    capacity: usize,
    out_length: *mut usize,
    error: *mut c_char,
    error_capacity: usize,
) -> i32 {
    ffi(error, error_capacity, || {
        set_output(out_length, 0)?;
        if capacity == 0 {
            return Err(Error::invalid(
                "HTTP read requires a nonempty output buffer",
            ));
        }
        let request = request_state(request)?;
        let count = runtime()?.block_on(read_response(&request, output(buffer, capacity)?))?;
        set_output(out_length, count)
    })
}

/// Returns trailing response headers after a successful EOF read.
///
/// # Safety
/// Output pointers and nonempty buffers must satisfy the C header contract.
#[no_mangle]
pub unsafe extern "C" fn rclone_http_response_trailers(
    request: u64,
    headers: *mut u8,
    headers_capacity: usize,
    headers_length: *mut usize,
    error: *mut c_char,
    error_capacity: usize,
) -> i32 {
    ffi(error, error_capacity, || {
        set_output(headers_length, 0)?;
        let request = request_state(request)?;
        runtime()?.block_on(async {
            let state = request.state.lock().await;
            if !state.eof {
                return Err(Error::invalid("HTTP trailers are available only after EOF"));
            }
            copy_headers(&state.trailers, headers, headers_capacity, headers_length)
        })
    })
}

/// Cancels a request without waiting for its header/read lock.
#[no_mangle]
pub extern "C" fn rclone_http_request_cancel(request: u64) {
    let _ = catch_unwind(|| {
        if let Ok(request) = request_state(request) {
            cancel_request(request);
        }
    });
}

/// Removes a handle and cancels any in-flight operation safely.
#[no_mangle]
pub extern "C" fn rclone_http_request_free(request: u64) {
    let _ = catch_unwind(|| {
        let request = registry_lock()
            .ok()
            .and_then(|mut registry| registry.requests.remove(&request));
        if let Some(request) = request {
            cancel_request(request);
        }
    });
}

#[cfg(test)]
mod tests {
    use super::*;

    #[cfg(unix)]
    #[test]
    fn runtime_blocks_sigpipe_without_changing_the_callers_mask() {
        fn sigpipe_blocked() -> bool {
            unsafe {
                let mut mask = std::mem::zeroed::<libc::sigset_t>();
                assert_eq!(
                    libc::pthread_sigmask(libc::SIG_BLOCK, ptr::null(), &mut mask),
                    0
                );
                libc::sigismember(&mask, libc::SIGPIPE) == 1
            }
        }
        let before = sigpipe_blocked();
        let worker_mask = runtime()
            .unwrap()
            .block_on(async { tokio::spawn(async { sigpipe_blocked() }).await.unwrap() });
        assert!(worker_mask);
        assert_eq!(sigpipe_blocked(), before);
    }

    #[test]
    fn cancellation_cleans_idle_and_busy_response_state() {
        let request = Arc::new(RequestState {
            cancel: CancellationToken::new(),
            idle_timeout: None,
            state: AsyncMutex::new(ResponseState {
                pending: None,
                head: None,
                body: None,
                buffered: Bytes::from_static(b"buffered response"),
                trailers: Vec::new(),
                eof: false,
                failure: None,
            }),
        });
        cancel_request(request.clone());
        assert!(request.cancel.is_cancelled());
        assert!(request.state.try_lock().unwrap().buffered.is_empty());

        runtime().unwrap().block_on(async {
            let mut guard = request.state.lock().await;
            guard.buffered = Bytes::from_static(b"concurrent header copy");
            cancel_request(request.clone());
            drop(guard);
            tokio::time::timeout(Duration::from_secs(1), async {
                loop {
                    if request.state.lock().await.buffered.is_empty() {
                        break;
                    }
                    tokio::task::yield_now().await;
                }
            })
            .await
            .unwrap();
        });
    }

    #[test]
    fn tls_errors_wrapped_in_io_errors_keep_their_category() {
        let error = std::io::Error::new(
            std::io::ErrorKind::InvalidData,
            rustls::Error::InvalidCertificate(rustls::CertificateError::UnknownIssuer),
        );
        assert_eq!(classify(&error, false).code, TLS);
    }

    #[test]
    fn headers_preserve_duplicates_and_non_utf8_values() {
        let encoded = b"x-test:first\r\nx-test:second\r\nx-byte:\xff\r\n";
        let headers = parse_headers(encoded).unwrap();
        assert_eq!(
            headers
                .get_all("x-test")
                .iter()
                .map(|v| v.as_bytes())
                .collect::<Vec<_>>(),
            vec![b"first".as_slice(), b"second".as_slice()]
        );
        assert_eq!(headers["x-byte"].as_bytes(), b"\xff");
        assert_eq!(encode_headers(&headers), encoded);
    }

    #[test]
    fn headers_reject_injection_and_missing_terminators() {
        assert!(parse_headers(b"x-test:one\ntwo\r\n").is_err());
        assert!(parse_headers(b"x-test:value").is_err());
        assert!(parse_headers(b"\r\n").is_err());
    }

    #[test]
    fn invalid_handles_and_small_buffers_return_errors() {
        unsafe {
            let mut status = 0;
            let mut version = 0;
            let mut length = 0;
            assert_eq!(
                rclone_http_response_headers(
                    u64::MAX,
                    &mut status,
                    &mut version,
                    ptr::null_mut(),
                    0,
                    &mut length,
                    ptr::null_mut(),
                    0
                ),
                INVALID
            );
            assert_eq!(
                copy_headers(b"x:v\r\n", ptr::null_mut(), 0, &mut length)
                    .unwrap_err()
                    .code,
                BUFFER_TOO_SMALL
            );
            assert_eq!(length, 5);
        }
    }
}
