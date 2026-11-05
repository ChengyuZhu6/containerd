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
	"bytes"
	"compress/gzip"
	"context"
	"testing"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/images/imagetest"
	crilabels "github.com/containerd/containerd/v2/internal/cri/labels"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImageForConfigVariants(t *testing.T) {
	ctx := context.Background()
	cs := imagetest.NewContentStore(ctx, t)
	client, err := containerd.New("", containerd.WithServices(containerd.WithContentStore(cs.Store)),
		containerd.WithDefaultPlatform(platforms.OnlyStrict(ocispec.Platform{OS: "linux", Architecture: "amd64"})))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, client.Close()) })
	var manifests []imagetest.Content
	var configs []imagetest.Content
	for _, variant := range []string{"v6", "v7"} {
		config := cs.JSONObject(ocispec.MediaTypeImageConfig, ocispec.Image{
			Platform: ocispec.Platform{OS: "linux", Architecture: "arm"},
			RootFS:   ocispec.RootFS{Type: "layers", DiffIDs: []digest.Digest{digest.FromString(variant)}},
		})
		configs = append(configs, config)
		manifests = append(manifests, imagetest.AddPlatform(cs.Manifest(config), ocispec.Platform{OS: "linux", Architecture: "arm", Variant: variant}))
	}
	index := cs.Index(manifests...)
	stored := images.Image{Name: "example.com/image:latest", Target: index.Descriptor}
	for n, config := range configs {
		image, err := ImageForConfig(ctx, client, stored, config.Descriptor.Digest.String())
		require.NoError(t, err)
		got, err := image.Config(ctx)
		require.NoError(t, err)
		assert.Equal(t, config.Descriptor.Digest, got.Digest)
		assert.Equal(t, manifests[n].Descriptor, image.Target())
		diffIDs, err := image.RootFS(ctx)
		require.NoError(t, err)
		assert.Equal(t, []digest.Digest{digest.FromString([]string{"v6", "v7"}[n])}, diffIDs)
	}
	assert.Equal(t, index.Descriptor, stored.Target)
	_, err = ImageForConfig(ctx, client, stored, digest.FromString("missing").String())
	require.ErrorIs(t, err, errdefs.ErrNotFound)

	for _, test := range []struct {
		name     string
		platform string
		label    string
		missing  []int
		same     bool
		target   int
		want     int
	}{
		{name: "client preference", platform: "linux/arm/v7", target: -1, want: 1},
		{name: "label preference", platform: "linux/arm/v6", label: "linux/arm/v7", target: -1, want: 1},
		{name: "label compatibility", platform: "linux/amd64", label: "linux/arm/v8", target: -1, want: 1},
		{name: "non-host config", platform: "linux/amd64", target: -1, want: 0},
		{name: "selected target", platform: "linux/amd64", target: 1, want: 1},
		{name: "missing first layer", platform: "linux/arm/v6", missing: []int{0}, target: -1, want: 1},
		{name: "missing preferred layer", platform: "linux/arm/v7", missing: []int{1}, target: -1, want: 0},
		{name: "all layers missing", platform: "linux/arm/v7", missing: []int{0, 1}, target: -1, want: 1},
		{name: "selected target layer missing", platform: "linux/amd64", missing: []int{1}, target: 1, want: 1},
		{name: "same platform ordering", platform: "linux/arm/v7", same: true, target: -1, want: 0},
		{name: "same platform missing first layer", platform: "linux/arm/v7", missing: []int{0}, same: true, target: -1, want: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			cs := imagetest.NewContentStore(ctx, t)
			platform, err := platforms.Parse(test.platform)
			require.NoError(t, err)
			client, err := containerd.New("", containerd.WithServices(containerd.WithContentStore(cs.Store)),
				containerd.WithDefaultPlatform(platforms.Only(platform)))
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, client.Close()) })
			config := cs.JSONObject(ocispec.MediaTypeImageConfig, ocispec.Image{
				Platform: ocispec.Platform{OS: "linux", Architecture: "arm"},
				RootFS:   ocispec.RootFS{Type: "layers", DiffIDs: []digest.Digest{digest.FromString("shared layer")}},
			})
			var manifests []imagetest.Content
			var layers []imagetest.Content
			for n, level := range []int{gzip.BestSpeed, gzip.BestCompression} {
				var compressed bytes.Buffer
				writer, err := gzip.NewWriterLevel(&compressed, level)
				require.NoError(t, err)
				_, err = writer.Write([]byte("shared layer"))
				require.NoError(t, err)
				require.NoError(t, writer.Close())
				layer := cs.Blob(ocispec.MediaTypeImageLayerGzip, compressed.Bytes())
				layers = append(layers, layer)
				manifests = append(manifests, imagetest.AddPlatform(cs.Manifest(config, layer),
					ocispec.Platform{OS: "linux", Architecture: "arm", Variant: []string{"v6", "v7"}[n]}))
			}
			require.NotEqual(t, layers[0].Descriptor.Digest, layers[1].Descriptor.Digest)
			if test.same {
				manifests[0].Descriptor.Platform = manifests[1].Descriptor.Platform
			}
			for _, n := range test.missing {
				require.NoError(t, cs.Delete(ctx, layers[n].Descriptor.Digest))
			}
			index := cs.Index(manifests...)
			stored := images.Image{Name: "example.com/image:latest", Target: index.Descriptor,
				Labels: map[string]string{crilabels.ImagePlatformLabelKey: test.label}}
			if test.target >= 0 {
				stored.Target = manifests[test.target].Descriptor
			}
			image, err := ImageForConfig(ctx, client, stored, config.Descriptor.Digest.String())
			require.NoError(t, err)
			assert.Equal(t, manifests[test.want].Descriptor, image.Target())
			got, err := image.Config(ctx)
			require.NoError(t, err)
			assert.Equal(t, config.Descriptor.Digest, got.Digest)
			diffIDs, err := image.RootFS(ctx)
			require.NoError(t, err)
			assert.Equal(t, []digest.Digest{digest.FromString("shared layer")}, diffIDs)
			assert.Equal(t, test.label, stored.Labels[crilabels.ImagePlatformLabelKey])
			if test.target < 0 {
				assert.Equal(t, index.Descriptor, stored.Target)
			}
		})
	}
}
