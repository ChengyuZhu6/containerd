/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package unpack

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/containerd/log"
	"github.com/containerd/platforms"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/identity"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"

	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/diff"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/labels"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/pkg/cleanup"
	"github.com/containerd/containerd/pkg/kmutex"
	"github.com/containerd/containerd/snapshots"
	"github.com/containerd/containerd/tracing"
)

const (
	labelSnapshotRef = "containerd.io/snapshot.ref"
	unpackSpanPrefix = "pkg.unpack.unpacker"
)

// Result returns information about the unpacks which were completed.
type Result struct {
	Unpacks int
}

type unpackerConfig struct {
	platforms []*Platform

	content content.Store

	limiter               *semaphore.Weighted
	unpackLimiter         *semaphore.Weighted
	duplicationSuppressor kmutex.KeyedLocker
}

// Platform represents a platform-specific unpack configuration which includes
// the platform matcher as well as snapshotter and applier.
type Platform struct {
	Platform platforms.Matcher

	SnapshotterKey string
	Snapshotter    snapshots.Snapshotter
	SnapshotOpts   []snapshots.Opt

	Applier   diff.Applier
	ApplyOpts []diff.ApplyOpt

	// SnapshotterCapabilities is a list of capabilities reported by the
	// snapshotter. Used to check whether parallel unpack (rebase) is
	// supported.
	SnapshotterCapabilities []string
}

type UnpackerOpt func(*unpackerConfig) error

func WithUnpackPlatform(u Platform) UnpackerOpt {
	return UnpackerOpt(func(c *unpackerConfig) error {
		if u.Platform == nil {
			u.Platform = platforms.All
		}
		if u.Snapshotter == nil {
			return fmt.Errorf("snapshotter must be provided to unpack")
		}
		if u.SnapshotterKey == "" {
			if s, ok := u.Snapshotter.(fmt.Stringer); ok {
				u.SnapshotterKey = s.String()
			} else {
				u.SnapshotterKey = "unknown"
			}
		}
		if u.Applier == nil {
			return fmt.Errorf("applier must be provided to unpack")
		}

		c.platforms = append(c.platforms, &u)

		return nil
	})
}

func WithLimiter(l *semaphore.Weighted) UnpackerOpt {
	return UnpackerOpt(func(c *unpackerConfig) error {
		c.limiter = l
		return nil
	})
}

// WithUnpackLimiter sets a semaphore to limit the number of concurrent
// unpack operations. This is different from WithLimiter which limits
// concurrent downloads.
func WithUnpackLimiter(l *semaphore.Weighted) UnpackerOpt {
	return UnpackerOpt(func(c *unpackerConfig) error {
		c.unpackLimiter = l
		return nil
	})
}

func WithDuplicationSuppressor(d kmutex.KeyedLocker) UnpackerOpt {
	return UnpackerOpt(func(c *unpackerConfig) error {
		c.duplicationSuppressor = d
		return nil
	})
}

// Unpacker unpacks images by hooking into the image handler process.
// Unpacks happen in the backgrounds and waited on to complete.
type Unpacker struct {
	unpackerConfig

	unpacks int32
	ctx     context.Context
	eg      *errgroup.Group
}

// NewUnpacker creates a new instance of the unpacker which can be used to wrap an
// image handler and unpack in parallel to handling. The unpacker will handle
// calling the block handlers when they are needed by the unpack process.
func NewUnpacker(ctx context.Context, cs content.Store, opts ...UnpackerOpt) (*Unpacker, error) {
	eg, ctx := errgroup.WithContext(ctx)

	u := &Unpacker{
		unpackerConfig: unpackerConfig{
			content:               cs,
			duplicationSuppressor: kmutex.NewNoop(),
		},
		ctx: ctx,
		eg:  eg,
	}
	for _, opt := range opts {
		if err := opt(&u.unpackerConfig); err != nil {
			return nil, err
		}
	}
	if len(u.platforms) == 0 {
		return nil, fmt.Errorf("no unpack platforms defined: %w", errdefs.ErrInvalidArgument)
	}
	return u, nil
}

// Unpack wraps an image handler to filter out blob handling and scheduling them
// during the unpack process. When an image config is encountered, the unpack
// process will be started in a goroutine.
func (u *Unpacker) Unpack(h images.Handler) images.Handler {
	var (
		lock   sync.Mutex
		layers = map[digest.Digest][]ocispec.Descriptor{}
	)
	return images.HandlerFunc(func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		ctx, span := tracing.StartSpan(ctx, tracing.Name(unpackSpanPrefix, "UnpackHandler"))
		defer span.End()
		span.SetAttributes(
			tracing.Attribute("descriptor.media.type", desc.MediaType),
			tracing.Attribute("descriptor.digest", desc.Digest.String()))
		unlock, err := u.lockBlobDescriptor(ctx, desc)
		if err != nil {
			return nil, err
		}
		children, err := h.Handle(ctx, desc)
		unlock()
		if err != nil {
			return children, err
		}

		switch desc.MediaType {
		case images.MediaTypeDockerSchema2Manifest, ocispec.MediaTypeImageManifest:
			var nonLayers []ocispec.Descriptor
			var manifestLayers []ocispec.Descriptor
			// Split layers from non-layers, layers will be handled after
			// the config
			for i, child := range children {
				span.SetAttributes(
					tracing.Attribute("descriptor.child."+strconv.Itoa(i), []string{child.MediaType, child.Digest.String()}),
				)
				if images.IsLayerType(child.MediaType) {
					manifestLayers = append(manifestLayers, child)
				} else {
					nonLayers = append(nonLayers, child)
				}
			}

			lock.Lock()
			for _, nl := range nonLayers {
				layers[nl.Digest] = manifestLayers
			}
			lock.Unlock()

			children = nonLayers
		case images.MediaTypeDockerSchema2Config, ocispec.MediaTypeImageConfig:
			lock.Lock()
			l := layers[desc.Digest]
			lock.Unlock()
			if len(l) > 0 {
				u.eg.Go(func() error {
					return u.unpack(h, desc, l)
				})
			}
		}
		return children, nil
	})
}

// Wait waits for any ongoing unpack processes to complete then will return
// the result.
func (u *Unpacker) Wait() (Result, error) {
	if err := u.eg.Wait(); err != nil {
		return Result{}, err
	}
	return Result{
		Unpacks: int(u.unpacks),
	}, nil
}

// unpackStatus is used to communicate the result of a topHalf operation
// to the corresponding bottomHalf.
type unpackStatus struct {
	key    string
	mounts []mount.Mount
	diff   ocispec.Descriptor
	diffID digest.Digest
	err    error
}

// supportParallel checks whether the given platform supports parallel unpack
// by looking for the "rebase" capability.
func supportParallel(p *Platform) bool {
	for _, c := range p.SnapshotterCapabilities {
		if c == "rebase" {
			return true
		}
	}
	return false
}

func (u *Unpacker) unpack(
	h images.Handler,
	config ocispec.Descriptor,
	layers []ocispec.Descriptor,
) error {
	ctx := u.ctx
	ctx, layerSpan := tracing.StartSpan(ctx, tracing.Name(unpackSpanPrefix, "unpack"))
	defer layerSpan.End()
	unpackStart := time.Now()
	p, err := content.ReadBlob(ctx, u.content, config)
	if err != nil {
		return err
	}

	var i ocispec.Image
	if err := json.Unmarshal(p, &i); err != nil {
		return fmt.Errorf("unmarshal image config: %w", err)
	}
	diffIDs := i.RootFS.DiffIDs
	if len(layers) != len(diffIDs) {
		return fmt.Errorf("number of layers and diffIDs don't match: %d != %d", len(layers), len(diffIDs))
	}

	// TODO: Support multiple unpacks rather than just first match
	var unpack *Platform

	imgPlatform := platforms.Normalize(ocispec.Platform{OS: i.OS, Architecture: i.Architecture})
	for _, up := range u.platforms {
		if up.Platform.Match(imgPlatform) {
			unpack = up
			break
		}
	}

	if unpack == nil {
		log.G(ctx).WithField("image", config.Digest).WithField("platform", platforms.Format(imgPlatform)).Debugf("unpacker does not support platform, only fetching layers")
		return u.fetch(ctx, h, layers, nil)
	}

	atomic.AddInt32(&u.unpacks, 1)

	var (
		sn = unpack.Snapshotter
		a  = unpack.Applier
		cs = u.content

		chain []digest.Digest
	)

	// If there is an early return, ensure any ongoing
	// fetches get their context cancelled
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Check if parallel unpack is supported
	parallel := supportParallel(unpack) && u.unpackLimiter != nil

	if parallel {
		return u.unpackParallel(ctx, h, config, layers, diffIDs, unpack, unpackStart)
	}

	// Sequential unpack (original logic)
	var (
		fetchOffset int
		fetchC      []chan struct{}
		fetchErr    chan error
	)

	doUnpackFn := func(i int, desc ocispec.Descriptor) error {
		parent := identity.ChainID(chain)
		chain = append(chain, diffIDs[i])
		chainID := identity.ChainID(chain).String()

		unlock, err := u.lockSnChainID(ctx, chainID, unpack.SnapshotterKey)
		if err != nil {
			return err
		}
		defer unlock()

		// inherits annotations which are provided as snapshot labels.
		snapshotLabels := snapshots.FilterInheritedLabels(desc.Annotations)
		if snapshotLabels == nil {
			snapshotLabels = make(map[string]string)
		}
		snapshotLabels[labelSnapshotRef] = chainID

		var (
			key    string
			mounts []mount.Mount
			opts   = append(unpack.SnapshotOpts, snapshots.WithLabels(snapshotLabels))
		)

		for try := 1; try <= 3; try++ {
			// Prepare snapshot with from parent, label as root
			key = fmt.Sprintf(snapshots.UnpackKeyFormat, uniquePart(), chainID)
			mounts, err = sn.Prepare(ctx, key, parent.String(), opts...)
			if err != nil {
				if errdefs.IsAlreadyExists(err) {
					if _, err := sn.Stat(ctx, chainID); err != nil {
						if !errdefs.IsNotFound(err) {
							return fmt.Errorf("failed to stat snapshot %s: %w", chainID, err)
						}
						// Try again, this should be rare, log it
						log.G(ctx).WithField("key", key).WithField("chainid", chainID).Debug("extraction snapshot already exists, chain id not found")
					} else {
						// no need to handle, snapshot now found with chain id
						return nil
					}
				} else {
					return fmt.Errorf("failed to prepare extraction snapshot %q: %w", key, err)
				}
			} else {
				break
			}
		}
		if err != nil {
			return fmt.Errorf("unable to prepare extraction snapshot: %w", err)
		}

		// Abort the snapshot if commit does not happen
		abort := func(ctx context.Context) {
			if err := sn.Remove(ctx, key); err != nil {
				log.G(ctx).WithError(err).Errorf("failed to cleanup %q", key)
			}
		}

		if fetchErr == nil {
			fetchErr = make(chan error, 1)
			fetchOffset = i
			fetchC = make([]chan struct{}, len(layers)-fetchOffset)
			for i := range fetchC {
				fetchC[i] = make(chan struct{})
			}

			go func(i int) {
				err := u.fetch(ctx, h, layers[i:], fetchC)
				if err != nil {
					fetchErr <- err
				}
				close(fetchErr)
			}(i)
		}

		select {
		case <-ctx.Done():
			cleanup.Do(ctx, abort)
			return ctx.Err()
		case err := <-fetchErr:
			if err != nil {
				cleanup.Do(ctx, abort)
				return err
			}
		case <-fetchC[i-fetchOffset]:
		}

		diff, err := a.Apply(ctx, desc, mounts, unpack.ApplyOpts...)
		if err != nil {
			cleanup.Do(ctx, abort)
			return fmt.Errorf("failed to extract layer %s: %w", diffIDs[i], err)
		}
		if diff.Digest != diffIDs[i] {
			cleanup.Do(ctx, abort)
			return fmt.Errorf("wrong diff id calculated on extraction %q", diffIDs[i])
		}

		if err = sn.Commit(ctx, chainID, key, opts...); err != nil {
			cleanup.Do(ctx, abort)
			if errdefs.IsAlreadyExists(err) {
				return nil
			}
			return fmt.Errorf("failed to commit snapshot %s: %w", key, err)
		}

		// Set the uncompressed label after the uncompressed
		// digest has been verified through apply.
		cinfo := content.Info{
			Digest: desc.Digest,
			Labels: map[string]string{
				labels.LabelUncompressed: diff.Digest.String(),
			},
		}
		if _, err := cs.Update(ctx, cinfo, "labels."+labels.LabelUncompressed); err != nil {
			return err
		}
		return nil
	}

	for i, desc := range layers {
		_, layerSpan := tracing.StartSpan(ctx, tracing.Name(unpackSpanPrefix, "unpackLayer"))
		unpackLayerStart := time.Now()
		layerSpan.SetAttributes(
			tracing.Attribute("layer.media.type", desc.MediaType),
			tracing.Attribute("layer.media.size", desc.Size),
			tracing.Attribute("layer.media.digest", desc.Digest.String()),
		)
		if err := doUnpackFn(i, desc); err != nil {
			layerSpan.SetStatus(err)
			layerSpan.End()
			return err
		}
		layerSpan.End()
		log.G(ctx).WithFields(log.Fields{
			"layer":    desc.Digest,
			"duration": time.Since(unpackLayerStart),
		}).Debug("layer unpacked")
	}

	chainID := identity.ChainID(chain).String()
	cinfo := content.Info{
		Digest: config.Digest,
		Labels: map[string]string{
			fmt.Sprintf("containerd.io/gc.ref.snapshot.%s", unpack.SnapshotterKey): chainID,
		},
	}
	_, err = cs.Update(ctx, cinfo, fmt.Sprintf("labels.containerd.io/gc.ref.snapshot.%s", unpack.SnapshotterKey))
	if err != nil {
		return err
	}
	log.G(ctx).WithFields(log.Fields{
		"config":   config.Digest,
		"chainID":  chainID,
		"duration": time.Since(unpackStart),
	}).Debug("image unpacked")

	return nil
}

// unpackParallel implements the parallel unpack using topHalf/bottomHalf pattern.
// topHalf: For each layer, Prepare (without parent), fetch, and Apply are done
// concurrently. bottomHalf: Commit is done sequentially (with rebase to set the
// correct parent chain).
func (u *Unpacker) unpackParallel(
	ctx context.Context,
	h images.Handler,
	config ocispec.Descriptor,
	layers []ocispec.Descriptor,
	diffIDs []digest.Digest,
	unpack *Platform,
	unpackStart time.Time,
) error {
	var (
		sn = unpack.Snapshotter
		cs = u.content
	)

	// Channel for each layer to communicate topHalf result to bottomHalf
	statusC := make([]chan unpackStatus, len(layers))
	for i := range statusC {
		statusC[i] = make(chan unpackStatus, 1)
	}

	// Start topHalf for all layers concurrently
	eg, egCtx := errgroup.WithContext(ctx)
	for i, desc := range layers {
		i, desc := i, desc
		eg.Go(func() error {
			// Acquire unpack limiter
			if u.unpackLimiter != nil {
				if err := u.unpackLimiter.Acquire(egCtx, 1); err != nil {
					statusC[i] <- unpackStatus{err: err}
					return err
				}
			}

			status := u.topHalf(egCtx, h, i, desc, diffIDs, unpack)
			statusC[i] <- status

			if u.unpackLimiter != nil {
				u.unpackLimiter.Release(1)
			}

			return status.err
		})
	}

	// bottomHalf: sequentially commit each layer with the correct parent
	var chain []digest.Digest
	var bottomErr error

	for i, desc := range layers {
		var status unpackStatus
		select {
		case status = <-statusC[i]:
		case <-ctx.Done():
			bottomErr = ctx.Err()
		}

		if bottomErr != nil {
			break
		}

		if status.err != nil {
			bottomErr = status.err
			break
		}

		parent := identity.ChainID(chain)
		chain = append(chain, diffIDs[i])
		chainID := identity.ChainID(chain).String()

		// Verify diff ID
		if status.diff.Digest != diffIDs[i] {
			// Abort the snapshot
			if err := sn.Remove(ctx, status.key); err != nil {
				log.G(ctx).WithError(err).Errorf("failed to cleanup %q", status.key)
			}
			bottomErr = fmt.Errorf("wrong diff id calculated on extraction %q", diffIDs[i])
			break
		}

		// inherits annotations which are provided as snapshot labels.
		snapshotLabels := snapshots.FilterInheritedLabels(desc.Annotations)
		if snapshotLabels == nil {
			snapshotLabels = make(map[string]string)
		}
		snapshotLabels[labelSnapshotRef] = chainID

		opts := append(unpack.SnapshotOpts, snapshots.WithLabels(snapshotLabels))

		// Commit with rebase: set the parent during commit
		if parent != "" {
			opts = append(opts, snapshots.WithParent(parent.String()))
		}

		if err := sn.Commit(ctx, chainID, status.key, opts...); err != nil {
			if err2 := sn.Remove(ctx, status.key); err2 != nil {
				log.G(ctx).WithError(err2).Errorf("failed to cleanup %q", status.key)
			}
			if errdefs.IsAlreadyExists(err) {
				// Already committed by another process, continue
				log.G(ctx).WithFields(log.Fields{
					"layer":   desc.Digest,
					"chainID": chainID,
				}).Debug("layer already committed")
			} else {
				bottomErr = fmt.Errorf("failed to commit snapshot %s: %w", status.key, err)
				break
			}
		}

		// Set the uncompressed label after the uncompressed
		// digest has been verified through apply.
		cinfo := content.Info{
			Digest: desc.Digest,
			Labels: map[string]string{
				labels.LabelUncompressed: status.diff.Digest.String(),
			},
		}
		if _, err := cs.Update(ctx, cinfo, "labels."+labels.LabelUncompressed); err != nil {
			bottomErr = err
			break
		}

		log.G(ctx).WithFields(log.Fields{
			"layer":   desc.Digest,
			"chainID": chainID,
		}).Debug("layer committed (parallel)")
	}

	// Wait for all topHalf goroutines to finish
	topErr := eg.Wait()

	// Prefer returning bottomErr if set
	if bottomErr != nil {
		return bottomErr
	}
	if topErr != nil {
		return topErr
	}

	// Update config label with final chain ID
	chainID := identity.ChainID(chain).String()
	cinfo := content.Info{
		Digest: config.Digest,
		Labels: map[string]string{
			fmt.Sprintf("containerd.io/gc.ref.snapshot.%s", unpack.SnapshotterKey): chainID,
		},
	}
	_, err := cs.Update(ctx, cinfo, fmt.Sprintf("labels.containerd.io/gc.ref.snapshot.%s", unpack.SnapshotterKey))
	if err != nil {
		return err
	}
	log.G(ctx).WithFields(log.Fields{
		"config":   config.Digest,
		"chainID":  chainID,
		"duration": time.Since(unpackStart),
	}).Debug("image unpacked (parallel)")

	return nil
}

// topHalf performs the Prepare, fetch, and Apply for a single layer.
// In parallel mode, Prepare is done without a parent (empty string).
// The parent will be set during Commit (bottomHalf) via rebase.
func (u *Unpacker) topHalf(
	ctx context.Context,
	h images.Handler,
	layerIdx int,
	desc ocispec.Descriptor,
	diffIDs []digest.Digest,
	unpack *Platform,
) unpackStatus {
	sn := unpack.Snapshotter

	chain := make([]digest.Digest, layerIdx+1)
	copy(chain, diffIDs[:layerIdx+1])
	chainID := identity.ChainID(chain).String()

	unlock, err := u.lockSnChainID(ctx, chainID, unpack.SnapshotterKey)
	if err != nil {
		return unpackStatus{err: err}
	}
	defer unlock()

	// Check if already committed
	if _, err := sn.Stat(ctx, chainID); err == nil {
		// Already exists, return a status that bottomHalf can skip
		return unpackStatus{
			diff:   ocispec.Descriptor{Digest: diffIDs[layerIdx]},
			diffID: diffIDs[layerIdx],
		}
	}

	// inherits annotations which are provided as snapshot labels.
	snapshotLabels := snapshots.FilterInheritedLabels(desc.Annotations)
	if snapshotLabels == nil {
		snapshotLabels = make(map[string]string)
	}
	snapshotLabels[labelSnapshotRef] = chainID

	opts := append(unpack.SnapshotOpts, snapshots.WithLabels(snapshotLabels))

	// Prepare without parent (will be rebased during commit)
	var key string
	var mounts []mount.Mount
	for try := 1; try <= 3; try++ {
		key = fmt.Sprintf(snapshots.UnpackKeyFormat, uniquePart(), chainID)
		mounts, err = sn.Prepare(ctx, key, "", opts...)
		if err != nil {
			if errdefs.IsAlreadyExists(err) {
				if _, err := sn.Stat(ctx, chainID); err != nil {
					if !errdefs.IsNotFound(err) {
						return unpackStatus{err: fmt.Errorf("failed to stat snapshot %s: %w", chainID, err)}
					}
					log.G(ctx).WithField("key", key).WithField("chainid", chainID).Debug("extraction snapshot already exists, chain id not found")
					continue
				}
				// Already committed
				return unpackStatus{
					diff:   ocispec.Descriptor{Digest: diffIDs[layerIdx]},
					diffID: diffIDs[layerIdx],
				}
			}
			return unpackStatus{err: fmt.Errorf("failed to prepare extraction snapshot %q: %w", key, err)}
		}
		break
	}
	if err != nil {
		return unpackStatus{err: fmt.Errorf("unable to prepare extraction snapshot: %w", err)}
	}

	abort := func(ctx context.Context) {
		if err := sn.Remove(ctx, key); err != nil {
			log.G(ctx).WithError(err).Errorf("failed to cleanup %q", key)
		}
	}

	// Fetch the layer (download blob)
	fetchC := make([]chan struct{}, 1)
	fetchC[0] = make(chan struct{})
	fetchErr := make(chan error, 1)

	go func() {
		err := u.fetch(ctx, h, []ocispec.Descriptor{desc}, fetchC)
		if err != nil {
			fetchErr <- err
		}
		close(fetchErr)
	}()

	select {
	case <-ctx.Done():
		cleanup.Do(ctx, abort)
		return unpackStatus{err: ctx.Err()}
	case err := <-fetchErr:
		if err != nil {
			cleanup.Do(ctx, abort)
			return unpackStatus{err: err}
		}
	case <-fetchC[0]:
	}

	// In case of parallel unpack, the parent snapshot isn't provided to the snapshotter.
	// The overlayfs will return bind mounts for all layers, we need to convert them
	// to overlay mounts for the applier to perform whiteout conversion correctly.
	// TODO: this is a temporary workaround until #13053 lands.
	// See: https://github.com/containerd/containerd/issues/13030
	if layerIdx > 0 && unpack.SnapshotterKey == "overlayfs" {
		mounts = bindToOverlay(mounts)
	}

	// Apply the layer
	diffResult, err := unpack.Applier.Apply(ctx, desc, mounts, unpack.ApplyOpts...)
	if err != nil {
		cleanup.Do(ctx, abort)
		return unpackStatus{err: fmt.Errorf("failed to extract layer %s: %w", diffIDs[layerIdx], err)}
	}

	return unpackStatus{
		key:    key,
		mounts: mounts,
		diff:   diffResult,
		diffID: diffIDs[layerIdx],
	}
}

func (u *Unpacker) fetch(ctx context.Context, h images.Handler, layers []ocispec.Descriptor, done []chan struct{}) error {
	eg, ctx2 := errgroup.WithContext(ctx)
	for i, desc := range layers {
		ctx2, layerSpan := tracing.StartSpan(ctx2, tracing.Name(unpackSpanPrefix, "fetchLayer"))
		layerSpan.SetAttributes(
			tracing.Attribute("layer.media.type", desc.MediaType),
			tracing.Attribute("layer.media.size", desc.Size),
			tracing.Attribute("layer.media.digest", desc.Digest.String()),
		)
		desc := desc
		var ch chan struct{}
		if done != nil {
			ch = done[i]
		}

		if err := u.acquire(ctx); err != nil {
			return err
		}

		eg.Go(func() error {
			defer layerSpan.End()

			unlock, err := u.lockBlobDescriptor(ctx2, desc)
			if err != nil {
				u.release()
				return err
			}

			_, err = h.Handle(ctx2, desc)

			unlock()
			u.release()

			if err != nil && !errors.Is(err, images.ErrSkipDesc) {
				return err
			}
			if ch != nil {
				close(ch)
			}

			return nil
		})
	}

	return eg.Wait()
}

func (u *Unpacker) acquire(ctx context.Context) error {
	if u.limiter == nil {
		return nil
	}
	return u.limiter.Acquire(ctx, 1)
}

func (u *Unpacker) release() {
	if u.limiter == nil {
		return
	}
	u.limiter.Release(1)
}

func (u *Unpacker) lockSnChainID(ctx context.Context, chainID, snapshotter string) (func(), error) {
	key := u.makeChainIDKeyWithSnapshotter(chainID, snapshotter)

	if err := u.duplicationSuppressor.Lock(ctx, key); err != nil {
		return nil, err
	}
	return func() {
		u.duplicationSuppressor.Unlock(key)
	}, nil
}

func (u *Unpacker) lockBlobDescriptor(ctx context.Context, desc ocispec.Descriptor) (func(), error) {
	key := u.makeBlobDescriptorKey(desc)

	if err := u.duplicationSuppressor.Lock(ctx, key); err != nil {
		return nil, err
	}
	return func() {
		u.duplicationSuppressor.Unlock(key)
	}, nil
}

func (u *Unpacker) makeChainIDKeyWithSnapshotter(chainID, snapshotter string) string {
	return fmt.Sprintf("sn://%s/%v", snapshotter, chainID)
}

func (u *Unpacker) makeBlobDescriptorKey(desc ocispec.Descriptor) string {
	return fmt.Sprintf("blob://%v", desc.Digest)
}

func uniquePart() string {
	t := time.Now()
	var b [3]byte
	// Ignore read failures, just decreases uniqueness
	rand.Read(b[:])
	return fmt.Sprintf("%d-%s", t.Nanosecond(), base64.URLEncoding.EncodeToString(b[:]))
}

// bindToOverlay converts a single bind mount to an overlay mount.
// In parallel unpack mode, the overlayfs snapshotter returns bind mounts
// because no parent is provided during Prepare. This function converts
// them to overlay mounts so the applier can correctly handle whiteout files.
// TODO: this is a temporary workaround until #13053 lands.
func bindToOverlay(mounts []mount.Mount) []mount.Mount {
	if len(mounts) != 1 || mounts[0].Type != "bind" {
		return mounts
	}

	m := mount.Mount{
		Type:   "overlay",
		Source: "overlay",
	}
	for _, o := range mounts[0].Options {
		if o != "rbind" {
			m.Options = append(m.Options, o)
		}
	}
	m.Options = append(m.Options, "upperdir="+mounts[0].Source)

	return []mount.Mount{m}
}
