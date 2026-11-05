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

package opts

import (
	"context"
	"fmt"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	crilabels "github.com/containerd/containerd/v2/internal/cri/labels"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// ImageForConfig selects a manifest for a previously resolved config ID.
// It does not change the persisted image target.
func ImageForConfig(ctx context.Context, client *containerd.Client, image images.Image, configID string) (containerd.Image, error) {
	matcher := containerd.NewImage(client, image).Platform()
	if value := image.Labels[crilabels.ImagePlatformLabelKey]; value != "" {
		platform, err := platforms.Parse(value)
		if err != nil {
			return nil, err
		}
		matcher = platforms.Only(platform)
	}
	var manifest ocispec.Descriptor
	var complete bool
	err := images.Walk(ctx, images.LimitManifests(images.HandlerFunc(func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		if complete {
			return nil, nil
		}
		if images.IsManifestType(desc.MediaType) {
			config, err := images.Config(ctx, client.ContentStore(), desc, nil)
			if err != nil {
				if errdefs.IsNotFound(err) {
					return nil, nil
				}
				return nil, err
			}
			if config.Digest.String() == configID {
				if manifest.MediaType == "" {
					manifest = desc
				}
				if desc.Digest == image.Target.Digest {
					complete = true
					return nil, nil
				}
				_, _, _, missing, err := images.Check(ctx, client.ContentStore(), desc, nil)
				if err != nil {
					return nil, err
				}
				if len(missing) == 0 {
					manifest = desc
					complete = true
				}
			}
			return nil, nil
		}
		children, err := images.Children(ctx, client.ContentStore(), desc)
		if errdefs.IsNotFound(err) && desc.Digest != image.Target.Digest {
			return nil, nil
		}
		return children, err
	}), matcher, 0), image.Target)
	if err != nil {
		return nil, err
	}
	if manifest.MediaType == "" {
		return nil, fmt.Errorf("image %q has no manifest for config %q: %w", image.Name, configID, errdefs.ErrNotFound)
	}
	image.Target = manifest
	return containerd.NewImageWithPlatform(client, image, platforms.All), nil
}
