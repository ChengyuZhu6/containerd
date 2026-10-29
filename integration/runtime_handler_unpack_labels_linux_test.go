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

package integration

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/identity"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	criruntime "k8s.io/cri-api/pkg/apis/runtime/v1"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	coreimages "github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/integration/images"
	"github.com/containerd/containerd/v2/internal/cri/labels"
	snpkg "github.com/containerd/containerd/v2/pkg/snapshotters"
	"github.com/containerd/containerd/v2/plugins"
)

func TestRuntimeHandlerUnpackWithSnapshotLabels(t *testing.T) {
	workDir := t.TempDir()
	cfgPath := filepath.Join(workDir, "config.toml")
	cfg := `
version = 3

[plugins.'io.containerd.cri.v1.images']
  snapshotter = "overlayfs"
  disable_snapshot_annotations = false

[plugins.'io.containerd.cri.v1.runtime'.containerd]
  default_runtime_name = "runc"

[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runc]
  runtime_type = "io.containerd.runc.v2"
  snapshotter = "overlayfs"

[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.erofs]
  runtime_type = "io.containerd.runc.v2"
  snapshotter = "erofs"
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfg), 0o600))

	ctrd := newCtrdProc(t, *containerdBin, workDir, nil)
	require.NoError(t, ctrd.isReady())

	rSvc := ctrd.criRuntimeService(t)
	iSvc := ctrd.criImageService(t)

	ctrdClient, err := containerd.New(ctrd.grpcAddress(), containerd.WithDefaultNamespace(k8sNamespace))
	require.NoError(t, err)

	t.Cleanup(func() {
		if t.Failed() {
			t.Log("Dumping containerd config and logs due to test failure")
			dumpFileContent(t, ctrd.configPath())
			dumpFileContent(t, ctrd.logPath())
		}
		assert.NoError(t, ctrdClient.Close())
		cleanupPods(t, rSvc)
		assert.NoError(t, ctrd.kill(syscall.SIGTERM))
		assert.NoError(t, ctrd.wait(5*time.Minute))
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := ctrdClient.IntrospectionService().Plugins(ctx, fmt.Sprintf("type==%s,id==%s", plugins.SnapshotPlugin, "erofs"))
	require.NoError(t, err)
	if len(resp.Plugins) == 0 {
		t.Skip("erofs snapshotter plugin is not registered")
	}
	if initErr := resp.Plugins[0].InitErr; initErr != nil {
		t.Skipf("erofs snapshotter plugin is not ready: %s", initErr.Message)
	}

	nginxImage := images.Get(images.Nginx)
	pullImagesByCRI(t, iSvc, nginxImage)

	img, err := ctrdClient.GetImage(context.Background(), nginxImage)
	require.NoError(t, err)
	diffIDs, err := img.RootFS(context.Background())
	require.NoError(t, err)
	chainIDs := identity.ChainIDs(diffIDs)

	// First pod uses default runtime handler (overlayfs). No containers created.
	sb1Cfg := PodSandboxConfig("overlay-pod", "runtime-handler-unpack")
	_, err = rSvc.RunPodSandbox(sb1Cfg, "")
	require.NoError(t, err)

	// Image is pulled with overlayfs; nginx snapshots should not exist on erofs yet.
	erofsSn := ctrdClient.SnapshotService("erofs")
	for _, chainID := range chainIDs {
		_, err := erofsSn.Stat(context.Background(), chainID.String())
		assert.Truef(t, errdefs.IsNotFound(err), "expected no erofs snapshot for chainID %s before erofs container creation, got err=%v", chainID, err)
	}

	// Second pod uses erofs runtime handler. Creating nginx container should trigger
	// automatic unpack for erofs with snapshot labels.
	sb2Cfg := PodSandboxConfig("erofs-pod", "runtime-handler-unpack")
	sb2ID, err := rSvc.RunPodSandbox(sb2Cfg, "erofs")
	require.NoError(t, err)
	cn2Cfg := ContainerConfig("erofs-container", nginxImage, WithCommand("sleep", "1d"))
	cn2ID, err := rSvc.CreateContainer(sb2ID, cn2Cfg, sb2Cfg)
	require.NoError(t, err)

	for _, chainID := range chainIDs {
		snInfo, err := erofsSn.Stat(context.Background(), chainID.String())
		require.NoErrorf(t, err, "failed to stat erofs snapshot for chainID %s", chainID)
		require.NotNil(t, snInfo.Labels)
		assert.NotEmpty(t, snInfo.Labels[snpkg.TargetRefLabel], "missing %s on chainID %s", snpkg.TargetRefLabel, chainID)
		assert.NotEmpty(t, snInfo.Labels[snpkg.TargetManifestDigestLabel], "missing %s on chainID %s", snpkg.TargetManifestDigestLabel, chainID)
		assert.NotEmpty(t, snInfo.Labels[snpkg.TargetLayerDigestLabel], "missing %s on chainID %s", snpkg.TargetLayerDigestLabel, chainID)
		assert.NotEmpty(t, snInfo.Labels[snpkg.TargetImageLayersLabel], "missing %s on chainID %s", snpkg.TargetImageLayersLabel, chainID)
	}

	// Make sure the second pod really used the erofs runtime handler path.
	sb2Status, err := rSvc.PodSandboxStatus(sb2ID)
	require.NoError(t, err)
	assert.Equal(t, "erofs", sb2Status.RuntimeHandler)

	// Container should be created.
	cn2Status, err := rSvc.ContainerStatus(cn2ID)
	require.NoError(t, err)
	assert.Equal(t, criruntime.ContainerState_CONTAINER_CREATED, cn2Status.State)
}

func TestImagePlatformConsumption(t *testing.T) {
	workDir := t.TempDir()
	pauseRef := images.Get(images.Pause)
	cfg := fmt.Sprintf(`version = 3
[plugins.'io.containerd.cri.v1.images']
  snapshotter = "native"
[plugins.'io.containerd.cri.v1.images'.pinned_images]
  sandbox = %q
[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runc]
  runtime_type = "io.containerd.runc.v2"
  snapshotter = "native"
`, pauseRef)
	require.NoError(t, os.WriteFile(filepath.Join(workDir, "config.toml"), []byte(cfg), 0o600))
	ctrd := newCtrdProc(t, *containerdBin, workDir, nil)
	t.Cleanup(func() {
		assert.NoError(t, ctrd.kill(syscall.SIGTERM))
		assert.NoError(t, ctrd.wait(time.Minute))
		if t.Failed() {
			dumpFileContent(t, ctrd.logPath())
		}
	})
	require.NoError(t, ctrd.isReady())
	rSvc := ctrd.criRuntimeService(t)
	iSvc := ctrd.criImageService(t)
	client, err := containerd.New(ctrd.grpcAddress(), containerd.WithDefaultNamespace(k8sNamespace))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, client.Close()) })
	t.Cleanup(func() { cleanupPods(t, rSvc) })
	ctx, done, err := client.WithLease(context.Background())
	require.NoError(t, err)
	defer done(ctx)
	cs := client.ContentStore()
	writeJSON := func(mediaType string, value any) imagespec.Descriptor {
		data, err := json.Marshal(value)
		require.NoError(t, err)
		desc := imagespec.Descriptor{MediaType: mediaType, Digest: digest.FromBytes(data), Size: int64(len(data))}
		require.NoError(t, content.WriteBlob(ctx, cs, desc.Digest.String(), bytes.NewReader(data), desc))
		return desc
	}
	selectedPlatform := platforms.DefaultSpec()
	selectedPlatform.Architecture = "arm64"
	if platforms.DefaultSpec().Architecture == "arm64" {
		selectedPlatform.Architecture = "amd64"
	}
	selectedPlatform.Variant = ""

	for _, source := range []string{images.Get(images.BusyBox), pauseRef} {
		t.Run(source, func(t *testing.T) {
			base, err := client.Pull(ctx, source)
			require.NoError(t, err)
			manifest, err := coreimages.Manifest(ctx, cs, base.Target(), platforms.Default())
			require.NoError(t, err)
			baseSpec, err := base.Spec(ctx)
			require.NoError(t, err)
			var index imagespec.Index
			var selectedID digest.Digest
			var selectedChain string
			for _, platform := range []imagespec.Platform{platforms.DefaultSpec(), selectedPlatform} {
				var layer bytes.Buffer
				tw := tar.NewWriter(&layer)
				marker := platforms.Format(platform)
				require.NoError(t, tw.WriteHeader(&tar.Header{Name: "pr1-platform", Mode: 0o644, Size: int64(len(marker))}))
				_, err := tw.Write([]byte(marker))
				require.NoError(t, err)
				require.NoError(t, tw.Close())
				layerDesc := imagespec.Descriptor{MediaType: imagespec.MediaTypeImageLayer, Digest: digest.FromBytes(layer.Bytes()), Size: int64(layer.Len())}
				require.NoError(t, content.WriteBlob(ctx, cs, layerDesc.Digest.String(), bytes.NewReader(layer.Bytes()), layerDesc))
				spec := baseSpec
				spec.Platform = platform
				spec.RootFS.DiffIDs = append(append([]digest.Digest{}, baseSpec.RootFS.DiffIDs...), layerDesc.Digest)
				m := manifest
				m.Config = writeJSON(imagespec.MediaTypeImageConfig, spec)
				m.Layers = append(append([]imagespec.Descriptor{}, manifest.Layers...), layerDesc)
				desc := writeJSON(imagespec.MediaTypeImageManifest, m)
				desc.Platform = &platform
				index.Manifests = append(index.Manifests, desc)
				if platform.Architecture == selectedPlatform.Architecture {
					selectedID = m.Config.Digest
					selectedChain = identity.ChainID(spec.RootFS.DiffIDs).String()
				}
			}
			index.MediaType = imagespec.MediaTypeImageIndex
			index.SchemaVersion = 2
			img := coreimages.Image{Name: source, Target: writeJSON(imagespec.MediaTypeImageIndex, index), Labels: map[string]string{labels.ImagePlatformLabelKey: platforms.Format(selectedPlatform)}}
			_, err = client.ImageService().Update(ctx, img, "target", "labels."+labels.ImagePlatformLabelKey)
			require.NoError(t, err)
			require.NoError(t, Eventually(func() (bool, error) {
				image, err := iSvc.ImageStatus(&criruntime.ImageSpec{Image: source})
				return image != nil && image.Id == selectedID.String(), err
			}, 100*time.Millisecond, 10*time.Second))
			_, err = client.SnapshotService("native").Stat(ctx, selectedChain)
			require.True(t, errdefs.IsNotFound(err), "selected image must exercise unpack fallback")
			sbConfig := PodSandboxConfig("platform", "image-platform", WithHostNetwork)
			sb, err := rSvc.RunPodSandbox(sbConfig, "")
			require.NoError(t, err)
			if source == pauseRef {
				container, err := client.LoadContainer(ctx, sb)
				require.NoError(t, err)
				info, err := container.Info(ctx)
				require.NoError(t, err)
				snapshot, err := client.SnapshotService("native").Stat(ctx, info.SnapshotKey)
				require.NoError(t, err)
				assert.Equal(t, selectedChain, snapshot.Parent)
			} else {
				config := ContainerConfig("platform", source, WithCommand("sleep", "300"))
				cn, err := rSvc.CreateContainer(sb, config, sbConfig)
				require.NoError(t, err)
				require.NoError(t, rSvc.StartContainer(cn))
				stdout, stderr, err := rSvc.ExecSync(cn, []string{"cat", "/pr1-platform"}, time.Minute)
				require.NoError(t, err, string(stderr))
				assert.Equal(t, platforms.Format(selectedPlatform), string(stdout))
				status, err := rSvc.ContainerStatus(cn)
				require.NoError(t, err)
				assert.Equal(t, selectedID.String(), status.ImageRef)
			}
			require.NoError(t, rSvc.StopPodSandbox(sb))
			require.NoError(t, rSvc.RemovePodSandbox(sb))
		})
	}
}
