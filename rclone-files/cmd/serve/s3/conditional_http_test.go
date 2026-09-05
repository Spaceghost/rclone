package s3

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/rclone/gofakes3"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/fs/object"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func conditionalRequest(handler http.Handler, method, target, body string, headers http.Header) *httptest.ResponseRecorder {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r := httptest.NewRequest(method, target, strings.NewReader(body)).WithContext(ctx)
	for k, values := range headers {
		r.Header[k] = append([]string(nil), values...)
	}
	r.Header.Set("Content-Length", strconv.Itoa(len(body)))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestS3ConditionalPut(t *testing.T) {
	for _, tc := range testRemotes {
		for _, hostStyle := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/HostStyle=%v", tc.name, hostStyle), func(t *testing.T) {
				b, _, bucket := newPutTestBackend(t, tc.backing, nil)
				s := b.s
				target := "/" + bucket + "/object"
				if hostStyle {
					gofakes3.WithHostBucket(true)(s.faker)
					s.handler = s.faker.Server()
					target = "http://" + bucket + ".localhost/object"
				}
				put := func(body string, h http.Header) *httptest.ResponseRecorder {
					return conditionalRequest(s.handler, http.MethodPut, target, body, h)
				}
				create := http.Header{"If-None-Match": {"*"}}
				require.Equal(t, 200, put("original", create).Code)
				head := conditionalRequest(s.handler, http.MethodHead, target, "", nil)
				require.Equal(t, 200, head.Code)
				etag := head.Header().Get("ETag")
				require.NotEmpty(t, strings.Trim(etag, `"`))
				failed := put("duplicate", create)
				assert.Equal(t, 412, failed.Code, failed.Body.String())
				assert.Contains(t, failed.Body.String(), "<Code>PreconditionFailed</Code>")
				assert.Equal(t, "original", conditionalRequest(s.handler, http.MethodGet, target, "", nil).Body.String())
				require.Equal(t, 200, put("replacement", http.Header{"If-Match": {etag}}).Code)
				assert.Equal(t, 412, put("lost update", http.Header{"If-Match": {etag}}).Code)
				assert.Equal(t, "replacement", conditionalRequest(s.handler, http.MethodGet, target, "", nil).Body.String())
				assert.Equal(t, 404, conditionalRequest(s.handler, http.MethodPut, target+"-missing", "absent", http.Header{"If-Match": {etag}}).Code)
				assert.Equal(t, 200, put("exists", http.Header{"If-Match": {"*"}}).Code)
				for _, h := range []http.Header{
					{"If-Match": {""}}, {"If-None-Match": {""}},
					{"If-Match": {etag, etag}}, {"If-None-Match": {"*", "*"}},
					{"If-Match": {"W/" + etag}}, {"If-None-Match": {etag}},
				} {
					assert.Equal(t, 400, put("invalid", h).Code)
				}
				assert.Equal(t, "exists", conditionalRequest(s.handler, http.MethodGet, target, "", nil).Body.String())
			})
		}
	}
}

func TestS3ConditionalConcurrent(t *testing.T) {
	for _, match := range []bool{false, true} {
		for _, tc := range testRemotes {
			t.Run(fmt.Sprintf("%s/Match=%v", tc.name, match), func(t *testing.T) {
				b, _, bucket := newPutTestBackend(t, tc.backing, nil)
				target := "/" + bucket + "/contended"
				h := http.Header{"If-None-Match": {"*"}}
				if match {
					require.Equal(t, 200, conditionalRequest(b.s.handler, http.MethodPut, target, "original", nil).Code)
					head := conditionalRequest(b.s.handler, http.MethodHead, target, "", nil)
					h = http.Header{"If-Match": {head.Header().Get("ETag")}}
				}
				const writers = 16
				type result struct {
					code        int
					body, value string
				}
				results := make(chan result, writers)
				start := make(chan struct{})
				for i := range writers {
					go func() {
						<-start
						value := fmt.Sprintf("writer-%d", i)
						w := conditionalRequest(b.s.handler, http.MethodPut, target, value, h)
						results <- result{w.Code, w.Body.String(), value}
					}()
				}
				close(start)
				wins, winner := 0, ""
				for range writers {
					r := <-results
					if r.code == 200 {
						wins++
						winner = r.value
					} else {
						assert.Equal(t, 412, r.code, r.body)
					}
				}
				require.Equal(t, 1, wins)
				assert.Equal(t, winner, conditionalRequest(b.s.handler, http.MethodGet, target, "", nil).Body.String())
			})
		}
	}
}

func TestS3ConditionalCached(t *testing.T) {
	for _, cm := range []vfscommon.CacheMode{vfscommon.CacheModeMinimal, vfscommon.CacheModeWrites, vfscommon.CacheModeFull} {
		t.Run(cm.String(), func(t *testing.T) {
			opt := vfscommon.Opt
			opt.CacheMode = cm
			opt.WriteBack = fs.Duration(time.Hour)
			b, f, bucket := newPutTestBackend(t, "", &opt)
			target := "/" + bucket + "/cached"
			_, err := f.Put(context.Background(), strings.NewReader("original"), object.NewStaticObjectInfo(bucket+"/cached", time.Now(), 8, true, nil, f))
			require.NoError(t, err)
			for i := range 3 {
				etag := conditionalRequest(b.s.handler, http.MethodHead, target, "", nil).Header().Get("ETag")
				h := http.Header{"If-Match": {etag}}
				value := fmt.Sprintf("replacement-%d", i)
				require.Equal(t, 200, conditionalRequest(b.s.handler, http.MethodPut, target, value, h).Code)
				assert.Equal(t, 412, conditionalRequest(b.s.handler, http.MethodPut, target, "stale", h).Code)
				assert.Equal(t, value, conditionalRequest(b.s.handler, http.MethodGet, target, "", nil).Body.String())
			}
		})
	}
}

func TestS3ConditionalUnavailable(t *testing.T) {
	b, f, bucket := newPutTestBackend(t, "", nil)
	target := "/" + bucket + "/object"
	require.Equal(t, 200, conditionalRequest(b.s.handler, http.MethodPut, target, "original", nil).Code)
	b.s.etagHashType = hash.None
	assert.Equal(t, 501, conditionalRequest(b.s.handler, http.MethodPut, target, "unsafe", http.Header{"If-Match": {`"etag"`}}).Code)
	assert.Equal(t, 412, conditionalRequest(b.s.handler, http.MethodPut, target, "duplicate", http.Header{"If-None-Match": {"*"}}).Code)
	assert.Equal(t, 200, conditionalRequest(b.s.handler, http.MethodPut, target+"-new", "created", http.Header{"If-None-Match": {"*"}}).Code)
	f.Features().Move = nil
	f.Features().Copy = nil
	assert.Equal(t, 501, conditionalRequest(b.s.handler, http.MethodPut, target+"-unsupported", "unsafe", http.Header{"If-None-Match": {"*"}}).Code)
	assert.Equal(t, "original", conditionalRequest(b.s.handler, http.MethodGet, target, "", nil).Body.String())
}

func TestS3ConditionalCopy(t *testing.T) {
	b, _, bucket := newPutTestBackend(t, "", nil)
	source, target := "/"+bucket+"/source", "/"+bucket+"/target"
	require.Equal(t, 200, conditionalRequest(b.s.handler, http.MethodPut, source, "source", nil).Code)
	h := http.Header{"X-Amz-Copy-Source": {source}, "If-None-Match": {"*"}}
	require.Equal(t, 200, conditionalRequest(b.s.handler, http.MethodPut, target, "", h).Code)
	assert.Equal(t, 412, conditionalRequest(b.s.handler, http.MethodPut, target, "", h).Code)
	h.Set("X-Amz-Copy-Source", target)
	assert.Equal(t, 412, conditionalRequest(b.s.handler, http.MethodPut, target, "", h).Code)
	h.Del("If-None-Match")
	h.Set("If-Match", conditionalRequest(b.s.handler, http.MethodHead, target, "", nil).Header().Get("ETag"))
	assert.Equal(t, 200, conditionalRequest(b.s.handler, http.MethodPut, target, "", h).Code)
	assert.Equal(t, "source", conditionalRequest(b.s.handler, http.MethodGet, target, "", nil).Body.String())
}

func TestS3ConditionalMultipart(t *testing.T) {
	for _, buffered := range []bool{false, true} {
		t.Run(fmt.Sprintf("Buffered=%v", buffered), func(t *testing.T) {
			b, f, bucket := newPutTestBackend(t, "", nil)
			b.s.opt.DisableMultipartStreaming = buffered
			target := "/" + bucket + "/multipart"
			init := conditionalRequest(b.s.handler, http.MethodPost, target+"?uploads", "", nil)
			require.Equal(t, 200, init.Code, init.Body.String())
			var upload struct {
				ID string `xml:"UploadId"`
			}
			require.NoError(t, xml.Unmarshal(init.Body.Bytes(), &upload))
			require.NotEmpty(t, upload.ID)
			query := "?uploadId=" + url.QueryEscape(upload.ID)
			part := conditionalRequest(b.s.handler, http.MethodPut, target+query+"&partNumber=1", "multipart body", nil)
			require.Equal(t, 200, part.Code, part.Body.String())
			body := "<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>" + part.Header().Get("ETag") + "</ETag></Part></CompleteMultipartUpload>"
			require.Equal(t, 200, conditionalRequest(b.s.handler, http.MethodPut, target, "other writer", nil).Code)
			failed := conditionalRequest(b.s.handler, http.MethodPost, target+query, body, http.Header{"If-None-Match": {"*"}})
			require.Equal(t, 412, failed.Code, failed.Body.String())
			assert.Equal(t, "other writer", conditionalRequest(b.s.handler, http.MethodGet, target, "", nil).Body.String())
			etag := conditionalRequest(b.s.handler, http.MethodHead, target, "", nil).Header().Get("ETag")
			completed := conditionalRequest(b.s.handler, http.MethodPost, target+query, body, http.Header{"If-Match": {etag}})
			require.Equal(t, 200, completed.Code, completed.Body.String())
			assert.Equal(t, "multipart body", conditionalRequest(b.s.handler, http.MethodGet, target, "", nil).Body.String())
			requireOnly(t, f, bucket, "multipart")
		})
	}
}

func TestS3ConditionalAuth(t *testing.T) {
	b, _, bucket := newPutTestBackend(t, "", nil)
	b.s.faker.AddAuthKeys(map[string]string{"access": "secret"})
	target := "http://localhost/" + bucket + "/object"
	request := func(secret string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPut, target, strings.NewReader("body"))
		r.Header.Set("Content-Length", "4")
		r.Header.Set("If-None-Match", "*")
		r.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
		require.NoError(t, v4.NewSigner().SignHTTP(context.Background(), aws.Credentials{AccessKeyID: "access", SecretAccessKey: secret}, r, "UNSIGNED-PAYLOAD", "s3", "us-east-1", time.Now()))
		w := httptest.NewRecorder()
		b.s.handler.ServeHTTP(w, r)
		return w
	}
	assert.Equal(t, 403, request("wrong").Code)
	assert.Equal(t, 200, request("secret").Code)
	assert.Equal(t, 403, request("wrong").Code)
	assert.Equal(t, 412, request("secret").Code)
}
