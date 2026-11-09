package s3

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"vornik.io/vornik/internal/artifacts"
)

// TestBackend_Get_EmptyKey — the resolveKey-error branch is uncovered
// by existing tests for Get (only Put exercises it). Pin the empty-key
// path so a future change to Get's argument validation surfaces.
func TestBackend_Get_EmptyKey(t *testing.T) {
	b, _ := testBackend(t, "")
	if _, err := b.Get(context.Background(), ""); err == nil {
		t.Error("Get(\"\") expected error, got nil")
	}
}

// TestBackend_Delete_EmptyKey — same pattern, Delete's resolveKey
// error branch.
func TestBackend_Delete_EmptyKey(t *testing.T) {
	b, _ := testBackend(t, "")
	if err := b.Delete(context.Background(), ""); err == nil {
		t.Error("Delete(\"\") expected error, got nil")
	}
}

// TestBackend_Exists_EmptyKey — Exists' resolveKey error branch.
func TestBackend_Exists_EmptyKey(t *testing.T) {
	b, _ := testBackend(t, "")
	if _, err := b.Exists(context.Background(), ""); err == nil {
		t.Error("Exists(\"\") expected error, got nil")
	}
}

// TestBackend_Stat_EmptyKey — Stat's resolveKey error branch.
func TestBackend_Stat_EmptyKey(t *testing.T) {
	b, _ := testBackend(t, "")
	if _, err := b.Stat(context.Background(), ""); err == nil {
		t.Error("Stat(\"\") expected error, got nil")
	}
}

// TestBackend_Put_BodyReadFailure — io.ReadAll on a Reader that
// errors mid-stream surfaces as a wrapped "read source" error rather
// than panicking. This is the only branch in Put that the existing
// tests miss because they all pass bytes.NewReader which never errors.
func TestBackend_Put_BodyReadFailure(t *testing.T) {
	b, _ := testBackend(t, "")
	rdr := &erroringReader{err: errors.New("source boom"), data: []byte("partial")}
	_, err := b.Put(context.Background(), "k.txt", rdr)
	if err == nil {
		t.Fatal("Put with erroring reader returned nil error")
	}
	if !errorMessageContains(err, "read source") {
		t.Errorf("Put err = %v, want one mentioning 'read source'", err)
	}
}

// GitHub #443: a server that answers IsTruncated=true with no
// NextContinuationToken and ignores StartAfter would repeat its first page
// forever. List used to return success after that page, so
// DeleteProjectArtifacts reported a complete delete and orphaned the rest.
// It must fail instead, after the one StartAfter retry that made no progress.
func TestBackend_List_TruncatedNoTokenNoProgressFails_443(t *testing.T) {
	client := &truncatedNoTokenStub{}
	b := newWithClient(Options{Region: "us-east-1", Bucket: "bucket"}, client)
	err := b.List(context.Background(), "", func(artifacts.ObjectInfo) error { return nil })
	if err == nil {
		t.Fatal("a listing that cannot continue must return an error, not success")
	}
	if client.calls != 2 {
		t.Errorf("ListObjectsV2 calls = %d, want 2 (one StartAfter retry, then stop)", client.calls)
	}
}

// GitHub #443: the fallback the code comment promised. A server that never
// sends a token still lists every key, continuing from each page's last key.
func TestBackend_List_TruncatedNoTokenFallsBackToStartAfter_443(t *testing.T) {
	keys := map[string]string{}
	for i := 0; i < 2500; i++ {
		keys[fmtSprintf("p/k%05d", i)] = "x"
	}
	b, stub := seedList(t, "", keys)
	stub.pageSize = 1000
	stub.omitToken = true
	n := 0
	if err := b.List(context.Background(), "p", func(artifacts.ObjectInfo) error { n++; return nil }); err != nil {
		t.Fatalf("List: %v", err)
	}
	if n != 2500 || stub.listCalls != 3 {
		t.Fatalf("listed %d keys in %d calls, want 2500 in 3", n, stub.listCalls)
	}
}

// errorMessageContains is a tiny helper so the assertions don't need
// to import strings just for one check.
func errorMessageContains(err error, sub string) bool {
	return err != nil && bytes.Contains([]byte(err.Error()), []byte(sub))
}

// erroringReader yields `data` once then returns `err`. Used by the
// body-read-failure test above to drive Put's io.ReadAll error branch.
type erroringReader struct {
	data []byte
	err  error
	done bool
}

func (r *erroringReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, r.err
	}
	r.done = true
	n := copy(p, r.data)
	return n, r.err
}

var _ io.Reader = (*erroringReader)(nil)

// truncatedNoTokenStub returns IsTruncated=true with no
// NextContinuationToken on the first ListObjectsV2 call so we can
// exercise the Backend's "stop on misbehaving server" fallback in
// List. The remaining s3API methods aren't exercised by the test and
// are kept as no-ops returning ErrNotImplemented so a misuse surfaces
// loudly.
type truncatedNoTokenStub struct {
	calls int
}

func (s *truncatedNoTokenStub) PutObject(context.Context, *awss3.PutObjectInput, ...func(*awss3.Options)) (*awss3.PutObjectOutput, error) {
	return nil, ErrNotImplemented
}

func (s *truncatedNoTokenStub) GetObject(context.Context, *awss3.GetObjectInput, ...func(*awss3.Options)) (*awss3.GetObjectOutput, error) {
	return nil, ErrNotImplemented
}

func (s *truncatedNoTokenStub) DeleteObject(context.Context, *awss3.DeleteObjectInput, ...func(*awss3.Options)) (*awss3.DeleteObjectOutput, error) {
	return nil, ErrNotImplemented
}

func (s *truncatedNoTokenStub) HeadObject(context.Context, *awss3.HeadObjectInput, ...func(*awss3.Options)) (*awss3.HeadObjectOutput, error) {
	return nil, ErrNotImplemented
}

func (s *truncatedNoTokenStub) ListObjectsV2(ctx context.Context, in *awss3.ListObjectsV2Input, opts ...func(*awss3.Options)) (*awss3.ListObjectsV2Output, error) {
	s.calls++
	truncated := true
	sz := int64(1)
	etag := `"x"`
	key := "k/a"
	return &awss3.ListObjectsV2Output{
		Contents:    []s3types.Object{{Key: &key, Size: &sz, ETag: &etag}},
		IsTruncated: &truncated,
		// Deliberately omit NextContinuationToken.
	}, nil
}

// keep aws.String reachable from this file for future tests.
var _ = aws.String
