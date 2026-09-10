package s3

import (
	"context"
	"io"
	"sync"

	"github.com/rclone/gofakes3"
)

type objectLock struct {
	rw   sync.RWMutex
	refs int
}

type keyedRWMutex struct {
	mu    sync.Mutex
	locks map[string]*objectLock
}

func (m *keyedRWMutex) acquire(key string) *objectLock {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.locks == nil {
		m.locks = make(map[string]*objectLock)
	}
	lock := m.locks[key]
	if lock == nil {
		lock = new(objectLock)
		m.locks[key] = lock
	}
	lock.refs++
	return lock
}

func (m *keyedRWMutex) release(key string, lock *objectLock) {
	m.mu.Lock()
	defer m.mu.Unlock()
	lock.refs--
	if lock.refs == 0 {
		delete(m.locks, key)
	}
}

func (m *keyedRWMutex) read(key string) func() {
	lock := m.acquire(key)
	lock.rw.RLock()
	return func() {
		lock.rw.RUnlock()
		m.release(key, lock)
	}
}

func (m *keyedRWMutex) write(key string) func() {
	lock := m.acquire(key)
	lock.rw.Lock()
	return func() {
		lock.rw.Unlock()
		m.release(key, lock)
	}
}

type conditionalBackend struct {
	*s3Backend
	locks keyedRWMutex
	xfs   bool
}

func newConditionalBackend(backend *s3Backend) *conditionalBackend {
	b := &conditionalBackend{
		s3Backend: backend,
	}
	if v := backend.s.provider.VFS(); v != nil {
		b.xfs, _ = xfsCommitRangeVFSCapability(v)
	}
	return b
}

var _ gofakes3.Backend = (*conditionalBackend)(nil)
var _ gofakes3.MultipartBackend = (*conditionalBackend)(nil)

func objectLockKey(bucket, object string) string {
	return bucket + "\x00" + object
}

func (b *conditionalBackend) GetObject(ctx context.Context, bucketName, objectName string, rangeRequest *gofakes3.ObjectRangeRequest) (*gofakes3.Object, error) {
	if !b.xfs {
		return b.s3Backend.GetObject(ctx, bucketName, objectName, rangeRequest)
	}
	// A streamed GET must not straddle an in-place XFS exchange.
	unlock := b.locks.read(objectLockKey(bucketName, objectName))
	obj, err := b.s3Backend.GetObject(ctx, bucketName, objectName, rangeRequest)
	if err != nil {
		unlock()
		return nil, err
	}
	obj.Contents = &unlockReadCloser{ReadCloser: obj.Contents, unlock: unlock}
	return obj, nil
}

func (b *conditionalBackend) HeadObject(ctx context.Context, bucketName, objectName string) (*gofakes3.Object, error) {
	if !b.xfs {
		return b.s3Backend.HeadObject(ctx, bucketName, objectName)
	}
	unlock := b.locks.read(objectLockKey(bucketName, objectName))
	defer unlock()
	return b.s3Backend.HeadObject(ctx, bucketName, objectName)
}

func (b *conditionalBackend) DeleteObject(ctx context.Context, bucketName, objectName string) (gofakes3.ObjectDeleteResult, error) {
	unlock := b.locks.write(objectLockKey(bucketName, objectName))
	defer unlock()
	return b.s3Backend.DeleteObject(ctx, bucketName, objectName)
}

func (b *conditionalBackend) DeleteMulti(ctx context.Context, bucket string, objects ...string) (result gofakes3.MultiDeleteResult, err error) {
	// Multi-delete is not a transaction; hold only the current object's lock.
	for _, object := range objects {
		unlock := b.locks.write(objectLockKey(bucket, object))
		part, err := b.s3Backend.DeleteMulti(ctx, bucket, object)
		unlock()
		if err != nil {
			return result, err
		}
		result.Deleted = append(result.Deleted, part.Deleted...)
		result.Error = append(result.Error, part.Error...)
	}
	return result, nil
}

func (b *conditionalBackend) lockCopy(srcBucket, srcObject, dstBucket, dstObject string) func() {
	srcKey := objectLockKey(srcBucket, srcObject)
	dstKey := objectLockKey(dstBucket, dstObject)
	if srcKey == dstKey || !b.xfs {
		return b.locks.write(dstKey)
	}

	// Opposite-direction copies must acquire their locks in the same order.
	var first, second func()
	if srcKey < dstKey {
		first = b.locks.read(srcKey)
		second = b.locks.write(dstKey)
	} else {
		first = b.locks.write(dstKey)
		second = b.locks.read(srcKey)
	}
	return func() {
		second()
		first()
	}
}

type unlockReadCloser struct {
	io.ReadCloser
	unlock func()
	once   sync.Once
}

func (r *unlockReadCloser) Close() error {
	err := r.ReadCloser.Close()
	r.once.Do(r.unlock)
	return err
}
