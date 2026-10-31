#ifndef RCLONE_HTTP_H
#define RCLONE_HTTP_H

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

enum {
    RCLONE_HTTP_OK = 0,
    RCLONE_HTTP_ERROR = 1,
    RCLONE_HTTP_CANCELED = 2,
    RCLONE_HTTP_INVALID = 3,
    RCLONE_HTTP_BUFFER_TOO_SMALL = 4,
    RCLONE_HTTP_TIMEOUT = 5,
    RCLONE_HTTP_TLS = 6,
    RCLONE_HTTP_CONNECTION = 7
};

/* Handles are process-local, nonzero IDs. Freeing a request cancels it;
 * concurrent cancel/free and an in-flight read/header wait are safe. Reads and
 * header waits on a request are serialized. Free each handle exactly once.
 * No caller buffers are retained after a function returns. Nonempty buffers
 * and output pointers must point to valid memory for the duration of the call.
 * Error buffers receive a truncated NUL-terminated message when capacity > 0.
 * connect_timeout_ms bounds TCP/TLS setup; idle_timeout_ms bounds socket
 * inactivity and each response stream's idle wait, independently of other
 * HTTP/2 streams. Response headers also have an absolute connect+idle timeout
 * budget, disabled when idle_timeout_ms is zero. HTTP/1 accepts at most 100
 * response header fields. Response header buffering is limited to 10 MiB.
 * HTTP/2 receive windows are fixed at 4 MiB per stream and 16 MiB per
 * connection, with 16 KiB maximum frames; consumers apply backpressure.
 */
int rclone_http_client_new(uint64_t connect_timeout_ms,
                           uint64_t idle_timeout_ms,
                           uint32_t max_idle_per_host,
                           uint8_t http1_only,
                           uint64_t *out_client,
                           char *error, size_t error_capacity);
void rclone_http_client_free(uint64_t client);

/* Headers are name:value\r\n lines with no final empty line. Duplicate header
 * values are retained. Only body-less GET and HEAD requests are accepted.
 * The URI must be absolute HTTP(S); its escaped path/query are not decoded.
 * Requests are sent once; redirects, decompression and retries belong to Go.
 */
int rclone_http_request_start(uint64_t client,
                              const uint8_t *method, size_t method_length,
                              const uint8_t *url, size_t url_length,
                              const uint8_t *headers, size_t headers_length,
                              uint64_t *out_request,
                              char *error, size_t error_capacity);

/* Blocks until headers arrive or cancellation/failure. Version is 1 for
 * HTTP/1.0, 2 for HTTP/1.1, 3 for HTTP/2. On BUFFER_TOO_SMALL, headers_length
 * receives the required capacity and this function may be called again.
 * Header output uses the same line format as request_start.
 */
int rclone_http_response_headers(uint64_t request,
                                 uint16_t *status, uint8_t *version,
                                 uint8_t *headers, size_t headers_capacity,
                                 size_t *headers_length,
                                 char *error, size_t error_capacity);

/* Returns at most capacity bytes; a successful zero-length read means EOF.
 * A nonempty buffer is required. At EOF, trailers can be read separately.
 */
int rclone_http_response_read(uint64_t request,
                              uint8_t *buffer, size_t capacity,
                              size_t *out_length,
                              char *error, size_t error_capacity);

/* Valid after EOF; the required-size/retry contract matches response_headers. */
int rclone_http_response_trailers(uint64_t request,
                                  uint8_t *headers, size_t headers_capacity,
                                  size_t *headers_length,
                                  char *error, size_t error_capacity);
void rclone_http_request_cancel(uint64_t request);
void rclone_http_request_free(uint64_t request);

#ifdef __cplusplus
}
#endif
#endif
