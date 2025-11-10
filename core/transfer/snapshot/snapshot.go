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

package snapshot

import (
	"context"
	"fmt"

	"github.com/containerd/typeurl/v2"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	transfertypes "github.com/containerd/containerd/api/types/transfer"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/core/streaming"
	"github.com/containerd/containerd/v2/core/transfer/plugins"
	"github.com/containerd/log"
)

func init() {
	plugins.Register(&transfertypes.SnapshotRef{}, &Ref{})
}

// DestinationOpt defines options when configuring a snapshot destination
type DestinationOpt func(*Destination)

// WithParent sets the parent snapshot key
func WithParent(parent string) DestinationOpt {
	return func(d *Destination) {
		d.parent = parent
	}
}

// WithLabels sets labels for the snapshot
func WithLabels(labels map[string]string) DestinationOpt {
	return func(d *Destination) {
		d.labels = labels
	}
}

// NewDestination creates a new snapshot destination
func NewDestination(snapshotter snapshots.Snapshotter, key string, opts ...DestinationOpt) *Destination {
	d := &Destination{
		snapshotter: snapshotter,
		key:         key,
	}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// Destination represents a snapshot destination for unpacking layers
type Destination struct {
	snapshotter snapshots.Snapshotter
	key         string
	parent      string
	labels      map[string]string
}

func (d *Destination) String() string {
	return fmt.Sprintf("Snapshot Destination (%s)", d.key)
}

// PrepareSnapshot prepares a snapshot for receiving layer data
func (d *Destination) PrepareSnapshot(ctx context.Context, key string, parent string) ([]mount.Mount, error) {
	var opts []snapshots.Opt
	if len(d.labels) > 0 {
		opts = append(opts, snapshots.WithLabels(d.labels))
	}
	return d.snapshotter.Prepare(ctx, key, parent, opts...)
}

// CommitSnapshot commits the snapshot
func (d *Destination) CommitSnapshot(ctx context.Context, name, key string, opts ...snapshots.Opt) error {
	return d.snapshotter.Commit(ctx, name, key, opts...)
}

// GetSnapshotter returns the snapshotter
func (d *Destination) GetSnapshotter() snapshots.Snapshotter {
	return d.snapshotter
}

// SourceOpt defines options when configuring a snapshot source
type SourceOpt func(*Source)

// WithSourceParent sets the parent snapshot key for diff
func WithSourceParent(parent string) SourceOpt {
	return func(s *Source) {
		s.parent = parent
	}
}

// NewSource creates a new snapshot source for creating diffs
func NewSource(snapshotter snapshots.Snapshotter, key string, opts ...SourceOpt) *Source {
	s := &Source{
		snapshotter: snapshotter,
		key:         key,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Source represents a snapshot source for creating layer diffs
type Source struct {
	snapshotter snapshots.Snapshotter
	key         string
	parent      string
}

func (s *Source) String() string {
	return fmt.Sprintf("Snapshot Source (%s)", s.key)
}

// GetMounts returns the mounts for the snapshot
func (s *Source) GetMounts(ctx context.Context) ([]mount.Mount, error) {
	return s.snapshotter.Mounts(ctx, s.key)
}

// GetParentMounts returns the mounts for the parent snapshot
func (s *Source) GetParentMounts(ctx context.Context) ([]mount.Mount, error) {
	if s.parent == "" {
		return nil, nil
	}
	return s.snapshotter.Mounts(ctx, s.parent)
}

// GetSnapshotter returns the snapshotter
func (s *Source) GetSnapshotter() snapshots.Snapshotter {
	return s.snapshotter
}

// Ref represents a snapshot reference that can be marshaled for RPC
type Ref struct {
	snapshotter string
	key         string
	parent      string
	labels      map[string]string
}

// NewRef creates a new snapshot reference
func NewRef(snapshotter, key, parent string, labels map[string]string) *Ref {
	return &Ref{
		snapshotter: snapshotter,
		key:         key,
		parent:      parent,
		labels:      labels,
	}
}

func (r *Ref) String() string {
	return fmt.Sprintf("Snapshot Ref (%s:%s)", r.snapshotter, r.key)
}

// MarshalAny marshals the snapshot reference for transfer over RPC
func (r *Ref) MarshalAny(ctx context.Context, sm streaming.StreamCreator) (typeurl.Any, error) {
	sr := &transfertypes.SnapshotRef{
		Snapshotter: r.snapshotter,
		Key:         r.key,
		Parent:      r.parent,
		Labels:      r.labels,
	}
	return typeurl.MarshalAny(sr)
}

// UnmarshalAny unmarshals the snapshot reference from RPC
func (r *Ref) UnmarshalAny(ctx context.Context, sm streaming.StreamGetter, a typeurl.Any) error {
	var sr transfertypes.SnapshotRef
	if err := typeurl.UnmarshalTo(a, &sr); err != nil {
		return err
	}

	r.snapshotter = sr.Snapshotter
	r.key = sr.Key
	r.parent = sr.Parent
	r.labels = sr.Labels

	log.G(ctx).WithFields(log.Fields{
		"snapshotter": r.snapshotter,
		"key":         r.key,
		"parent":      r.parent,
	}).Debug("unmarshaled snapshot reference")

	return nil
}
