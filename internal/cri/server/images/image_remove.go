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

package images

import (
	"context"
	"fmt"
	"slices"

	"github.com/containerd/containerd/v2/core/images"
	crilabels "github.com/containerd/containerd/v2/internal/cri/labels"
	imagestore "github.com/containerd/containerd/v2/internal/cri/store/image"
	"github.com/containerd/containerd/v2/pkg/tracing"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/containerd/platforms"

	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// RemoveImage removes the image.
// TODO(random-liu): Update CRI to pass image reference instead of ImageSpec. (See
// kubernetes/kubernetes#46255)
// TODO(random-liu): We should change CRI to distinguish image id and image spec.
// Remove the whole image no matter the it's image id or reference. This is the
// semantic defined in CRI now.
func (c *GRPCCRIImageService) RemoveImage(ctx context.Context, r *runtime.RemoveImageRequest) (*runtime.RemoveImageResponse, error) {
	err := c.CRIImageService.RemoveImage(ctx, r.GetImage())
	if err != nil && !errdefs.IsNotFound(err) {
		return nil, err
	}
	return &runtime.RemoveImageResponse{}, nil
}

func (c *CRIImageService) RemoveImage(ctx context.Context, imageSpec *runtime.ImageSpec) error {
	span := tracing.SpanFromContext(ctx)

	h, err := c.resolveRuntimeHandler(imageSpec.GetRuntimeHandler())
	if err != nil {
		return err
	}
	c.imageLifecycleMutex.Lock()
	defer c.imageLifecycleMutex.Unlock()
	if err := c.imageMutationLocker.Lock(ctx, h.Name); err != nil {
		return err
	}
	defer c.imageMutationLocker.Unlock(h.Name)
	image, err := c.LocalResolve(imageSpec.GetImage(), imageSpec.GetRuntimeHandler())
	if err != nil {
		if errdefs.IsNotFound(err) {
			span.AddEvent(err.Error())
			// return empty without error when image not found.
			return nil
		}
		return fmt.Errorf("can not resolve %q locally: %w", imageSpec.GetImage(), err)
	}
	span.SetAttributes(tracing.Attribute("image.id", image.ID))
	persisted, err := c.images.List(ctx)
	if err != nil {
		return err
	}
	var references []images.Image
	configs := make(map[[3]string]string)
	for _, img := range persisted {
		_, handler, err := imagestore.ParseName(img.Name)
		if err != nil || (handler != h.Name && (handler != "" || h.Name != c.defaultRuntimeName)) {
			continue
		}
		matcher, err := imagePlatformFromLabels(img.Labels)
		if err != nil {
			log.G(ctx).WithError(err).Warnf("Skipping image %q with invalid platform metadata", img.Name)
			continue
		}
		if matcher == nil {
			matcher = platforms.Default()
		}
		key := [3]string{img.Target.MediaType, img.Target.Digest.String(), img.Labels[crilabels.ImagePlatformLabelKey]}
		configID, ok := configs[key]
		if !ok {
			config, err := img.Config(ctx, c.content, matcher)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if errdefs.IsNotFound(err) {
					continue
				}
				if slices.Contains(image.References, img.Name) {
					return err
				}
				log.G(ctx).WithError(err).Warnf("Skipping unrelated image %q with invalid manifest", img.Name)
				continue
			}
			configID = config.Digest.String()
			configs[key] = configID
		}
		if configID == image.ID {
			references = append(references, img)
		}
	}
	// Remove all image references.
	for i, img := range references {
		ref := img.Name
		opts := []images.DeleteOpt{images.DeleteTarget(&img.Target)}
		if i == len(references)-1 {
			// Delete the last image reference synchronously to trigger garbage collection.
			// This is best effort. It is possible that the image reference is deleted by
			// someone else before this point.
			opts = append(opts, images.SynchronousDelete())
		}
		err = c.images.Delete(ctx, ref, opts...)
		if err == nil || errdefs.IsNotFound(err) {
			// Update image store to reflect the newest state in containerd.
			if err := c.imageStore.Update(ctx, ref); err != nil {
				return fmt.Errorf("failed to update image reference %q for %q: %w", ref, image.ID, err)
			}
			continue
		}
		return fmt.Errorf("failed to delete image reference %q for %q: %w", ref, image.ID, err)
	}
	return nil
}
