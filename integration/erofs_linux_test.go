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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	criruntime "k8s.io/cri-api/pkg/apis/runtime/v1"

	containerd "github.com/containerd/containerd/v2/client"
	coreimages "github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/images/converter"
	"github.com/containerd/containerd/v2/core/images/converter/erofs"
	"github.com/containerd/containerd/v2/integration/images"
	"github.com/containerd/containerd/v2/internal/fsverity"
	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
)

const erofsID = "erofs"

type erofsCtrd struct {
	proc   *ctrdProc
	client *containerd.Client
}

// newEROFSCtrd starts a containerd with the erofs snapshotter/differ enabled.
// snExtras / differExtras are TOML fragments placed under the respective
// plugin stanzas. The test is skipped when either plugin is not ready.
func newEROFSCtrd(t *testing.T, snExtras, differExtras string) *erofsCtrd {
	t.Helper()

	workDir := t.TempDir()
	cfg := fmt.Sprintf(`
version = 3

[plugins.'io.containerd.cri.v1.images']
  snapshotter = "erofs"
  disable_snapshot_annotations = false

[plugins.'io.containerd.cri.v1.runtime'.containerd]
  default_runtime_name = "runc"

[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runc]
  runtime_type = "io.containerd.runc.v2"
  snapshotter = "erofs"

[plugins.'io.containerd.service.v1.diff-service']
  default = ["erofs", "walking"]

[plugins.'io.containerd.snapshotter.v1.erofs']
%s

[plugins.'io.containerd.differ.v1.erofs']
%s
`, snExtras, differExtras)

	require.NoError(t, os.WriteFile(filepath.Join(workDir, "config.toml"), []byte(cfg), 0o600))

	proc := newCtrdProc(t, *containerdBin, workDir, nil)
	require.NoError(t, proc.isReady())

	client, err := containerd.New(proc.grpcAddress(), containerd.WithDefaultNamespace(k8sNamespace))
	require.NoError(t, err)

	t.Cleanup(func() {
		if t.Failed() {
			dumpFileContent(t, proc.logPath())
		}
		_ = client.Close()
		cleanupPods(t, proc.criRuntimeService(t))
		_ = proc.kill(syscall.SIGTERM)
		_ = proc.wait(5 * time.Minute)
	})

	skipIfEROFSNotReady(t, client)
	return &erofsCtrd{proc: proc, client: client}
}

func skipIfEROFSNotReady(t *testing.T, client *containerd.Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, p := range []struct{ typ, id string }{
		{string(plugins.SnapshotPlugin), erofsID},
		{string(plugins.DiffPlugin), erofsID},
	} {
		resp, err := client.IntrospectionService().Plugins(ctx, fmt.Sprintf("type==%s,id==%s", p.typ, p.id))
		require.NoError(t, err)
		if len(resp.Plugins) == 0 {
			t.Skipf("plugin %s/%s not registered", p.typ, p.id)
		}
		if e := resp.Plugins[0].InitErr; e != nil {
			t.Skipf("plugin %s/%s not ready: %s", p.typ, p.id, e.Message)
		}
	}
}

func erofsSnapshotsDir(proc *ctrdProc) string {
	return filepath.Join(proc.rootPath(), string(plugins.SnapshotPlugin)+".erofs", "snapshots")
}

func listEROFSSnapshotDirs(t *testing.T, proc *ctrdProc) []string {
	dir := erofsSnapshotsDir(proc)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		require.NoError(t, err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, filepath.Join(dir, e.Name()))
		}
	}
	return dirs
}

// assertEROFSArtifacts walks the erofs snapshot root and asserts that every
// snapshot dir has the `.erofslayer` marker + fs/ + work/, and that at least
// one dir carries layer.erofs (and optionally its .dmverity sidecar).
func assertEROFSArtifacts(t *testing.T, proc *ctrdProc, needDmverity bool) {
	dirs := listEROFSSnapshotDirs(t, proc)
	require.NotEmpty(t, dirs, "no erofs snapshots under %s", erofsSnapshotsDir(proc))

	sawLayer, sawDmverity := false, false
	for _, d := range dirs {
		assert.FileExists(t, filepath.Join(d, ".erofslayer"))
		assert.DirExists(t, filepath.Join(d, "fs"))
		assert.DirExists(t, filepath.Join(d, "work"))
		if _, err := os.Stat(filepath.Join(d, "layer.erofs")); err == nil {
			sawLayer = true
		}
		if _, err := os.Stat(filepath.Join(d, "layer.erofs.dmverity")); err == nil {
			sawDmverity = true
		}
	}
	assert.True(t, sawLayer, "no layer.erofs blob under %s", erofsSnapshotsDir(proc))
	if needDmverity {
		assert.True(t, sawDmverity, "no layer.erofs.dmverity sidecar under %s", erofsSnapshotsDir(proc))
	}
}

// runEROFSBusyboxContainer pulls busybox and starts a sleeping container.
func runEROFSBusyboxContainer(t *testing.T, ctrd *erofsCtrd, podName, ctrName string) (sbID, cnID string) {
	rSvc := ctrd.proc.criRuntimeService(t)
	imageName := images.Get(images.BusyBox)
	pullImagesByCRI(t, ctrd.proc.criImageService(t), imageName)

	sbCfg := PodSandboxConfig(podName, "erofs-integration")
	sbID, err := rSvc.RunPodSandbox(sbCfg, "")
	require.NoError(t, err)

	cnCfg := ContainerConfig(ctrName, imageName, WithCommand("sleep", "1d"))
	cnID, err = rSvc.CreateContainer(sbID, cnCfg, sbCfg)
	require.NoError(t, err)
	require.NoError(t, rSvc.StartContainer(cnID))
	return sbID, cnID
}

func TestEROFSContainerLifecycle(t *testing.T) {
	ctrd := newEROFSCtrd(t, "", "")
	rSvc := ctrd.proc.criRuntimeService(t)

	_, cnID := runEROFSBusyboxContainer(t, ctrd, "erofs-lifecycle-pod", "erofs-lifecycle-ctr")

	stdout, stderr, err := rSvc.ExecSync(cnID, []string{"/bin/ls", "/"}, 10*time.Second)
	require.NoError(t, err, "stderr=%q", string(stderr))
	assert.Contains(t, string(stdout), "bin")

	assertEROFSArtifacts(t, ctrd.proc, false)
}

func TestEROFSContainerdRestart(t *testing.T) {
	ctrd := newEROFSCtrd(t, "", "")
	sbID, cnID := runEROFSBusyboxContainer(t, ctrd, "erofs-restart-pod", "erofs-restart-ctr")

	require.NoError(t, ctrd.proc.kill(syscall.SIGTERM))
	require.NoError(t, ctrd.proc.wait(2*time.Minute))

	restarted := newCtrdProc(t, *containerdBin, ctrd.proc.workDir, nil)
	require.NoError(t, restarted.isReady())
	t.Cleanup(func() {
		_ = restarted.kill(syscall.SIGTERM)
		_ = restarted.wait(2 * time.Minute)
	})

	rSvc := restarted.criRuntimeService(t)
	_, err := rSvc.ContainerStatus(cnID)
	require.NoError(t, err)

	cn2Cfg := ContainerConfig("erofs-restart-ctr-2", images.Get(images.BusyBox), WithCommand("sleep", "1d"))
	cn2ID, err := rSvc.CreateContainer(sbID, cn2Cfg, PodSandboxConfig("erofs-restart-pod", "erofs-integration"))
	require.NoError(t, err)
	require.NoError(t, rSvc.StartContainer(cn2ID))

	_, _, err = rSvc.ExecSync(cn2ID, []string{"/bin/true"}, 10*time.Second)
	require.NoError(t, err)
}

func TestEROFSSnapshotGC(t *testing.T) {
	ctrd := newEROFSCtrd(t, "", "")
	rSvc := ctrd.proc.criRuntimeService(t)
	iSvc := ctrd.proc.criImageService(t)

	imageName := images.Get(images.BusyBox)
	sbID, cnID := runEROFSBusyboxContainer(t, ctrd, "erofs-gc-pod", "erofs-gc-ctr")
	require.NotEmpty(t, listEROFSSnapshotDirs(t, ctrd.proc))

	require.NoError(t, rSvc.StopContainer(cnID, 0))
	require.NoError(t, rSvc.RemoveContainer(cnID))
	require.NoError(t, rSvc.StopPodSandbox(sbID))
	require.NoError(t, rSvc.RemovePodSandbox(sbID))
	require.NoError(t, iSvc.RemoveImage(&criruntime.ImageSpec{Image: imageName}))

	// Pause image snapshots may remain; just ensure busybox layers are gone.
	err := Eventually(func() (bool, error) {
		return len(listEROFSSnapshotDirs(t, ctrd.proc)) <= 1, nil
	}, time.Second, 60*time.Second)
	require.NoError(t, err, "erofs snapshots were not garbage collected")
}

func TestEROFSFsverity(t *testing.T) {
	if supported, err := fsverity.IsSupported(t.TempDir()); !supported || err != nil {
		t.Skipf("fsverity not supported: supported=%v err=%v", supported, err)
	}

	ctrd := newEROFSCtrd(t, "  enable_fsverity = true\n", "")
	runEROFSBusyboxContainer(t, ctrd, "erofs-fsverity-pod", "erofs-fsverity-ctr")

	sawVerity := false
	for _, d := range listEROFSSnapshotDirs(t, ctrd.proc) {
		blob := filepath.Join(d, "layer.erofs")
		if _, err := os.Stat(blob); err != nil {
			continue
		}
		if enabled, _ := fsverity.IsEnabled(blob); enabled {
			sawVerity = true
			break
		}
	}
	assert.True(t, sawVerity, "fsverity not enabled on any layer.erofs blob")
}

func TestEROFSDmverity(t *testing.T) {
	t.Run("auto", func(t *testing.T) {
		ctrd := newEROFSCtrd(t, "  dmverity_mode = \"auto\"\n", "  enable_dmverity = true\n")
		_, cnID := runEROFSBusyboxContainer(t, ctrd, "erofs-dmv-auto-pod", "erofs-dmv-auto-ctr")

		_, _, err := ctrd.proc.criRuntimeService(t).ExecSync(cnID, []string{"/bin/true"}, 10*time.Second)
		require.NoError(t, err)

		assertEROFSArtifacts(t, ctrd.proc, true)
	})

	t.Run("on", func(t *testing.T) {
		ctrd := newEROFSCtrd(t, "  dmverity_mode = \"on\"\n", "  enable_dmverity = true\n")
		runEROFSBusyboxContainer(t, ctrd, "erofs-dmv-on-pod", "erofs-dmv-on-ctr")

		assertEROFSArtifacts(t, ctrd.proc, true)

		mountInfo, err := os.ReadFile("/proc/self/mountinfo")
		require.NoError(t, err)
		snapRoot := filepath.Dir(erofsSnapshotsDir(ctrd.proc))
		hasDm := false
		for _, line := range strings.Split(string(mountInfo), "\n") {
			if strings.Contains(line, snapRoot) && (strings.Contains(line, "/dev/dm-") || strings.Contains(line, "/dev/mapper/")) {
				hasDm = true
				break
			}
		}
		assert.True(t, hasDm, "no dm-* mount under %s", snapRoot)
	})
}

func TestEROFSTarIndex(t *testing.T) {
	ctrd := newEROFSCtrd(t, "", "  enable_tar_index = true\n")
	_, cnID := runEROFSBusyboxContainer(t, ctrd, "erofs-tarindex-pod", "erofs-tarindex-ctr")

	_, _, err := ctrd.proc.criRuntimeService(t).ExecSync(cnID, []string{"/bin/true"}, 10*time.Second)
	require.NoError(t, err)

	assertEROFSArtifacts(t, ctrd.proc, false)
}

func TestEROFSBlockQuota(t *testing.T) {
	ctrd := newEROFSCtrd(t, "  default_size = \"1GiB\"\n", "")
	_, cnID := runEROFSBusyboxContainer(t, ctrd, "erofs-quota-pod", "erofs-quota-ctr")

	_, _, err := ctrd.proc.criRuntimeService(t).ExecSync(cnID, []string{"/bin/true"}, 10*time.Second)
	require.NoError(t, err)

	sawRw := false
	for _, d := range listEROFSSnapshotDirs(t, ctrd.proc) {
		if _, err := os.Stat(filepath.Join(d, "rwlayer.img")); err == nil {
			sawRw = true
			break
		}
	}
	assert.True(t, sawRw, "no rwlayer.img under %s", erofsSnapshotsDir(ctrd.proc))
}

// prepareNativeImage converts busybox to an erofs-native image in the content
// store and returns the local reference. blobCompression="" yields raw EROFS
// layers; "zstd" forwards to erofs.WithBlobCompression.
func prepareNativeImage(t *testing.T, ctrd *erofsCtrd, blobCompression string) string {
	t.Helper()
	if _, err := exec.LookPath("mkfs.erofs"); err != nil {
		t.Skipf("mkfs.erofs not in PATH: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	srcRef := images.Get(images.BusyBox)
	_, err := ctrd.client.Fetch(ctx, srcRef)
	require.NoError(t, err, "fetch %s", srcRef)

	nativeRef := srcRef + "-erofs-native"
	if blobCompression != "" {
		nativeRef = srcRef + "-erofs-native-" + blobCompression
	}

	var erofsOpts []erofs.ConvertOpt
	if blobCompression != "" {
		erofsOpts = append(erofsOpts, erofs.WithBlobCompression(blobCompression))
	}
	// UpdateManifestPlatform writes ["erofs"] into the converted manifest's
	// OSFeatures, so the platform matcher must include "erofs" to survive the
	// index-level filter on any repeated conversion.
	platSpec := platforms.DefaultSpec()
	platSpec.OSFeatures = append(platSpec.OSFeatures, "erofs")
	opts := []converter.Opt{
		converter.WithLayerConvertFunc(erofs.LayerConvertFunc(erofsOpts...)),
		converter.WithUpdateManifest(erofs.UpdateManifestPlatform),
		converter.WithPlatform(platforms.OnlyStrict(platSpec)),
	}
	dstImgCore, err := converter.Convert(ctx, ctrd.client, nativeRef, srcRef, opts...)
	require.NoError(t, err, "converter.Convert")

	imgSvc := ctrd.client.ImageService()
	img := coreimages.Image{
		Name:   nativeRef,
		Target: dstImgCore.Target,
	}
	if _, err := imgSvc.Update(ctx, img); err != nil {
		if errdefs.IsNotFound(err) {
			_, err = imgSvc.Create(ctx, img)
			require.NoError(t, err, "Create native image ref")
		} else {
			require.NoError(t, err, "Update native image ref")
		}
	}
	return nativeRef
}

func TestEROFSNativeImageRun(t *testing.T) {
	cases := []struct {
		name, compression string
	}{
		{"raw", ""},
		{"zstd", "zstd"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ctrd := newEROFSCtrd(t, "", "")
			nativeRef := prepareNativeImage(t, ctrd, tc.compression)

			rSvc := ctrd.proc.criRuntimeService(t)
			pullImagesByCRI(t, ctrd.proc.criImageService(t), nativeRef)

			sbCfg := PodSandboxConfig("erofs-native-"+tc.name+"-pod", "erofs-integration")
			sbID, err := rSvc.RunPodSandbox(sbCfg, "")
			require.NoError(t, err)

			cnCfg := ContainerConfig("erofs-native-"+tc.name+"-ctr", nativeRef, WithCommand("sleep", "1d"))
			cnID, err := rSvc.CreateContainer(sbID, cnCfg, sbCfg)
			require.NoError(t, err)
			require.NoError(t, rSvc.StartContainer(cnID))

			stdout, stderr, err := rSvc.ExecSync(cnID, []string{"/bin/echo", "ok"}, 10*time.Second)
			require.NoError(t, err, "stderr=%q", string(stderr))
			assert.Contains(t, string(stdout), "ok")

			assertEROFSArtifacts(t, ctrd.proc, false)
		})
	}
}

func TestEROFSNativeImageContainerdRestart(t *testing.T) {
	ctrd := newEROFSCtrd(t, "", "")
	nativeRef := prepareNativeImage(t, ctrd, "")

	rSvc := ctrd.proc.criRuntimeService(t)
	pullImagesByCRI(t, ctrd.proc.criImageService(t), nativeRef)

	sbCfg := PodSandboxConfig("erofs-native-restart-pod", "erofs-integration")
	sbID, err := rSvc.RunPodSandbox(sbCfg, "")
	require.NoError(t, err)

	cnCfg := ContainerConfig("erofs-native-restart-ctr", nativeRef, WithCommand("sleep", "1d"))
	cnID, err := rSvc.CreateContainer(sbID, cnCfg, sbCfg)
	require.NoError(t, err)
	require.NoError(t, rSvc.StartContainer(cnID))

	require.NoError(t, ctrd.proc.kill(syscall.SIGTERM))
	require.NoError(t, ctrd.proc.wait(2*time.Minute))

	restarted := newCtrdProc(t, *containerdBin, ctrd.proc.workDir, nil)
	require.NoError(t, restarted.isReady())
	t.Cleanup(func() {
		_ = restarted.kill(syscall.SIGTERM)
		_ = restarted.wait(2 * time.Minute)
	})

	rSvc = restarted.criRuntimeService(t)
	st, err := rSvc.ContainerStatus(cnID)
	require.NoError(t, err)
	require.Equal(t, criruntime.ContainerState_CONTAINER_RUNNING, st.State)

	// Create a new container to ensure snapshotter is still happy and diff applies correctly
	cn2Cfg := ContainerConfig("erofs-native-restart-ctr-2", nativeRef, WithCommand("sleep", "1d"))
	cn2ID, err := rSvc.CreateContainer(sbID, cn2Cfg, PodSandboxConfig("erofs-native-restart-pod", "erofs-integration"))
	require.NoError(t, err)
	require.NoError(t, rSvc.StartContainer(cn2ID))
	
	_, _, err = rSvc.ExecSync(cn2ID, []string{"/bin/true"}, 10*time.Second)
	require.NoError(t, err)
}

func TestEROFSNativeImageSnapshotGC(t *testing.T) {
	ctrd := newEROFSCtrd(t, "", "")
	nativeRef := prepareNativeImage(t, ctrd, "")

	rSvc := ctrd.proc.criRuntimeService(t)
	iSvc := ctrd.proc.criImageService(t)
	pullImagesByCRI(t, iSvc, nativeRef)

	sbCfg := PodSandboxConfig("erofs-native-gc-pod", "erofs-integration")
	sbID, err := rSvc.RunPodSandbox(sbCfg, "")
	require.NoError(t, err)

	cnCfg := ContainerConfig("erofs-native-gc-ctr", nativeRef, WithCommand("sleep", "1d"))
	cnID, err := rSvc.CreateContainer(sbID, cnCfg, sbCfg)
	require.NoError(t, err)
	require.NoError(t, rSvc.StartContainer(cnID))

	require.NotEmpty(t, listEROFSSnapshotDirs(t, ctrd.proc))

	require.NoError(t, rSvc.StopContainer(cnID, 0))
	require.NoError(t, rSvc.RemoveContainer(cnID))
	require.NoError(t, rSvc.StopPodSandbox(sbID))
	require.NoError(t, rSvc.RemovePodSandbox(sbID))

	// Remove via CRI to trigger garbage collection
	require.NoError(t, iSvc.RemoveImage(&criruntime.ImageSpec{Image: nativeRef}))

	err = Eventually(func() (bool, error) {
		return len(listEROFSSnapshotDirs(t, ctrd.proc)) <= 1, nil
	}, time.Second, 60*time.Second)
	require.NoError(t, err, "erofs-native snapshots were not garbage collected")
}

// TestEROFSPullMixedImage verifies that on an erofs-enabled daemon the erofs
// manifest of a mixed OCI+erofs index is preferred over the OCI fallback.
// layer.erofs presence proves the selection.
func TestEROFSPullMixedImage(t *testing.T) {
	ctrd := newEROFSCtrd(t, "", "")

	ref := images.Get(images.ErofsOciAndRaw)
	pullImagesByCRI(t, ctrd.proc.criImageService(t), ref)

	rSvc := ctrd.proc.criRuntimeService(t)
	sbCfg := PodSandboxConfig("erofs-mixed-pod", "erofs-integration")
	sbID, err := rSvc.RunPodSandbox(sbCfg, "")
	require.NoError(t, err)

	cnCfg := ContainerConfig("erofs-mixed-ctr", ref, WithCommand("sleep", "1d"))
	cnID, err := rSvc.CreateContainer(sbID, cnCfg, sbCfg)
	require.NoError(t, err)
	require.NoError(t, rSvc.StartContainer(cnID))

	_, _, err = rSvc.ExecSync(cnID, []string{"/bin/true"}, 10*time.Second)
	require.NoError(t, err)

	assertEROFSArtifacts(t, ctrd.proc, false)
}
