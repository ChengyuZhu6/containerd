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

package converter

import (
	"context"
	"testing"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/images/imagetest"
	"github.com/containerd/containerd/v2/pkg/labels"
	"github.com/containerd/platforms"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
)

func TestConvertIndexAddsErofsOSFeature(t *testing.T) {
	ctx := t.Context()
	cs := imagetest.NewContentStore(ctx, t)

	layer := cs.Blob(ocispec.MediaTypeImageLayer, []byte("layer-data"))
	config := cs.JSONObject(ocispec.MediaTypeImageConfig, ocispec.Image{
		Platform: ocispec.Platform{
			OS:           "linux",
			Architecture: "amd64",
		},
		RootFS: ocispec.RootFS{
			Type:    "layers",
			DiffIDs: []digest.Digest{layer.Descriptor.Digest},
		},
	})
	manifest := imagetest.AddPlatform(cs.Manifest(config, layer), ocispec.Platform{
		OS:           "linux",
		Architecture: "amd64",
	})
	index := cs.Index(manifest)

	convertFn := DefaultIndexConvertFunc(func(ctx context.Context, store content.Store, desc ocispec.Descriptor) (*ocispec.Descriptor, error) {
		if desc.MediaType != ocispec.MediaTypeImageLayer {
			return nil, nil
		}

		info, err := store.Info(ctx, desc.Digest)
		if err != nil {
			return nil, err
		}
		if info.Labels == nil {
			info.Labels = map[string]string{}
		}
		info.Labels[labels.LabelUncompressed] = desc.Digest.String()
		if _, err := store.Update(ctx, info, "labels"); err != nil {
			return nil, err
		}

		newDesc := desc
		newDesc.MediaType = images.MediaTypeErofsLayer
		return &newDesc, nil
	}, false, platforms.All)

	newIndexDesc, err := convertFn(ctx, cs.Store, index.Descriptor)
	require.NoError(t, err)
	require.NotNil(t, newIndexDesc)

	var convertedIndex ocispec.Index
	_, err = readJSON(ctx, cs.Store, &convertedIndex, *newIndexDesc)
	require.NoError(t, err)
	require.Len(t, convertedIndex.Manifests, 1)
	require.NotNil(t, convertedIndex.Manifests[0].Platform)
	require.Equal(t, []string{"erofs"}, convertedIndex.Manifests[0].Platform.OSFeatures)
}
