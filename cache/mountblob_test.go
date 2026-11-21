package cache

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/pkg/labels"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/plugins/snapshots/native"
	"github.com/moby/buildkit/snapshot"
	"github.com/moby/buildkit/util/compression"
	"github.com/moby/buildkit/util/leaseutil"
	digest "github.com/opencontainers/go-digest"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
)

func TestComputeMutableBlob(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Depends on unimplemented containerd bind-mount support on Windows")
	}
	t.Parallel()

	ctx := namespaces.WithNamespace(t.Context(), "buildkit-test")
	tmpdir := t.TempDir()
	snapshotter, err := native.NewSnapshotter(filepath.Join(tmpdir, "snapshots"))
	require.NoError(t, err)
	co, cleanup, err := newCacheManager(ctx, t, cmOpt{
		snapshotter:     snapshotter,
		snapshotterName: "native",
	})
	require.NoError(t, err)
	t.Cleanup(cleanup)
	cm := co.manager

	// Seed the mutable ref with a parent layer, like an imported cache mount.
	b, desc, err := mapToBlob(map[string]string{"parent": "p"}, true)
	require.NoError(t, err)
	require.NoError(t, content.WriteBlob(ctx, co.cs, "parent", bytes.NewBuffer(b), desc))
	parent, err := cm.GetByBlob(ctx, desc, nil)
	require.NoError(t, err)

	mref, err := cm.New(ctx, parent, nil, CachePolicyRetain)
	require.NoError(t, err)
	require.NoError(t, parent.Release(ctx))
	refID := mref.ID()

	writeFile := func(ref MutableRef, name, data string) {
		m, err := ref.Mount(ctx, false, nil)
		require.NoError(t, err)
		lm := snapshot.LocalMounter(m)
		target, err := lm.Mount()
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(target, name), []byte(data), 0644))
		require.NoError(t, lm.Unmount())
	}
	writeFile(mref, "child", "c")

	leaseCtx, done, err := leaseutil.WithLease(ctx, co.lm, leases.WithExpiration(0))
	require.NoError(t, err)
	defer done(ctx)

	blob, err := ComputeMutableBlob(leaseCtx, mref, compression.New(compression.Gzip), nil)
	require.NoError(t, err)
	require.Equal(t, ocispecs.MediaTypeImageLayerGzip, blob.MediaType)

	// The blob holds the whole tree, parent contents included.
	files, diffID := readBlobFiles(t, co.cs, blob)
	require.Equal(t, map[string]string{"parent": "p", "child": "c"}, files)
	require.Equal(t, diffID.String(), blob.Annotations[labels.LabelUncompressed])

	// The ref stays a usable cache directory.
	writeFile(mref, "child2", "c2")
	require.NoError(t, mref.Release(ctx))
	mref, err = cm.GetMutable(ctx, refID)
	require.NoError(t, err)
	blob2, err := ComputeMutableBlob(leaseCtx, mref, compression.New(compression.Gzip), nil)
	require.NoError(t, err)
	files, _ = readBlobFiles(t, co.cs, blob2)
	require.Equal(t, map[string]string{"parent": "p", "child": "c", "child2": "c2"}, files)
	require.NoError(t, mref.Release(ctx))
}

func readBlobFiles(t *testing.T, cs content.Store, desc ocispecs.Descriptor) (map[string]string, digest.Digest) {
	t.Helper()
	dt, err := content.ReadBlob(t.Context(), cs, desc)
	require.NoError(t, err)
	gz, err := gzip.NewReader(bytes.NewReader(dt))
	require.NoError(t, err)
	dgstr := digest.SHA256.Digester()
	r := io.TeeReader(gz, dgstr.Hash())
	tr := tar.NewReader(r)
	files := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if h.Typeflag != tar.TypeReg {
			continue
		}
		v, err := io.ReadAll(tr)
		require.NoError(t, err)
		files[h.Name] = string(v)
	}
	_, err = io.Copy(io.Discard, r)
	require.NoError(t, err)
	return files, dgstr.Digest()
}
