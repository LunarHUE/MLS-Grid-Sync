package server_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/attachmentjob"
	entmedia "github.com/LunarHUE/MLS-Grid-Sync/ent/media"
	"github.com/LunarHUE/MLS-Grid-Sync/health"
	"github.com/LunarHUE/MLS-Grid-Sync/internal/testutil"
	"github.com/LunarHUE/MLS-Grid-Sync/search"
	"github.com/LunarHUE/MLS-Grid-Sync/server"
	"github.com/LunarHUE/MLS-Grid-Sync/storage"
)

// memStorer is a Storer that also satisfies storage.Fetcher, so the handler's
// hit path can be exercised without a cloud backend.
type memStorer struct {
	objects map[string][]byte
	types   map[string]string
}

func newMemStorer() *memStorer {
	return &memStorer{objects: map[string][]byte{}, types: map[string]string{}}
}

func (m *memStorer) Upload(_ context.Context, key string, body io.Reader, contentType string) (string, error) {
	b, err := io.ReadAll(body)
	if err != nil {
		return "", err
	}
	m.objects[key] = b
	m.types[key] = contentType
	return "mem://" + key, nil
}

func (m *memStorer) Download(_ context.Context, key string) (io.ReadCloser, string, error) {
	b, ok := m.objects[key]
	if !ok {
		return nil, "", storage.ErrObjectNotFound
	}
	return io.NopCloser(strings.NewReader(string(b))), m.types[key], nil
}

// uploadOnlyStorer deliberately does NOT implement storage.Fetcher, covering
// the "backend cannot read back" degradation.
type uploadOnlyStorer struct{}

func (uploadOnlyStorer) Upload(context.Context, string, io.Reader, string) (string, error) {
	return "", nil
}

func newMediaHarness(t *testing.T, opts server.MediaOptions) (*httptest.Server, *ent.Client) {
	t.Helper()
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	hsvc := health.NewService(client, okPing, testThresholds(), time.Now).
		WithTrigramProbe(func(ctx context.Context) error {
			return search.CheckExtension(ctx, sqlDB)
		})
	srv := httptest.NewServer(server.NewMux(client, hsvc, server.Options{Media: opts}))
	t.Cleanup(srv.Close)
	return srv, client
}

// seedMedia inserts a media row, optionally already linked to a stored
// attachment. Returns the object key the handler is expected to read.
func seedMedia(t *testing.T, client *ent.Client, mediaKey string, withAttachment bool, body []byte) string {
	t.Helper()
	ctx := context.Background()

	create := client.Media.Create().
		SetID(mediaKey).
		SetSourceModifiedAt(time.Now()).
		SetResourceType(entmedia.ResourceTypeProperty).
		SetResourceRecordKey("LK-1").
		SetMediaURL("https://media.example.test/" + mediaKey + ".jpg")

	if !withAttachment {
		require.NoError(t, create.Exec(ctx))
		return ""
	}

	// sha256 of the body is the second half of the key, matching how the
	// attachment worker addresses objects.
	const hash = "0000000000000000000000000000000000000000000000000000000000000001"
	att, err := client.Attachment.Create().
		SetSourceURL("https://media.example.test/" + mediaKey + ".jpg").
		SetSourceHash(hash).
		SetHostURL("mem://media/" + mediaKey + "/" + hash).
		SetMimeType("image/jpeg").
		SetSizeBytes(len(body)).
		Save(ctx)
	require.NoError(t, err)
	require.NoError(t, create.SetAttachmentID(att.ID).Exec(ctx))
	return "media/" + mediaKey + "/" + hash
}

func TestMedia_RouteAbsentWithoutStorer(t *testing.T) {
	t.Parallel()
	srv, client := newMediaHarness(t, server.MediaOptions{})
	seedMedia(t, client, "MK-nostore", false, nil)

	resp, err := http.Get(srv.URL + "/media/MK-nostore")
	require.NoError(t, err)
	resp.Body.Close()
	// No storer means the route was never registered at all.
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestMedia_CacheHitStreamsStoredBytes(t *testing.T) {
	t.Parallel()
	store := newMemStorer()
	srv, client := newMediaHarness(t, server.MediaOptions{Storer: store})

	body := []byte("\xff\xd8\xff-not-really-a-jpeg")
	key := seedMedia(t, client, "MK-hit", true, body)
	store.objects[key] = body
	store.types[key] = "image/jpeg"

	resp, err := http.Get(srv.URL + "/media/MK-hit")
	require.NoError(t, err)
	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, body, got)
	assert.Equal(t, "image/jpeg", resp.Header.Get("Content-Type"))
	assert.Equal(t, "hit", resp.Header.Get("X-Media-Status"))
	assert.Contains(t, resp.Header.Get("Cache-Control"), "immutable")
}

func TestMedia_UnknownKeyIs404(t *testing.T) {
	t.Parallel()
	srv, _ := newMediaHarness(t, server.MediaOptions{Storer: newMemStorer()})

	resp, err := http.Get(srv.URL + "/media/MK-does-not-exist")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	// No prefetch signal: there is nothing to prefetch for an unknown key.
	assert.Empty(t, resp.Header.Get("X-Media-Status"))
}

// The critical behavior: a miss must NOT try the stored media_url (expired and
// single-use) — it registers work for the background worker instead.
func TestMedia_MissQueuesPrefetch(t *testing.T) {
	t.Parallel()
	srv, client := newMediaHarness(t, server.MediaOptions{Storer: newMemStorer()})
	seedMedia(t, client, "MK-miss", false, nil)

	// A SourceSystem must exist for the synthetic prefetch SyncEvent to hang
	// off, mirroring "init has run".
	ctx := context.Background()
	require.NoError(t, client.SourceSystem.Create().
		SetID("SS-1").
		SetSourceSystemName("test").
		Exec(ctx))

	resp, err := http.Get(srv.URL + "/media/MK-miss")
	require.NoError(t, err)
	resp.Body.Close()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Equal(t, "prefetch-queued", resp.Header.Get("X-Media-Status"))
	assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"))

	n, err := client.AttachmentJob.Query().
		Where(attachmentjob.MediaKey("MK-miss")).
		Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "exactly one prefetch job should be queued")

	// A second request must not pile up duplicate work.
	resp2, err := http.Get(srv.URL + "/media/MK-miss")
	require.NoError(t, err)
	resp2.Body.Close()
	n2, err := client.AttachmentJob.Query().
		Where(attachmentjob.MediaKey("MK-miss")).
		Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n2, "repeat requests must not duplicate the job")
}

// An attachment row pointing at bytes the backend no longer holds must fall
// back to the miss path, not 404 forever or 500.
func TestMedia_MissingObjectFallsBackToPrefetch(t *testing.T) {
	t.Parallel()
	store := newMemStorer() // deliberately left empty
	srv, client := newMediaHarness(t, server.MediaOptions{Storer: store})
	seedMedia(t, client, "MK-dangling", true, []byte("gone"))

	ctx := context.Background()
	require.NoError(t, client.SourceSystem.Create().
		SetID("SS-2").
		SetSourceSystemName("test").
		Exec(ctx))

	resp, err := http.Get(srv.URL + "/media/MK-dangling")
	require.NoError(t, err)
	resp.Body.Close()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Equal(t, "prefetch-queued", resp.Header.Get("X-Media-Status"))
}

func TestMedia_UploadOnlyBackendStillQueues(t *testing.T) {
	t.Parallel()
	srv, client := newMediaHarness(t, server.MediaOptions{Storer: uploadOnlyStorer{}})
	seedMedia(t, client, "MK-noread", true, []byte("x"))

	ctx := context.Background()
	require.NoError(t, client.SourceSystem.Create().
		SetID("SS-3").
		SetSourceSystemName("test").
		Exec(ctx))

	resp, err := http.Get(srv.URL + "/media/MK-noread")
	require.NoError(t, err)
	resp.Body.Close()
	// Cannot read back, so even a linked attachment takes the miss path.
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestMedia_PrefetchDisabled(t *testing.T) {
	t.Parallel()
	srv, client := newMediaHarness(t, server.MediaOptions{
		Storer:          newMemStorer(),
		DisablePrefetch: true,
	})
	seedMedia(t, client, "MK-nopf", false, nil)

	resp, err := http.Get(srv.URL + "/media/MK-nopf")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	n, err := client.AttachmentJob.Query().
		Where(attachmentjob.MediaKey("MK-nopf")).
		Count(context.Background())
	require.NoError(t, err)
	assert.Zero(t, n, "prefetch disabled must not enqueue")
}

// noRedirectClient returns the 302 itself rather than following it; the target
// host does not exist in these tests.
func noRedirectClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// The point of redirect mode: the browser is sent to the object's public
// address and the bytes never transit this process.
func TestMedia_PublicBaseURLRedirectsOnHit(t *testing.T) {
	t.Parallel()
	store := newMemStorer()
	srv, client := newMediaHarness(t, server.MediaOptions{
		Storer:        store,
		PublicBaseURL: "https://media.example.test",
	})

	body := []byte("\xff\xd8\xff-not-really-a-jpeg")
	key := seedMedia(t, client, "MK-redir", true, body)
	// Deliberately NOT placed in the store: a redirect must not depend on this
	// process being able to read the object.

	resp, err := noRedirectClient().Get(srv.URL + "/media/MK-redir")
	require.NoError(t, err)
	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	resp.Body.Close()

	assert.Equal(t, http.StatusFound, resp.StatusCode)
	assert.Equal(t, "https://media.example.test/"+key, resp.Header.Get("Location"))
	assert.Equal(t, "redirect", resp.Header.Get("X-Media-Status"))
	// http.Redirect writes its stock <a href> body on a GET; what matters is
	// that the image itself did not come through this process.
	assert.NotEqual(t, body, got, "redirect must not carry the image bytes")
	assert.Less(t, len(got), 256, "redirect body should be the stock hyperlink, not a payload")
	// A permanent redirect would pin browsers to this sha256 forever; the
	// MediaKey -> object mapping changes when a listing is re-photographed.
	assert.Contains(t, resp.Header.Get("Cache-Control"), "max-age=")
	assert.NotContains(t, resp.Header.Get("Cache-Control"), "immutable")
}

// Redirect mode reads no objects, so serve must be able to run it with no
// storage backend configured at all — that is what lets the API process drop
// its storage credentials.
func TestMedia_PublicBaseURLNeedsNoStorer(t *testing.T) {
	t.Parallel()
	srv, client := newMediaHarness(t, server.MediaOptions{
		PublicBaseURL: "https://media.example.test/",
	})
	key := seedMedia(t, client, "MK-nostorer", true, []byte("x"))

	resp, err := noRedirectClient().Get(srv.URL + "/media/MK-nostorer")
	require.NoError(t, err)
	resp.Body.Close()

	assert.Equal(t, http.StatusFound, resp.StatusCode)
	// Trailing slash on the base must not double up in the target.
	assert.Equal(t, "https://media.example.test/"+key, resp.Header.Get("Location"))
}

// A key prefix belongs in the redirect target too, or the browser is sent
// somewhere the worker never wrote.
func TestMedia_PublicBaseURLHonoursKeyPrefix(t *testing.T) {
	t.Parallel()
	srv, client := newMediaHarness(t, server.MediaOptions{
		PublicBaseURL: "https://cdn.example.test/assets",
		KeyPrefix:     "prod",
	})
	key := seedMedia(t, client, "MK-prefixed", true, []byte("x"))

	resp, err := noRedirectClient().Get(srv.URL + "/media/MK-prefixed")
	require.NoError(t, err)
	resp.Body.Close()

	assert.Equal(t, http.StatusFound, resp.StatusCode)
	assert.Equal(t, "https://cdn.example.test/assets/prod/"+key, resp.Header.Get("Location"))
}

// A media row with no attachment still takes the miss path in redirect mode —
// there is no object address to hand out yet.
func TestMedia_PublicBaseURLMissStillQueuesPrefetch(t *testing.T) {
	t.Parallel()
	srv, client := newMediaHarness(t, server.MediaOptions{
		PublicBaseURL: "https://media.example.test",
	})
	seedMedia(t, client, "MK-redir-miss", false, nil)

	ctx := context.Background()
	require.NoError(t, client.SourceSystem.Create().
		SetID("SS-redir").
		SetSourceSystemName("test").
		Exec(ctx))

	resp, err := noRedirectClient().Get(srv.URL + "/media/MK-redir-miss")
	require.NoError(t, err)
	resp.Body.Close()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Equal(t, "prefetch-queued", resp.Header.Get("X-Media-Status"))

	n, err := client.AttachmentJob.Query().
		Where(attachmentjob.MediaKey("MK-redir-miss")).
		Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

// An unusable base must not silently become a broken redirect: the handler
// falls back to streaming when it still has a Fetcher.
func TestMedia_UnusablePublicBaseURLFallsBackToStreaming(t *testing.T) {
	t.Parallel()
	store := newMemStorer()
	srv, client := newMediaHarness(t, server.MediaOptions{
		Storer:        store,
		PublicBaseURL: "not-a-url",
	})

	body := []byte("streamed")
	key := seedMedia(t, client, "MK-badbase", true, body)
	store.objects[key] = body
	store.types[key] = "image/jpeg"

	resp, err := noRedirectClient().Get(srv.URL + "/media/MK-badbase")
	require.NoError(t, err)
	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, body, got)
	assert.Equal(t, "hit", resp.Header.Get("X-Media-Status"))
}
