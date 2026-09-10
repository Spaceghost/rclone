package s3

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConditionalRejectedKeys(t *testing.T) {
	for _, key := range []string{"./object.txt", "dir/../object.txt", "dir//object.txt", "../root-secret.txt"} {
		t.Run(key, func(t *testing.T) {
			b, _ := newTestBackend(t)
			cb := newConditionalBackend(b)
			etag := objectETag(t, b, "bucket", "object.txt")
			ctx, _ := conditionalContext("", "*")
			_, err := cb.PutObject(ctx, "bucket", key, map[string]string{}, strings.NewReader("replacement"), 11)
			require.Error(t, err)
			assert.Equal(t, etag, objectETag(t, b, "bucket", "object.txt"))
			assert.Empty(t, cb.locks.locks)
		})
	}
}

func TestConditionalReadLockCleanup(t *testing.T) {
	for _, method := range []string{"Get", "Head"} {
		t.Run(method, func(t *testing.T) {
			b, _ := newTestBackend(t)
			cb := newConditionalBackend(b)
			cb.xfs = true
			if method == "Get" {
				_, err := cb.GetObject(context.Background(), "bucket", "missing", nil)
				require.Error(t, err)
			} else {
				_, err := cb.HeadObject(context.Background(), "bucket", "missing")
				require.Error(t, err)
			}
			assert.Empty(t, cb.locks.locks)
		})
	}
}

func TestConditionalCopyLockOrder(t *testing.T) {
	b, _ := newTestBackend(t)
	cb := newConditionalBackend(b)
	cb.xfs = true
	ctx := context.Background()
	for _, key := range []string{"first", "second"} {
		_, err := cb.PutObject(ctx, "bucket", key, map[string]string{}, strings.NewReader(key), int64(len(key)))
		require.NoError(t, err)
	}
	start := make(chan struct{})
	done := make(chan error, 2)
	for _, pair := range [][2]string{{"first", "second"}, {"second", "first"}} {
		go func() {
			<-start
			_, err := cb.CopyObject(ctx, "bucket", pair[0], "bucket", pair[1], map[string]string{})
			done <- err
		}()
	}
	close(start)
	for range 2 {
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("opposite-direction copies deadlocked")
		}
	}
	assert.Equal(t, objectETag(t, b, "bucket", "first"), objectETag(t, b, "bucket", "second"))
	assert.Empty(t, cb.locks.locks)
}

func TestConditionalIndependentKeys(t *testing.T) {
	b, _ := newTestBackend(t)
	cb := newConditionalBackend(b)
	cb.xfs = true
	obj, err := cb.GetObject(context.Background(), "bucket", "object.txt", nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = obj.Contents.Close() })
	done := make(chan error, 1)
	go func() {
		_, err := cb.PutObject(context.Background(), "bucket", "other", map[string]string{}, strings.NewReader("new"), 3)
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("a read blocked a different object")
	}
	got, err := io.ReadAll(obj.Contents)
	require.NoError(t, err)
	assert.Equal(t, "normal object", string(got))
	require.NoError(t, obj.Contents.Close())
	assert.Empty(t, cb.locks.locks)
}
