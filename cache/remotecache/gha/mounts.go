package gha

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"slices"
	"time"

	"github.com/containerd/containerd/v2/pkg/labels"
	"github.com/moby/buildkit/cache/remotecache"
	v1 "github.com/moby/buildkit/cache/remotecache/v1"
	cacheimporttypes "github.com/moby/buildkit/cache/remotecache/v1/types"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/util/bklog"
	digest "github.com/opencontainers/go-digest"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	actionscache "github.com/tonistiigi/go-actions-cache"
	"golang.org/x/sync/errgroup"
)

// mountIndex is the cache entry that maps cache mount IDs to the blob holding
// their contents. Blobs are stored under the same keys as layer blobs, so
// identical contents are only uploaded once.
type mountIndex struct {
	Mounts map[string]cacheimporttypes.CacheLayer `json:"mounts"`
}

var (
	_ remotecache.MountCacheExporter = &exporter{}
	_ remotecache.MountCacheImporter = &importer{}
)

// mountIndexKey returns the key of the mounts index. Cache keys are matched by
// prefix on load, so the key must not be a prefix of, or prefixed by, a layer
// index key ("index-...") or a blob key ("buildkit-blob-...").
func mountIndexKey(scope string, config *Config) string {
	scope = digest.FromBytes([]byte(scope)).Hex()[:8]
	return "buildkit-mounts-" + version + "-" + config.Scope + "-" + scope
}

func (ce *exporter) mountIndexKey() string {
	scope := ""
	for _, s := range ce.cache.Scopes() {
		if s.Permission&actionscache.PermissionWrite != 0 {
			scope = s.Scope
		}
	}
	return mountIndexKey(scope, ce.config)
}

func checkMountsSupported(config *Config) error {
	// The mounts index is not signed, so it must not be mixed with a cache
	// that is expected to be verifiable.
	if config.Sign != nil || config.Verify.Required {
		return errors.New("cache mounts are not supported with signed GitHub Actions cache")
	}
	return nil
}

func (ce *exporter) ExportCacheMount(ctx context.Context, id string, remote *solver.Remote) error {
	if err := checkMountsSupported(ce.config); err != nil {
		return err
	}
	if len(remote.Descriptors) != 1 {
		return errors.Errorf("cache mount %q must have exactly one layer, got %d", id, len(remote.Descriptors))
	}
	ce.mountsMu.Lock()
	defer ce.mountsMu.Unlock()
	if ce.mounts == nil {
		ce.mounts = map[string]v1.DescriptorProviderPair{}
	}
	ce.mounts[id] = v1.DescriptorProviderPair{
		Descriptor: remote.Descriptors[0],
		Provider:   remote.Provider,
	}
	return nil
}

// finalizeMounts uploads the queued cache mount blobs and updates the mounts
// index. Entries for mounts that this build did not use are kept, so a build
// that hits the layer cache for a step does not drop that step's cache mount.
func (ce *exporter) finalizeMounts(ctx context.Context) error {
	ce.mountsMu.Lock()
	queued := maps.Clone(ce.mounts)
	ce.mountsMu.Unlock()
	if len(queued) == 0 {
		return nil
	}

	now := time.Now().UTC()
	records := make(map[string]cacheimporttypes.CacheLayer, len(queued))
	for id, dp := range queued {
		v, ok := dp.Descriptor.Annotations[labels.LabelUncompressed]
		if !ok {
			return errors.Errorf("cache mount %q blob without uncompressed annotation", id)
		}
		diffID, err := digest.Parse(v)
		if err != nil {
			return errors.Wrapf(err, "failed to parse uncompressed annotation of cache mount %q", id)
		}
		records[id] = cacheimporttypes.CacheLayer{
			Blob: dp.Descriptor.Digest,
			Annotations: &cacheimporttypes.LayerAnnotations{
				MediaType: dp.Descriptor.MediaType,
				DiffID:    diffID,
				Size:      dp.Descriptor.Size,
				CreatedAt: now,
			},
		}
	}

	// Mounts with the same contents share a blob; upload each blob once.
	blobs := map[digest.Digest]v1.DescriptorProviderPair{}
	for _, dp := range queued {
		blobs[dp.Descriptor.Digest] = dp
	}
	eg, egCtx := errgroup.WithContext(ctx)
	eg.SetLimit(4)
	for _, dgst := range slices.Sorted(maps.Keys(blobs)) {
		eg.Go(func() error {
			return ce.uploadBlob(egCtx, blobs[dgst])
		})
	}
	if err := eg.Wait(); err != nil {
		return err
	}

	return ce.cache.SaveMutable(ctx, ce.mountIndexKey(), 15*time.Second, func(old *actionscache.Entry) (actionscache.Blob, error) {
		idx := mountIndex{Mounts: map[string]cacheimporttypes.CacheLayer{}}
		if old != nil {
			prev, err := readMountIndex(ctx, old)
			if err != nil {
				bklog.G(ctx).WithError(err).Warn("discarding invalid cache mounts index")
			} else {
				maps.Copy(idx.Mounts, prev.Mounts)
			}
		}
		maps.Copy(idx.Mounts, records)
		dt, err := json.Marshal(idx)
		if err != nil {
			return nil, err
		}
		return actionscache.NewBlob(dt), nil
	})
}

func readMountIndex(ctx context.Context, entry *actionscache.Entry) (*mountIndex, error) {
	buf := &bytes.Buffer{}
	if err := entry.WriteTo(ctx, buf); err != nil {
		return nil, err
	}
	var idx mountIndex
	if err := json.Unmarshal(buf.Bytes(), &idx); err != nil {
		return nil, errors.WithStack(err)
	}
	return &idx, nil
}

// ResolveCacheMounts loads the mounts index of every readable scope. When a
// mount is stored in several scopes, the one from a writable scope (the
// current branch) wins over read-only ones (such as the default branch).
func (ci *importer) ResolveCacheMounts(ctx context.Context) (map[string]*solver.Remote, error) {
	if err := checkMountsSupported(ci.config); err != nil {
		return nil, err
	}
	scopes := slices.Clone(ci.cache.Scopes())
	slices.SortStableFunc(scopes, func(a, b actionscache.Scope) int {
		aw := a.Permission&actionscache.PermissionWrite != 0
		bw := b.Permission&actionscache.PermissionWrite != 0
		switch {
		case aw && !bw:
			return -1
		case !aw && bw:
			return 1
		}
		return 0
	})

	remotes := map[string]*solver.Remote{}
	for _, s := range scopes {
		entry, err := ci.cache.Load(ctx, mountIndexKey(s.Scope, ci.config))
		if err != nil {
			return nil, err
		}
		if entry == nil {
			continue
		}
		idx, err := readMountIndex(ctx, entry)
		if err != nil {
			bklog.G(ctx).WithError(err).Warnf("ignoring invalid cache mounts index for scope %s", s.Scope)
			continue
		}
		for id, l := range idx.Mounts {
			if _, ok := remotes[id]; ok {
				continue
			}
			dp, err := ci.makeDescriptorProviderPair(l)
			if err != nil {
				bklog.G(ctx).WithError(err).Warnf("ignoring invalid cache mount %q in scope %s", id, s.Scope)
				continue
			}
			remotes[id] = &solver.Remote{
				Descriptors: []ocispecs.Descriptor{dp.Descriptor},
				Provider:    &ciProvider{desc: dp.Descriptor, ci: ci},
			}
		}
	}
	return remotes, nil
}
