package mounts

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/pkg/labels"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/plugins/snapshots/native"
	"github.com/moby/buildkit/cache"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/snapshot"
	"github.com/moby/buildkit/solver/pb"
	digest "github.com/opencontainers/go-digest"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
)

type testCacheMountHook struct {
	mu      sync.Mutex
	used    map[string]string
	imports map[string]int
	importF func(ctx context.Context, cm cache.Manager, id string) (cache.ImmutableRef, error)
}

func (h *testCacheMountHook) ImportCacheMount(ctx context.Context, cm cache.Manager, id string, _ session.Group) (cache.ImmutableRef, error) {
	h.mu.Lock()
	h.imports[id]++
	h.mu.Unlock()
	return h.importF(ctx, cm, id)
}

func (h *testCacheMountHook) CacheMountUsed(_ cache.Manager, id string, refID string) {
	h.mu.Lock()
	h.used[id] = refID
	h.mu.Unlock()
}

func TestCacheMountHooks(t *testing.T) {
	t.Parallel()
	ctx := namespaces.WithNamespace(t.Context(), "buildkit-test")

	tmpdir := t.TempDir()
	snapshotter, err := native.NewSnapshotter(filepath.Join(tmpdir, "snapshots"))
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, snapshotter.Close())
	})
	co, err := newCacheManager(ctx, t, cmOpt{
		snapshotter:     snapshotter,
		snapshotterName: "native",
	})
	require.NoError(t, err)

	desc := writeTestBlob(ctx, t, co.cs, "imported", "data")

	hook := &testCacheMountHook{
		used:    map[string]string{},
		imports: map[string]int{},
		importF: func(ctx context.Context, cm cache.Manager, id string) (cache.ImmutableRef, error) {
			switch id {
			case "imp":
				return cm.GetByBlob(ctx, desc, nil)
			case "bad":
				return nil, errors.New("import failed")
			}
			return nil, nil
		},
	}
	hctx := WithCacheMountHooks(ctx, []CacheMountHook{hook})

	// A missing cache directory is seeded from the import.
	ref, err := newRefGetter(co.manager, sharedCacheRefs).getRefCacheDir(hctx, nil, "imp", pb.CacheSharingOpt_SHARED)
	require.NoError(t, err)
	require.Equal(t, "data", readTestFile(ctx, t, ref, "imported"))
	refID := ref.ID()
	require.Equal(t, map[string]string{"imp": refID}, hook.used)

	// While the directory is open it cannot be exported.
	err = WithCacheMountRef(ctx, co.manager, "imp", refID, func(cache.MutableRef) error { return nil })
	require.ErrorIs(t, err, cache.ErrLocked)
	require.NoError(t, ref.Release(ctx))

	// Once released it can be read for export, and stays the cache directory.
	called := false
	err = WithCacheMountRef(ctx, co.manager, "imp", refID, func(r cache.MutableRef) error {
		called = true
		require.Equal(t, refID, r.ID())
		return nil
	})
	require.NoError(t, err)
	require.True(t, called)

	// A local cache directory wins over the import.
	ref, err = newRefGetter(co.manager, sharedCacheRefs).getRefCacheDir(hctx, nil, "imp", pb.CacheSharingOpt_SHARED)
	require.NoError(t, err)
	require.Equal(t, refID, ref.ID())
	require.Equal(t, 1, hook.imports["imp"])
	require.NoError(t, ref.Release(ctx))

	// A failed import falls back to an empty directory.
	ref, err = newRefGetter(co.manager, sharedCacheRefs).getRefCacheDir(hctx, nil, "bad", pb.CacheSharingOpt_PRIVATE)
	require.NoError(t, err)
	require.Equal(t, 1, hook.imports["bad"])
	require.Contains(t, hook.used, "bad")
	require.NoError(t, ref.Release(ctx))

	// Cache mounts with a from= source are neither imported nor reported.
	base, err := co.manager.New(ctx, nil, nil)
	require.NoError(t, err)
	baseRef, err := base.Commit(ctx)
	require.NoError(t, err)
	ref, err = newRefGetter(co.manager, sharedCacheRefs).getRefCacheDir(hctx, baseRef, "from", pb.CacheSharingOpt_PRIVATE)
	require.NoError(t, err)
	require.NotContains(t, hook.used, "from")
	require.NotContains(t, hook.used, "from:"+baseRef.ID())
	require.Zero(t, hook.imports["from"])
	require.NoError(t, ref.Release(ctx))
	require.NoError(t, baseRef.Release(ctx))
}

func writeTestBlob(ctx context.Context, t *testing.T, cs content.Store, name, data string) ocispecs.Descriptor {
	t.Helper()
	tarBuf := &bytes.Buffer{}
	tw := tar.NewWriter(tarBuf)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(data))}))
	_, err := tw.Write([]byte(data))
	require.NoError(t, err)
	require.NoError(t, tw.Close())

	gzBuf := &bytes.Buffer{}
	gw := gzip.NewWriter(gzBuf)
	_, err = gw.Write(tarBuf.Bytes())
	require.NoError(t, err)
	require.NoError(t, gw.Close())

	desc := ocispecs.Descriptor{
		MediaType: ocispecs.MediaTypeImageLayerGzip,
		Digest:    digest.FromBytes(gzBuf.Bytes()),
		Size:      int64(gzBuf.Len()),
		Annotations: map[string]string{
			labels.LabelUncompressed: digest.FromBytes(tarBuf.Bytes()).String(),
		},
	}
	require.NoError(t, content.WriteBlob(ctx, cs, name, bytes.NewReader(gzBuf.Bytes()), desc))
	return desc
}

func readTestFile(ctx context.Context, t *testing.T, ref cache.MutableRef, name string) string {
	t.Helper()
	m, err := ref.Mount(ctx, true, nil)
	require.NoError(t, err)
	lm := snapshot.LocalMounter(m)
	target, err := lm.Mount()
	require.NoError(t, err)
	defer lm.Unmount()
	dt, err := os.ReadFile(filepath.Join(target, name))
	require.NoError(t, err)
	return string(dt)
}
