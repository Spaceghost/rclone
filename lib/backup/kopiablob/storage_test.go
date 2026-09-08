package kopiablob

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"

	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/filesystem"
	"github.com/rclone/rclone/fs"
)

type testBytes []byte

type seekCloser struct{ *bytes.Reader }

func (seekCloser) Close() error { return nil }
func (b testBytes) Length() int { return len(b) }
func (b testBytes) Reader() io.ReadSeekCloser {
	return seekCloser{bytes.NewReader(b)}
}
func (b testBytes) WriteTo(w io.Writer) (int64, error) { return bytes.NewReader(b).WriteTo(w) }

type outputBuffer struct{ bytes.Buffer }

func (b *outputBuffer) Length() int { return b.Len() }

func newTestStorage(t *testing.T) (*Storage, string) {
	t.Helper()
	dir := t.TempDir()
	f, err := fs.NewFs(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := FromFs(f, Options{Remote: dir}, true)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func readBlob(t *testing.T, s blob.Storage, id blob.ID) []byte {
	t.Helper()
	var out outputBuffer
	if err := s.GetBlob(context.Background(), id, 0, -1, &out); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestRangesAndErrors(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStorage(t)
	if err := s.PutBlob(ctx, "p0123456789", testBytes("0123456789"), blob.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name           string
		offset, length int64
		want           string
		bad            bool
	}{
		{"whole", 0, -1, "0123456789", false},
		{"whole-ignores-offset", 9, -1, "0123456789", false},
		{"first", 0, 1, "0", false},
		{"middle", 3, 4, "3456", false},
		{"last", 9, 1, "9", false},
		{"empty-at-end", 10, 0, "", false},
		{"past-end", 11, 0, "", true},
		{"negative-offset", -1, 1, "", true},
		{"too-long", 9, 2, "", true},
		{"overflow", math.MaxInt64, math.MaxInt64, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out outputBuffer
			out.WriteString("stale output must be cleared")
			err := s.GetBlob(ctx, "p0123456789", tc.offset, tc.length, &out)
			if tc.bad {
				if !errors.Is(err, blob.ErrInvalidRange) || out.Len() != 0 {
					t.Fatalf("got error=%v output=%q", err, out.String())
				}
				return
			}
			if err != nil || out.String() != tc.want {
				t.Fatalf("got %q, %v; want %q", out.String(), err, tc.want)
			}
		})
	}
	var out outputBuffer
	out.WriteString("stale")
	if err := s.GetBlob(ctx, "missing", 0, -1, &out); !errors.Is(err, blob.ErrBlobNotFound) || out.Len() != 0 {
		t.Fatalf("missing blob: %v, %q", err, out.String())
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.GetBlob(cancelled, "p0123456789", 0, -1, &out); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read: %v", err)
	}
}

func TestStockKopiaStorageCompatibility(t *testing.T) {
	ctx := context.Background()
	s, dir := newTestStorage(t)
	for _, id := range []blob.ID{"p0123456789", "p0123499999", "n1234567890", "kopia.repository"} {
		if err := s.PutBlob(ctx, id, testBytes("content:"+id), blob.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	// A fresh stock provider must discover the exact layout without local cache.
	stock, err := filesystem.New(ctx, &filesystem.Options{Path: dir}, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stock.Close(ctx) })
	if got := string(readBlob(t, stock, "p0123456789")); got != "content:p0123456789" {
		t.Fatalf("stock provider read %q", got)
	}
	if err := stock.PutBlob(ctx, "pabcdef1234", testBytes("from-stock"), blob.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	fresh, err := New(ctx, &Options{Remote: dir, ReadOnly: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(readBlob(t, fresh, "pabcdef1234")); got != "from-stock" {
		t.Fatalf("native provider read %q", got)
	}
	var ids []string
	if err := fresh.ListBlobs(ctx, "p01234", func(md blob.Metadata) error {
		ids = append(ids, string(md.BlobID))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(ids)
	if !reflect.DeepEqual(ids, []string{"p0123456789", "p0123499999"}) {
		t.Fatalf("prefix listing: %v", ids)
	}
	stop := errors.New("stop listing")
	if err := fresh.ListBlobs(ctx, "", func(blob.Metadata) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("callback error lost: %v", err)
	}
}

func TestReadOnlyNeverInitializesStorage(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "absent")
	s, err := New(ctx, &Options{Remote: dir, ReadOnly: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	var out outputBuffer
	_ = s.GetBlob(ctx, "missing", 0, -1, &out)
	if err := s.PutBlob(ctx, "p1234567890", testBytes("no"), blob.PutOptions{}); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("read-only put: %v", err)
	}
	if err := s.DeleteBlob(ctx, "p1234567890"); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("read-only delete: %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only access created storage: %v", err)
	}
}

type unknownFs struct{ fs.Fs }

func TestUnqualifiedWritesAndUnsupportedOptions(t *testing.T) {
	s, _ := newTestStorage(t)
	if _, err := FromFs(unknownFs{s.f}, Options{}, false); err == nil {
		t.Fatal("accepted an unqualified backend with inherited Move")
	}
	if _, err := FromFs(s.f, Options{ReadOnly: true}, true); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("read-only create: %v", err)
	}
	if err := s.PutBlob(context.Background(), "p1234567890", testBytes("no"), blob.PutOptions{DoNotRecreate: true}); !errors.Is(err, blob.ErrUnsupportedPutBlobOption) {
		t.Fatalf("conditional create was silently weakened: %v", err)
	}
}

type truncatedBytes struct{ testBytes }

func (b truncatedBytes) Length() int { return len(b.testBytes) + 100 }

func TestFailedReplacementPreservesCommittedBlob(t *testing.T) {
	ctx := context.Background()
	s, dir := newTestStorage(t)
	id := blob.ID("p1234567890")
	if err := s.PutBlob(ctx, id, testBytes("committed"), blob.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutBlob(ctx, id, truncatedBytes{testBytes("short")}, blob.PutOptions{}); err == nil {
		t.Fatal("accepted a short upload")
	}
	if got := string(readBlob(t, s, id)); got != "committed" {
		t.Fatalf("failed write replaced committed data: %q", got)
	}
	if err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && filepath.Ext(p) == ".tmp" {
			t.Errorf("leaked staging object: %s", p)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestParallelWritesAndIdempotentDelete(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStorage(t)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := blob.ID("p1234567890" + string(rune('a'+i)))
			if err := s.PutBlob(ctx, id, testBytes(bytes.Repeat([]byte{byte(i)}, 8192)), blob.PutOptions{}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	for i := 0; i < 12; i++ {
		id := blob.ID("p1234567890" + string(rune('a'+i)))
		if got := readBlob(t, s, id); !bytes.Equal(got, bytes.Repeat([]byte{byte(i)}, 8192)) {
			t.Fatalf("wrong parallel content for %s", id)
		}
		for j := 0; j < 2; j++ {
			if err := s.DeleteBlob(ctx, id); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestRejectTraversal(t *testing.T) {
	s, _ := newTestStorage(t)
	for _, id := range []blob.ID{"", ".", "..", "../outside", "/absolute", "a/b", "a\\b", "a\x00b"} {
		if err := s.PutBlob(context.Background(), id, testBytes("no"), blob.PutOptions{}); err == nil {
			t.Errorf("accepted blob ID %q", id)
		}
	}
	for _, p := range []string{"../outside", "x/../../outside", "/absolute", "a\\b", "a\x00b"} {
		if _, err := remotePath(p); err == nil {
			t.Errorf("accepted path %q", p)
		}
	}
}
