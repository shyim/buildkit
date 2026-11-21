package llbsolver

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/moby/buildkit/cache"
	"github.com/moby/buildkit/cache/remotecache"
	"github.com/moby/buildkit/frontend"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/solver/llbsolver/mounts"
	"github.com/moby/buildkit/util/bklog"
	"github.com/moby/buildkit/util/progress"
	"github.com/moby/buildkit/worker"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

// cacheMountTracker is the per-build mounts.CacheMountHook. It records the
// cache mounts a build uses so they can be exported, and seeds missing cache
// directories from cache imports with mounts=true.
type cacheMountTracker struct {
	workers []worker.Worker
	sources []*cacheMountSource

	mu   sync.Mutex
	used map[string]usedCacheMount
}

type usedCacheMount struct {
	cm    cache.Manager
	refID string
}

// cacheMountSource is a cache import with mounts=true. Its index is loaded the
// first time a build needs a cache directory that does not exist locally.
type cacheMountSource struct {
	name    string
	resolve func(ctx context.Context) (map[string]*solver.Remote, error)

	once    sync.Once
	remotes map[string]*solver.Remote
	err     error
}

func (s *cacheMountSource) load(ctx context.Context) (map[string]*solver.Remote, error) {
	s.once.Do(func() {
		done := progress.OneOff(ctx, "importing cache mounts index from "+s.name)
		s.remotes, s.err = s.resolve(context.WithoutCancel(ctx))
		done(s.err)
	})
	return s.remotes, s.err
}

func newCacheMountTracker(workers []worker.Worker, sessionID string, imports []frontend.CacheOptionsEntry, resolvers map[string]remotecache.ResolveCacheImporterFunc) (*cacheMountTracker, error) {
	t := &cacheMountTracker{
		workers: workers,
		used:    map[string]usedCacheMount{},
	}
	for _, im := range imports {
		mounts, err := remotecache.ParseMountsAttr(im.Attrs)
		if err != nil {
			return nil, errors.Wrapf(err, "invalid %s cache import", im.Type)
		}
		if !mounts.Mounts() {
			continue
		}
		resolveCI, ok := resolvers[im.Type]
		if !ok {
			return nil, errors.Errorf("unknown cache importer: %s", im.Type)
		}
		t.sources = append(t.sources, &cacheMountSource{
			name: im.Type,
			resolve: func(ctx context.Context) (map[string]*solver.Remote, error) {
				ci, _, err := resolveCI(ctx, session.NewGroup(sessionID), im.Attrs)
				if err != nil {
					return nil, errors.Wrapf(err, "failed to configure %v cache importer", im.Type)
				}
				mci, ok := ci.(remotecache.MountCacheImporter)
				if !ok {
					return nil, errors.Errorf("%s cache importer does not support importing cache mounts", im.Type)
				}
				return mci.ResolveCacheMounts(ctx)
			},
		})
	}
	return t, nil
}

func (t *cacheMountTracker) workerFor(cm cache.Manager) worker.Worker {
	for _, w := range t.workers {
		if w.CacheManager() == cm {
			return w
		}
	}
	return nil
}

func (t *cacheMountTracker) CacheMountUsed(cm cache.Manager, id string, refID string) {
	t.mu.Lock()
	t.used[id] = usedCacheMount{cm: cm, refID: refID}
	t.mu.Unlock()
}

func (t *cacheMountTracker) ImportCacheMount(ctx context.Context, cm cache.Manager, id string, g session.Group) (cache.ImmutableRef, error) {
	if len(t.sources) == 0 {
		return nil, nil
	}
	w := t.workerFor(cm)
	if w == nil {
		return nil, nil
	}
	for _, src := range t.sources {
		remotes, err := src.load(ctx)
		if err != nil {
			bklog.G(ctx).WithError(err).Warnf("failed to load cache mounts from %s", src.name)
			continue
		}
		remote, ok := remotes[id]
		if !ok {
			continue
		}
		done := progress.OneOff(ctx, fmt.Sprintf("importing cache mount %q from %s", id, src.name))
		ref, err := w.FromRemote(ctx, remote)
		done(err)
		if err != nil {
			bklog.G(ctx).WithError(err).Warnf("failed to import cache mount %q from %s", id, src.name)
			continue
		}
		return ref, nil
	}
	return nil, nil
}

// exportCacheMounts snapshots every cache mount this build used and queues it
// on exp. A mount that a running build still has open is skipped with a
// warning, since it cannot be read consistently.
func (t *cacheMountTracker) exportCacheMounts(ctx context.Context, exp RemoteCacheExporter, g session.Group) error {
	mce, ok := exp.Exporter.(remotecache.MountCacheExporter)
	if !ok {
		return errors.Errorf("%s does not support exporting cache mounts", exp.Name())
	}

	t.mu.Lock()
	used := maps.Clone(t.used)
	t.mu.Unlock()

	for _, id := range slices.Sorted(maps.Keys(used)) {
		u := used[id]
		w := t.workerFor(u.cm)
		if w == nil {
			continue
		}
		done := progress.OneOff(ctx, fmt.Sprintf("preparing cache mount %q for export", id))
		var desc ocispecs.Descriptor
		err := mounts.WithCacheMountRef(ctx, u.cm, id, u.refID, func(ref cache.MutableRef) error {
			var err error
			desc, err = cache.ComputeMutableBlob(ctx, ref, exp.Compression, g)
			return err
		})
		if errors.Is(err, cache.ErrLocked) {
			bklog.G(ctx).Warnf("skipping export of cache mount %q: in use by another build", id)
			done(nil)
			continue
		}
		if err != nil {
			return done(errors.Wrapf(err, "failed to export cache mount %q", id))
		}
		done(nil)
		if err := mce.ExportCacheMount(ctx, id, &solver.Remote{
			Descriptors: []ocispecs.Descriptor{desc},
			Provider:    w.ContentStore(),
		}); err != nil {
			return err
		}
	}
	return nil
}
