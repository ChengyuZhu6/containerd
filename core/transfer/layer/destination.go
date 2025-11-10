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

package layer

import (
	"context"
	"fmt"
	"io"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/containerd/containerd/v2/core/content"
)

// DestinationOpt defines options when configuring a layer destination
type DestinationOpt func(*Destination)

// WithLabels sets labels for the layer content
func WithLabels(labels map[string]string) DestinationOpt {
	return func(d *Destination) {
		d.labels = labels
	}
}

// NewDestination creates a new layer destination that writes to a content store
func NewDestination(cs content.Store, opts ...DestinationOpt) *Destination {
	d := &Destination{
		content: cs,
	}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// Destination represents a layer destination that writes to a content store
type Destination struct {
	content content.Store
	labels  map[string]string
}

func (d *Destination) String() string {
	return "Layer Destination (Content Store)"
}

// WriteLayer writes layer data to the content store
func (d *Destination) WriteLayer(ctx context.Context, desc ocispec.Descriptor, r io.Reader) error {
	ref := fmt.Sprintf("layer-write-%s", desc.Digest.String())

	var opts []content.Opt
	if len(d.labels) > 0 {
		opts = append(opts, content.WithLabels(d.labels))
	}

	writer, err := content.OpenWriter(ctx, d.content,
		content.WithRef(ref),
		content.WithDescriptor(desc))
	if err != nil {
		return fmt.Errorf("failed to open writer: %w", err)
	}
	defer writer.Close()

	// Copy data to writer
	if _, err := io.Copy(writer, r); err != nil {
		return fmt.Errorf("failed to write layer: %w", err)
	}

	// Commit the content
	if err := writer.Commit(ctx, desc.Size, desc.Digest, opts...); err != nil {
		return fmt.Errorf("failed to commit layer: %w", err)
	}

	return nil
}
