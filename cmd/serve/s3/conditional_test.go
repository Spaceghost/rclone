package s3

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func conditionalContext(ifMatch, ifNoneMatch string) (context.Context, *writeConditions) {
	conditions := &writeConditions{ifMatch: ifMatch, ifNoneMatch: ifNoneMatch}
	return context.WithValue(context.Background(), writeConditionsKey{}, conditions), conditions
}

func objectETag(t *testing.T, b *s3Backend, bucket, object string) string {
	t.Helper()
	obj, err := b.HeadObject(context.Background(), bucket, object)
	require.NoError(t, err)
	defer func() { _ = obj.Contents.Close() }()
	return `"` + hex.EncodeToString(obj.Hash) + `"`
}

func TestEntityTagConditions(t *testing.T) {
	const etag = `"0123456789abcdef"`

	assert.True(t, ifMatchHolds("", false, ""))
	assert.True(t, ifMatchHolds("*", true, etag))
	assert.False(t, ifMatchHolds("*", false, ""))
	assert.True(t, ifMatchHolds(`"other", `+etag, true, etag))
	assert.False(t, ifMatchHolds(`W/`+etag, true, etag))
	assert.False(t, ifMatchHolds(`"other"`, true, etag))

	assert.True(t, ifNoneMatchHolds("", true, etag))
	assert.True(t, ifNoneMatchHolds("*", false, ""))
	assert.False(t, ifNoneMatchHolds("*", true, etag))
	assert.False(t, ifNoneMatchHolds(`W/`+etag, true, etag))
	assert.True(t, ifNoneMatchHolds(`"other"`, true, etag))
}

func TestConditionalPutObject(t *testing.T) {
	b, root := newTestBackend(t)
	cb := newConditionalBackend(b)
	oldETag := objectETag(t, b, "bucket", "object.txt")
	objectPath := filepath.Join(root, "bucket", "object.txt")

	t.Run("IfMatchSuccess", func(t *testing.T) {
		ctx, conditions := conditionalContext(oldETag, "")
		const body = "conditional replacement"
		_, err := cb.PutObject(ctx, "bucket", "object.txt", map[string]string{}, strings.NewReader(body), int64(len(body)))
		require.NoError(t, err)
		assert.False(t, conditions.failed)
		got, err := os.ReadFile(objectPath)
		require.NoError(t, err)
		assert.Equal(t, body, string(got))
	})

	t.Run("IfMatchStale", func(t *testing.T) {
		ctx, conditions := conditionalContext(oldETag, "")
		_, err := cb.PutObject(ctx, "bucket", "object.txt", map[string]string{}, strings.NewReader("must not win"), 12)
		assert.ErrorIs(t, err, errPreconditionFailed)
		assert.True(t, conditions.failed)
		got, readErr := os.ReadFile(objectPath)
		require.NoError(t, readErr)
		assert.Equal(t, "conditional replacement", string(got))
	})

	t.Run("IfNoneMatchCreate", func(t *testing.T) {
		ctx, conditions := conditionalContext("", "*")
		const body = "created once"
		_, err := cb.PutObject(ctx, "bucket", "new.txt", map[string]string{}, strings.NewReader(body), int64(len(body)))
		require.NoError(t, err)
		assert.False(t, conditions.failed)

		ctx, conditions = conditionalContext("", "*")
		_, err = cb.PutObject(ctx, "bucket", "new.txt", map[string]string{}, strings.NewReader("second"), 6)
		assert.ErrorIs(t, err, errPreconditionFailed)
		assert.True(t, conditions.failed)
	})
}

func TestConditionalPutRace(t *testing.T) {
	b, _ := newTestBackend(t)
	cb := newConditionalBackend(b)
	etag := objectETag(t, b, "bucket", "object.txt")

	start := make(chan struct{})
	results := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for _, body := range []string{"writer-one", "writer-two"} {
		go func() {
			ready.Done()
			<-start
			ctx, _ := conditionalContext(etag, "")
			_, err := cb.PutObject(ctx, "bucket", "object.txt", map[string]string{}, strings.NewReader(body), int64(len(body)))
			results <- err
		}()
	}
	ready.Wait()
	close(start)

	var successes, failures int
	for range 2 {
		err := <-results
		if err == nil {
			successes++
		} else {
			assert.ErrorIs(t, err, errPreconditionFailed)
			failures++
		}
	}
	assert.Equal(t, 1, successes)
	assert.Equal(t, 1, failures)
}

func TestXFSGetObjectBlocksReplacementUntilClose(t *testing.T) {
	b, _ := newTestBackend(t)
	cb := newConditionalBackend(b)
	cb.xfs = true
	obj, err := cb.GetObject(context.Background(), "bucket", "object.txt", nil)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		_, err := cb.PutObject(context.Background(), "bucket", "object.txt", map[string]string{}, strings.NewReader("replacement"), 11)
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("replacement completed while GET was still open: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	require.NoError(t, obj.Contents.Close())
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("replacement did not complete after GET closed")
	}
}
