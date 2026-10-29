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
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/opencontainers/image-spec/identity"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	criruntime "k8s.io/cri-api/pkg/apis/runtime/v1"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/integration/images"
	imagestore "github.com/containerd/containerd/v2/internal/cri/store/image"
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

	metadata := img.Metadata()
	metadata.Name = imagestore.Name(nginxImage, "erofs")
	_, err = ctrdClient.ImageService().Create(context.Background(), metadata)
	require.NoError(t, err)
	require.NoError(t, Eventually(func() (bool, error) {
		image, err := iSvc.ImageStatus(&criruntime.ImageSpec{Image: nginxImage, RuntimeHandler: "erofs"})
		return image != nil, err
	}, 100*time.Millisecond, 10*time.Second))

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

func TestRuntimeHandlerImageLifecycle(t *testing.T) {
	for _, local := range []bool{true, false} {
		for _, snapshotter := range []string{"overlayfs", "native"} {
			t.Run(fmt.Sprintf("local=%t/b=%s", local, snapshotter), func(t *testing.T) {
				workDir := t.TempDir()
				cfg := fmt.Sprintf(`version = 3
[plugins.'io.containerd.cri.v1.images']
  use_local_image_pull = %t
[plugins.'io.containerd.cri.v1.images'.pinned_images]
  sandbox = %q
[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.a]
  runtime_type = "io.containerd.runc.v2"
  snapshotter = "overlayfs"
[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.b]
  runtime_type = "io.containerd.runc.v2"
  snapshotter = %q
`, local, images.Get(images.Pause), snapshotter)
				require.NoError(t, os.WriteFile(filepath.Join(workDir, "config.toml"), []byte(cfg), 0o600))
				ctrd := newCtrdProc(t, *containerdBin, workDir, nil)
				require.NoError(t, ctrd.isReady())
				rSvc := ctrd.criRuntimeService(t)
				iSvc := ctrd.criImageService(t)
				client, err := containerd.New(ctrd.grpcAddress(), containerd.WithDefaultNamespace(k8sNamespace))
				require.NoError(t, err)
				t.Cleanup(func() {
					cleanupPods(t, rSvc)
					assert.NoError(t, client.Close())
					assert.NoError(t, ctrd.kill(syscall.SIGTERM))
					assert.NoError(t, ctrd.wait(time.Minute))
					if t.Failed() {
						dumpFileContent(t, ctrd.logPath())
					}
				})
				ctx := context.Background()
				ref := images.Get(images.BusyBox)
				legacy, err := client.Pull(ctx, ref)
				require.NoError(t, err)
				config, err := legacy.Config(ctx)
				require.NoError(t, err)
				id := config.Digest.String()
				require.NoError(t, Eventually(func() (bool, error) {
					image, err := iSvc.ImageStatus(&criruntime.ImageSpec{Image: ref})
					return image != nil && image.Spec.GetRuntimeHandler() == "runc", err
				}, 100*time.Millisecond, 10*time.Second))
				missing, err := iSvc.ImageStatus(&criruntime.ImageSpec{Image: id, RuntimeHandler: "a"})
				require.NoError(t, err)
				require.Nil(t, missing)
				_, err = iSvc.ImageStatus(&criruntime.ImageSpec{Image: ref, RuntimeHandler: "unknown"})
				require.Error(t, err)

				pods := make(map[string]*criruntime.PodSandboxConfig)
				sandboxes := make(map[string]string)
				containers := make(map[string]string)
				create := func(handler, name string) string {
					cfg := ContainerConfig(name, id, WithCommand("sleep", "300"))
					cn, err := rSvc.CreateContainer(sandboxes[handler], cfg, pods[handler])
					require.NoError(t, err)
					require.NoError(t, rSvc.StartContainer(cn))
					stdout, stderr, err := rSvc.ExecSync(cn, []string{"echo", "running"}, time.Minute)
					require.NoError(t, err, string(stderr))
					require.Equal(t, "running\n", string(stdout))
					status, err := rSvc.ContainerStatus(cn)
					require.NoError(t, err)
					require.Equal(t, id, status.ImageId)
					require.Equal(t, handler, status.Image.GetRuntimeHandler())
					return cn
				}
				for _, handler := range []string{"a", "b"} {
					pulled, err := iSvc.PullImage(&criruntime.ImageSpec{Image: ref}, nil, nil, handler)
					require.NoError(t, err)
					require.Equal(t, id, pulled)
					pods[handler] = PodSandboxConfig(handler, "handler-images", WithHostNetwork)
					sandboxes[handler], err = rSvc.RunPodSandbox(pods[handler], handler)
					require.NoError(t, err)
					containers[handler] = create(handler, "before-restart")
				}
				listed, err := iSvc.ListImages(nil)
				require.NoError(t, err)
				handlers := make(map[string]bool)
				for _, image := range listed {
					if image.Id == id {
						handlers[image.Spec.GetRuntimeHandler()] = true
						require.Contains(t, image.RepoTags, ref)
					}
				}
				require.Equal(t, map[string]bool{"runc": true, "a": true, "b": true}, handlers)
				conn, err := grpc.NewClient("unix://"+ctrd.grpcAddress(), grpc.WithTransportCredentials(insecure.NewCredentials()))
				require.NoError(t, err)
				t.Cleanup(func() { assert.NoError(t, conn.Close()) })
				filter := &criruntime.ImageFilter{Image: &criruntime.ImageSpec{Image: id, RuntimeHandler: "b"}}
				filtered, err := iSvc.ListImages(filter)
				require.NoError(t, err)
				require.Len(t, filtered, 1)
				stream, err := criruntime.NewImageServiceClient(conn).StreamImages(ctx, &criruntime.StreamImagesRequest{Filter: filter})
				require.NoError(t, err)
				batch, err := stream.Recv()
				require.NoError(t, err)
				require.Len(t, batch.Images, 1)
				require.Equal(t, "b", batch.Images[0].Spec.GetRuntimeHandler())
				_, err = stream.Recv()
				require.ErrorIs(t, err, io.EOF)

				alias := "example.com/handler-alias:latest"
				metadata, err := client.ImageService().Get(ctx, imagestore.Name(ref, "b"))
				require.NoError(t, err)
				metadata.Name = imagestore.Name(alias, "b")
				_, err = client.ImageService().Create(ctx, metadata)
				require.NoError(t, err)
				require.NoError(t, Eventually(func() (bool, error) {
					image, err := iSvc.ImageStatus(&criruntime.ImageSpec{Image: alias, RuntimeHandler: "b"})
					return image != nil && image.Id == id, err
				}, 100*time.Millisecond, 10*time.Second))
				require.NoError(t, client.ImageService().Delete(ctx, metadata.Name))
				require.NoError(t, Eventually(func() (bool, error) {
					image, err := iSvc.ImageStatus(&criruntime.ImageSpec{Image: alias, RuntimeHandler: "b"})
					return image == nil, err
				}, 100*time.Millisecond, 10*time.Second))
				require.NoError(t, ctrd.kill(syscall.SIGTERM))
				require.NoError(t, ctrd.wait(time.Minute))
				ctrd = newCtrdProc(t, *containerdBin, workDir, nil)
				require.NoError(t, ctrd.isReady())
				for _, handler := range []string{"a", "b"} {
					image, err := iSvc.ImageStatus(&criruntime.ImageSpec{Image: id, RuntimeHandler: handler})
					require.NoError(t, err)
					require.NotNil(t, image)
					status, err := rSvc.ContainerStatus(containers[handler])
					require.NoError(t, err)
					require.Equal(t, handler, status.Image.GetRuntimeHandler())
					stdout, _, err := rSvc.ExecSync(containers[handler], []string{"echo", "recovered"}, time.Minute)
					require.NoError(t, err)
					require.Equal(t, "recovered\n", string(stdout))
				}
				listedContainers, err := rSvc.ListContainers(nil)
				require.NoError(t, err)
				for _, container := range listedContainers {
					if container.ImageId == id {
						require.Contains(t, handlers, container.Image.GetRuntimeHandler())
					}
				}
				require.NoError(t, iSvc.RemoveImage(&criruntime.ImageSpec{Image: id}))
				image, err := iSvc.ImageStatus(&criruntime.ImageSpec{Image: id, RuntimeHandler: "runc"})
				require.NoError(t, err)
				require.Nil(t, image)
				require.NoError(t, iSvc.RemoveImage(&criruntime.ImageSpec{Image: id, RuntimeHandler: "a"}))
				require.NoError(t, iSvc.RemoveImage(&criruntime.ImageSpec{Image: id, RuntimeHandler: "a"}))
				image, err = iSvc.ImageStatus(&criruntime.ImageSpec{Image: id, RuntimeHandler: "b"})
				require.NoError(t, err)
				require.NotNil(t, image)
				create("b", "after-delete-without-pull")
				cleanupPods(t, rSvc)
				require.NoError(t, iSvc.RemoveImage(&criruntime.ImageSpec{Image: id, RuntimeHandler: "b"}))
				require.NoError(t, Eventually(func() (bool, error) {
					_, err := client.ContentStore().Info(ctx, config.Digest)
					return errdefs.IsNotFound(err), nil
				}, 100*time.Millisecond, 10*time.Second))
			})
		}
	}
}
