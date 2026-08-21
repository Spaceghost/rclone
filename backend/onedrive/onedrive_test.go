// Test OneDrive filesystem interface
package onedrive

import (
	"context"
	"testing"

	"github.com/rclone/rclone/backend/onedrive/api"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fstest"
	"github.com/rclone/rclone/fstest/fstests"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewUpdateOpts(t *testing.T) {
	o := &Object{fs: &Fs{opt: Options{Region: regionGlobal}}}
	require.NoError(t, o.setMetaData(&api.Item{
		ID:              "item",
		ETag:            `"etag"`,
		File:            &api.FileFacet{},
		ParentReference: &api.ItemReference{DriveID: "drive"},
	}))
	opts := o.newUpdateOpts(context.Background(), "POST", "/createUploadSession")
	assert.Equal(t, graphAPIEndpoint[regionGlobal]+"/v1.0/drives", opts.RootURL)
	assert.Equal(t, "/drive/items/item/createUploadSession", opts.Path)
	assert.Equal(t, `"etag"`, opts.ExtraHeaders["If-Match"])
}

// TestIntegration runs integration tests against the remote
func TestIntegration(t *testing.T) {
	fstests.Run(t, &fstests.Opt{
		RemoteName: "TestOneDrive:",
		NilObject:  (*Object)(nil),
		ChunkedUpload: fstests.ChunkedUploadConfig{
			CeilChunkSize: fstests.NextMultipleOf(chunkSizeMultiple),
		},
	})
}

// TestIntegrationCn runs integration tests against the remote
func TestIntegrationCn(t *testing.T) {
	if *fstest.RemoteName != "" {
		t.Skip("skipping as -remote is set")
	}
	fstests.Run(t, &fstests.Opt{
		RemoteName: "TestOneDriveCn:",
		NilObject:  (*Object)(nil),
		ChunkedUpload: fstests.ChunkedUploadConfig{
			CeilChunkSize: fstests.NextMultipleOf(chunkSizeMultiple),
		},
	})
}

func (f *Fs) SetUploadChunkSize(cs fs.SizeSuffix) (fs.SizeSuffix, error) {
	return f.setUploadChunkSize(cs)
}

var _ fstests.SetUploadChunkSizer = (*Fs)(nil)
