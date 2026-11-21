package cache

import (
	"context"
	"maps"
	"runtime"

	"github.com/containerd/containerd/v2/core/diff"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/labels"
	"github.com/containerd/containerd/v2/plugins/diff/walking"
	"github.com/moby/buildkit/identity"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/util/compression"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

// ComputeMutableBlob writes the full contents of a mutable ref to the content
// store as a single layer blob, and returns its descriptor with the
// uncompressed digest in its annotations.
//
// Unlike computing the blob of a committed ref, this leaves ref mutable and
// usable, which is what cache mounts need. The ref is mounted read-only, so the
// caller must make sure nothing writes to it while this runs. The caller must
// hold a lease, which will own the new blob.
func ComputeMutableBlob(ctx context.Context, ref MutableRef, comp compression.Config, s session.Group) (ocispecs.Descriptor, error) {
	if runtime.GOOS == "windows" {
		return ocispecs.Descriptor{}, errors.New("computing blobs of mutable refs is not supported on Windows")
	}
	sr, ok := ref.(*mutableRef)
	if !ok {
		return ocispecs.Descriptor{}, errors.Errorf("invalid mutable ref type %T", ref)
	}
	if comp.Type == nil {
		comp = compression.New(compression.Default)
	}

	m, err := sr.Mount(ctx, true, s)
	if err != nil {
		return ocispecs.Descriptor{}, err
	}
	upper, release, err := m.Mount()
	if err != nil {
		return ocispecs.Descriptor{}, err
	}
	if release != nil {
		defer release()
	}

	mediaType := comp.Type.MediaType()
	compressorFunc, finalize := comp.Type.Compress(ctx, comp)
	// A nil lower diffs against an empty directory, so the blob holds the
	// whole tree and no whiteouts.
	var lower []mount.Mount
	desc, err := walking.NewWalkingDiff(openWriterStore{Store: sr.cm.ContentStore}).Compare(ctx, lower, upper,
		diff.WithMediaType(mediaType),
		diff.WithReference("mutable-blob-"+sr.ID()+"-"+identity.NewID()),
		diff.WithCompressor(compressorFunc),
	)
	if err != nil {
		return ocispecs.Descriptor{}, errors.Wrapf(err, "failed to compute blob of %s", sr.ID())
	}

	if desc.Annotations == nil {
		desc.Annotations = map[string]string{}
	}
	if finalize != nil {
		a, err := finalize(ctx, sr.cm.ContentStore)
		if err != nil {
			return ocispecs.Descriptor{}, errors.Wrapf(err, "failed to finalize compression")
		}
		maps.Copy(desc.Annotations, a)
	}
	info, err := sr.cm.ContentStore.Info(ctx, desc.Digest)
	if err != nil {
		return ocispecs.Descriptor{}, err
	}
	if diffID, ok := info.Labels[labels.LabelUncompressed]; ok {
		desc.Annotations[labels.LabelUncompressed] = diffID
	} else if mediaType == ocispecs.MediaTypeImageLayer {
		desc.Annotations[labels.LabelUncompressed] = desc.Digest.String()
	} else {
		return ocispecs.Descriptor{}, errors.Errorf("unknown layer compression type")
	}
	return desc, nil
}
