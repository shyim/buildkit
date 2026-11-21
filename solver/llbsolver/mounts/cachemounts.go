package mounts

import (
	"context"

	"github.com/moby/buildkit/cache"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/util/bklog"
	"github.com/pkg/errors"
)

// JobKeyCacheMountHook is the job value key under which a build stores its
// CacheMountHook.
const JobKeyCacheMountHook = "llb.cachemounthook"

// CacheMountHook lets a build observe and seed the cache mounts its exec
// operations use. It is used to export cache mount contents to a remote cache
// and to import them on a builder that does not have them locally.
type CacheMountHook interface {
	// ImportCacheMount is called when no local cache directory exists for id.
	// It returns the contents to seed the new cache directory with, or nil if
	// there is nothing to import.
	ImportCacheMount(ctx context.Context, cm cache.Manager, id string, g session.Group) (cache.ImmutableRef, error)
	// CacheMountUsed records that the cache directory for id, backed by the
	// mutable ref refID in cm, was used by this build.
	CacheMountUsed(cm cache.Manager, id string, refID string)
}

type cacheMountHooksKey struct{}

// WithCacheMountHooks returns a context that makes cache mounts resolved with
// it report to hooks.
func WithCacheMountHooks(ctx context.Context, hooks []CacheMountHook) context.Context {
	if len(hooks) == 0 {
		return ctx
	}
	return context.WithValue(ctx, cacheMountHooksKey{}, hooks)
}

func cacheMountHooks(ctx context.Context) []CacheMountHook {
	hooks, _ := ctx.Value(cacheMountHooksKey{}).([]CacheMountHook)
	return hooks
}

// importCacheMount asks the hooks in order for contents to seed a new cache
// directory with. Import errors are logged and treated as a miss, since a
// cache mount is valid with any contents.
func importCacheMount(ctx context.Context, cm cache.Manager, id string, g session.Group) cache.ImmutableRef {
	for _, h := range cacheMountHooks(ctx) {
		ref, err := h.ImportCacheMount(ctx, cm, id, g)
		if err != nil {
			bklog.G(ctx).WithError(err).Warnf("failed to import cache mount %q", id)
			continue
		}
		if ref != nil {
			return ref
		}
	}
	return nil
}

// WithCacheMountRef gets exclusive access to the cache directory refID that
// was created for cache mount id, and calls fn with it. The lookup lock for
// id is held while fn runs, so a concurrent build waits for fn and then reuses
// the same directory instead of creating a second one.
//
// It returns cache.ErrLocked if the directory is in use by a running build.
func WithCacheMountRef(ctx context.Context, cm cache.Manager, id string, refID string, fn func(cache.MutableRef) error) error {
	cacheRefsLocker.Lock(id)
	defer cacheRefsLocker.Unlock(id)

	ref, err := cm.GetMutable(ctx, refID)
	if err != nil {
		return err
	}
	defer ref.Release(context.WithoutCancel(ctx))

	md := CacheRefMetadata{ref}
	if v := md.Get(keyCacheDir); v == nil || v.Index != cacheDirIndex+id {
		return errors.Errorf("ref %s is no longer the cache directory for %q", refID, id)
	}
	return fn(ref)
}
