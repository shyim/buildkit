package remotecache

import (
	"context"
	"strconv"

	"github.com/moby/buildkit/solver"
	"github.com/pkg/errors"
)

// AttrMounts is the cache import/export attribute that controls importing
// and exporting the contents of cache mounts (RUN --mount=type=cache).
const AttrMounts = "mounts"

// MountsMode is the value of the mounts attribute.
type MountsMode int

const (
	// MountsDisabled imports and exports only the layer cache (mounts=false,
	// the default).
	MountsDisabled MountsMode = iota
	// MountsEnabled imports and exports cache mounts along with the layer
	// cache (mounts=true).
	MountsEnabled
	// MountsOnly imports and exports only cache mounts, without the layer
	// cache (mounts=only).
	MountsOnly
)

// Mounts reports whether cache mounts are imported and exported.
func (m MountsMode) Mounts() bool {
	return m != MountsDisabled
}

// Layers reports whether the layer cache is imported and exported.
func (m MountsMode) Layers() bool {
	return m != MountsOnly
}

// ParseMountsAttr returns the cache mount mode set in attrs.
func ParseMountsAttr(attrs map[string]string) (MountsMode, error) {
	v, ok := attrs[AttrMounts]
	if !ok || v == "" {
		return MountsDisabled, nil
	}
	if v == "only" {
		return MountsOnly, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return MountsDisabled, errors.Errorf("invalid value %q for %s, expected true, false or only", v, AttrMounts)
	}
	if b {
		return MountsEnabled, nil
	}
	return MountsDisabled, nil
}

// MountCacheExporter is implemented by cache exporters that can store the
// contents of cache mounts.
type MountCacheExporter interface {
	// ExportCacheMount queues the contents of cache mount id for export. The
	// remote holds a single layer with the full contents of the mount. Queued
	// mounts are written by Finalize.
	ExportCacheMount(ctx context.Context, id string, remote *solver.Remote) error
}

// MountCacheImporter is implemented by cache importers that can load the
// contents of cache mounts stored by a MountCacheExporter.
type MountCacheImporter interface {
	// ResolveCacheMounts returns the stored cache mounts by id. Each remote
	// holds a single layer with the full contents of the mount.
	ResolveCacheMounts(ctx context.Context) (map[string]*solver.Remote, error)
}
