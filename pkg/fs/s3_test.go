package fs

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/client/metadata"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3iface"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
)

func TestS3_Create(t *testing.T) {
	files := make(map[string][]byte)
	stor, err := newMockS3(files, "")
	assert.NoError(t, err)

	written, err := stor.Create(testCtx, "1/test", bytes.NewBuffer([]byte{1, 5, 7, 8, 3}))
	assert.NoError(t, err)
	assert.EqualValues(t, 5, written)

	d, ok := files["1/test"]
	assert.True(t, ok)
	assert.EqualValues(t, 5, len(d))
}

func TestS3_Size(t *testing.T) {
	files := make(map[string][]byte)
	stor, err := newMockS3(files, "")
	assert.NoError(t, err)

	_, err = stor.Create(testCtx, "1/test", bytes.NewBuffer([]byte{1, 5, 7, 8, 3}))
	assert.NoError(t, err)

	sz, err := stor.Size(testCtx, "1/test")
	assert.NoError(t, err)
	assert.EqualValues(t, 5, sz)
}

func TestS3_NoSize(t *testing.T) {
	files := make(map[string][]byte)
	stor, err := newMockS3(files, "")
	assert.NoError(t, err)

	_, err = stor.Size(testCtx, "1/test")
	assert.True(t, os.IsNotExist(err))
}

func TestS3_Delete(t *testing.T) {
	files := make(map[string][]byte)
	stor, err := newMockS3(files, "")
	assert.NoError(t, err)

	_, err = stor.Create(testCtx, "1/test", bytes.NewBuffer([]byte{1, 5, 7, 8, 3}))
	assert.NoError(t, err)

	err = stor.Delete(testCtx, "1/test")
	assert.NoError(t, err)

	_, err = stor.Size(testCtx, "1/test")
	assert.True(t, errors.Is(err, os.ErrNotExist))

	_, ok := files["1/test"]
	assert.False(t, ok)

	err = stor.Delete(testCtx, "1/test")
	assert.True(t, errors.Is(err, os.ErrNotExist))
}

func TestS3_BuildKey(t *testing.T) {
	files := make(map[string][]byte)

	stor, _ := newMockS3(files, "")
	key := stor.buildKey("test-fn")
	assert.EqualValues(t, "test-fn", key)

	stor, _ = newMockS3(files, "mock-prefix")
	key = stor.buildKey("test-fn")
	assert.EqualValues(t, "mock-prefix/test-fn", key)
}

type mockS3API struct {
	s3iface.S3API
	files map[string][]byte
}

func newMockS3(files map[string][]byte, prefix string) (*S3, error) {
	api := &mockS3API{files: files}
	return &S3{
		api:      api,
		uploader: s3manager.NewUploaderWithClient(api),
		bucket:   "mock-bucket",
		prefix:   prefix,
	}, nil
}

func (m *mockS3API) PutObjectRequest(input *s3.PutObjectInput) (*request.Request, *s3.PutObjectOutput) {
	content, _ := io.ReadAll(input.Body)
	req := request.New(aws.Config{}, metadata.ClientInfo{}, request.Handlers{}, nil, &request.Operation{}, nil, nil)
	m.files[*input.Key] = content
	return req, &s3.PutObjectOutput{}
}

func (m *mockS3API) HeadObjectWithContext(ctx aws.Context, input *s3.HeadObjectInput, opts ...request.Option) (*s3.HeadObjectOutput, error) {
	if _, ok := m.files[*input.Key]; ok {
		return &s3.HeadObjectOutput{ContentLength: aws.Int64(int64(len(m.files[*input.Key])))}, nil
	}
	return nil, awserr.New("NotFound", "", nil)
}

func (m *mockS3API) DeleteObjectWithContext(ctx aws.Context, input *s3.DeleteObjectInput, opts ...request.Option) (*s3.DeleteObjectOutput, error) {
	if _, ok := m.files[*input.Key]; ok {
		delete(m.files, *input.Key)
		return &s3.DeleteObjectOutput{}, nil
	}
	return nil, awserr.New("NotFound", "", nil)
}

type artworkListAPI struct {
	s3iface.S3API
	prefixes []string
}

func (m *artworkListAPI) ListObjectsV2PagesWithContext(ctx aws.Context, input *s3.ListObjectsV2Input, fn func(*s3.ListObjectsV2Output, bool) bool, _ ...request.Option) error {
	m.prefixes = append(m.prefixes, aws.StringValue(input.Prefix))
	if !fn(&s3.ListObjectsV2Output{Contents: []*s3.Object{{Key: aws.String("podsync/artwork/v/a.jpg")}, {Key: aws.String("outside/user.jpg")}}}, false) {
		return nil
	}
	fn(&s3.ListObjectsV2Output{Contents: []*s3.Object{{Key: aws.String("podsync/artwork/v/a.jpg.ready")}}}, true)
	return nil
}
func TestS3ArtworkWalkPagesAndPrefix(t *testing.T) {
	api := &artworkListAPI{}
	storage := &S3{api: api, bucket: "test", prefix: "podsync"}
	var names []string
	err := storage.Walk(testCtx, "artwork/v", func(name string) error { names = append(names, name); return nil })
	assert.NoError(t, err)
	assert.Equal(t, []string{"podsync/artwork/v/"}, api.prefixes)
	assert.Equal(t, []string{"artwork/v/a.jpg", "artwork/v/a.jpg.ready"}, names)
	expected := errors.New("callback failure")
	assert.ErrorIs(t, storage.Walk(testCtx, "artwork/v", func(string) error { return expected }), expected)
}

func (m *mockS3API) ListObjectsV2PagesWithContext(ctx aws.Context, input *s3.ListObjectsV2Input, fn func(*s3.ListObjectsV2Output, bool) bool, _ ...request.Option) error {
	var objects []*s3.Object
	for key := range m.files {
		if strings.HasPrefix(key, aws.StringValue(input.Prefix)) {
			objects = append(objects, &s3.Object{Key: aws.String(key)})
		}
	}
	fn(&s3.ListObjectsV2Output{Contents: objects}, true)
	return nil
}
func TestS3WalkAndDeleteGeneratedObjects(t *testing.T) {
	files := map[string][]byte{"podsync/artwork/version/generated.jpg": {1}, "podsync/artwork/version/generated.jpg.ready": {1}, "podsync/user-cover.jpg": {2}}
	storage, err := newMockS3(files, "podsync")
	assert.NoError(t, err)
	assert.NoError(t, storage.Walk(testCtx, "artwork/version", func(name string) error { return storage.Delete(testCtx, name) }))
	assert.Equal(t, map[string][]byte{"podsync/user-cover.jpg": {2}}, files)
}

func TestS3WalkWithoutStoragePrefix(t *testing.T) {
	storage, err := newMockS3(map[string][]byte{"artwork/v/a.jpg": {1}}, "")
	assert.NoError(t, err)
	var names []string
	assert.NoError(t, storage.Walk(testCtx, "artwork/v", func(name string) error { names = append(names, name); return nil }))
	assert.Equal(t, []string{"artwork/v/a.jpg"}, names)
}
