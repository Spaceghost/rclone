package s3

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"syscall"

	"github.com/rclone/gofakes3"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/vfs"
)

func (b *conditionalBackend) checkConditions(ctx context.Context, bucket, object string, conditions *writeConditions) (exists bool, err error) {
	if conditions.invalid {
		return false, gofakes3.ErrInvalidArgument
	}
	var etag string
	obj, err := b.s3Backend.HeadObject(ctx, bucket, object)
	if err != nil {
		if !gofakes3.HasErrorCode(err, gofakes3.ErrNoSuchKey) {
			return false, err
		}
	} else {
		exists = true
		etag = `"` + hex.EncodeToString(obj.Hash) + `"`
		_ = obj.Contents.Close()
	}
	if !exists && conditions.ifMatch != "" {
		return false, gofakes3.KeyNotFound(object)
	}
	if !ifMatchHolds(conditions.ifMatch, exists, etag) || !ifNoneMatchHolds(conditions.ifNoneMatch, exists, etag) {
		return exists, conditions.fail()
	}
	return exists, nil
}

func (b *conditionalBackend) beginConditionalWrite(ctx context.Context, v *vfs.VFS, bucket, object string) (guard *xfsCommitGuard, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	conditions := conditionsFromContext(ctx)
	if conditions == nil {
		return nil, nil
	}
	if b.xfs {
		fp, err := bucketObjectPath(bucket, object)
		if err != nil {
			return nil, err
		}
		// Sample before checking the ETag, including when re-reading VFS metadata.
		guard, err = startXFSCommitGuard(v.Fs(), fp)
		// A read-only inode may still be replaceable by rename.
		if err != nil && !os.IsNotExist(err) && !os.IsPermission(err) && !xfsCommitUnavailable(err) {
			return nil, err
		}
		if guard != nil {
			b.forgetPath(v, fp)
		}
	}
	exists, err := b.checkConditions(ctx, bucket, object, conditions)
	if err != nil || !exists {
		_ = guard.close()
		guard = nil
	}
	conditions.guard = guard
	return guard, err
}

// publishObject uses the existing upload staging path for both publication methods.
func (b *s3Backend) publishObject(ctx context.Context, v *vfs.VFS, staging, target string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	conditions := conditionsFromContext(ctx)
	if conditions != nil && conditions.guard != nil {
		err := conditions.guard.commit(v.Fs(), staging, false)
		switch {
		case err == nil:
			// Staging contains the old contents after the exchange.
			b.forgetPath(v, target)
			b.forgetPath(v, staging)
			if err := v.Remove(staging); err != nil {
				fs.Errorf(staging, "Failed to remove XFS staging object: %v", err)
			}
			return nil
		case errors.Is(err, syscall.EBUSY):
			return conditions.conflict()
		case !xfsCommitUnavailable(err):
			return err
		}
		// START_COMMIT can succeed on filesystems without the exchange feature.
		// Keep the staged data and use the coordinated VFS publication path.
	}
	return v.Rename(staging, target)
}

func (b *conditionalBackend) PutObject(ctx context.Context, bucket, object string, meta map[string]string, input io.Reader, size int64) (result gofakes3.PutObjectResult, err error) {
	unlock := b.locks.write(objectLockKey(bucket, object))
	defer unlock()
	v, err := b.s.getVFS(ctx)
	if err != nil {
		return result, err
	}
	guard, err := b.beginConditionalWrite(ctx, v, bucket, object)
	if err != nil {
		return result, err
	}
	defer func() { _ = guard.close() }()
	return b.s3Backend.PutObject(ctx, bucket, object, meta, input, size)
}

func (b *conditionalBackend) CopyObject(ctx context.Context, srcBucket, srcKey, dstBucket, dstKey string, meta map[string]string) (result gofakes3.CopyObjectResult, err error) {
	unlock := b.lockCopy(srcBucket, srcKey, dstBucket, dstKey)
	defer unlock()
	v, err := b.s.getVFS(ctx)
	if err != nil {
		return result, err
	}
	guard, err := b.beginConditionalWrite(ctx, v, dstBucket, dstKey)
	if err != nil {
		return result, err
	}
	defer func() { _ = guard.close() }()
	return b.s3Backend.CopyObject(ctx, srcBucket, srcKey, dstBucket, dstKey, meta)
}

func (b *conditionalBackend) CompleteMultipartUpload(ctx context.Context, bucket, object string, uploadID gofakes3.UploadID, input *gofakes3.CompleteMultipartUploadRequest) (gofakes3.VersionID, string, error) {
	unlock := b.locks.write(objectLockKey(bucket, object))
	defer unlock()
	v, err := b.s.getVFS(ctx)
	if err != nil {
		return "", "", err
	}
	up, err := b.loadUpload(uploadID)
	if err != nil {
		return "", "", err
	}
	if up.bucket != bucket || up.key != object || up.vfs != v {
		return "", "", gofakes3.ErrNoSuchUpload
	}
	guard, err := b.beginConditionalWrite(ctx, v, bucket, object)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = guard.close() }()
	return b.s3Backend.CompleteMultipartUpload(ctx, bucket, object, uploadID, input)
}
