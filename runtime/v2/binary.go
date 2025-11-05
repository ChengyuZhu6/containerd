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

package v2

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	gruntime "runtime"

	"github.com/containerd/containerd/api/runtime/task/v2"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/protobuf"
	"github.com/containerd/containerd/protobuf/proto"
	"github.com/containerd/containerd/protobuf/types"
	"github.com/containerd/containerd/runtime"
	client "github.com/containerd/containerd/runtime/v2/shim"
	"github.com/containerd/log"
)

type shimBinaryConfig struct {
	runtime      string
	address      string
	ttrpcAddress string
	schedCore    bool
}

func shimBinary(bundle *Bundle, config shimBinaryConfig) *binary {
	return &binary{
		bundle:                 bundle,
		runtime:                config.runtime,
		containerdAddress:      config.address,
		containerdTTRPCAddress: config.ttrpcAddress,
		schedCore:              config.schedCore,
	}
}

type binary struct {
	runtime                string
	containerdAddress      string
	containerdTTRPCAddress string
	schedCore              bool
	bundle                 *Bundle
}

func (b *binary) Start(ctx context.Context, opts *types.Any, onClose func()) (_ *shim, err error) {
	// Feature gate for prewarm: enable when CONTAINERD_SHIM_PREWARM=1
	prewarmEnabled := os.Getenv("CONTAINERD_SHIM_PREWARM") == "1"
	if prewarmEnabled {
		// Optional runtime allowlist via CONTAINERD_SHIM_PREWARM_RUNTIMES=runtime1,runtime2
		if allow := os.Getenv("CONTAINERD_SHIM_PREWARM_RUNTIMES"); allow != "" {
			allowed := false
			for _, r := range bytes.Split([]byte(allow), []byte(",")) {
				if string(bytes.TrimSpace(r)) == b.runtime {
					allowed = true
					break
				}
			}
			if !allowed {
				prewarmEnabled = false
			}
		}
	}

	buildArgs := func(action string) []string {
		args := []string{"-id", b.bundle.ID}
		switch log.GetLevel() {
		case log.DebugLevel, log.TraceLevel:
			args = append(args, "-debug")
		}
		args = append(args, action)
		return args
	}

	runShim := func(action string) (response []byte, runErr error) {
		args := buildArgs(action)

		// Prewarm should not bind to bundle-specific working dir.
		var workPath string
		if action != "prewarm" {
			workPath = b.bundle.Path
		}

		cmd, err := client.Command(
			ctx,
			&client.CommandConfig{
				Runtime:      b.runtime,
				Address:      b.containerdAddress,
				TTRPCAddress: b.containerdTTRPCAddress,
				Path:         workPath,
				Opts:         opts,
				Args:         args,
				SchedCore:    b.schedCore,
			})
		if err != nil {
			return nil, err
		}
		log.G(ctx).WithFields(log.Fields{
			"bundle_id": b.bundle.ID,
			"runtime":   b.runtime,
			"args":      args,
			"path":      workPath,
		}).Info("binary.Start: executing shim command")

		out, err := cmd.CombinedOutput()

		log.G(ctx).Info("binary.Start: shim command execution completed")

		if err != nil {
			return nil, fmt.Errorf("%s: %w", out, err)
		}
		return bytes.TrimSpace(out), nil
	}

	// Windows needs a namespace when openShimLog
	ns, _ := namespaces.Namespace(ctx)
	shimCtx, cancelShimLog := context.WithCancel(namespaces.WithNamespace(context.Background(), ns))
	defer func() {
		if err != nil {
			cancelShimLog()
		}
	}()
	f, err := openShimLog(shimCtx, b.bundle, client.AnonDialer)
	if err != nil {
		return nil, fmt.Errorf("open shim log pipe: %w", err)
	}
	defer func() {
		if err != nil {
			f.Close()
		}
	}()
	// copy the shim's logs to containerd's output
	go func() {
		defer f.Close()
		_, err := io.Copy(os.Stderr, f)
		err = checkCopyShimLogError(ctx, err)
		if err != nil {
			log.G(ctx).WithError(err).Error("copy shim log")
		}
	}()

	// Try prewarm first if enabled, otherwise fall back to start
	var response []byte
	if prewarmEnabled {
		resp, perr := runShim("prewarm")
		if perr == nil {
			response = resp
		} else {
			// Prewarm failed, log and fall back to start
			log.G(ctx).WithError(perr).Warn("shim prewarm failed, falling back to start")
		}
	}
	if response == nil {
		var serr error
		response, serr = runShim("start")
		if serr != nil {
			return nil, serr
		}
	}

	onCloseWithShimLog := func() {
		onClose()
		cancelShimLog()
		f.Close()
	}
	// Save runtime binary path for restore only when we are binding bundle (start path).
	// Prewarm should not write into bundle.
	if !prewarmEnabled || response == nil {
		if err := os.WriteFile(filepath.Join(b.bundle.Path, "shim-binary-path"), []byte(b.runtime), 0600); err != nil {
			return nil, err
		}
	}

	params, err := parseStartResponse(ctx, response)
	if err != nil {
		// If parse indicates unsupported (e.g., prewarm returned v3 but parser rejects),
		// and we had attempted prewarm, then fall back to start once.
		if prewarmEnabled {
			log.G(ctx).WithError(err).Warn("parse prewarm response failed, retrying shim start")
			resp, serr := runShim("start")
			if serr != nil {
				return nil, serr
			}
			params, err = parseStartResponse(ctx, resp)
			if err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}

	conn, err := makeConnection(ctx, params, onCloseWithShimLog)
	if err != nil {
		return nil, err
	}

	// If we prewarmed and the shim supports Adopt, bind container context before returning.
	// Use namespace from ctx; ID/Bundle来自当前bundle。
	if os.Getenv("CONTAINERD_SHIM_PREWARM") == "1" && params.Version >= 3 {
		ns, _ := namespaces.Namespace(ctx)
		adoptErr := client.AdoptContainer(ctx, conn, &client.AdoptRequest{
			Id:        b.bundle.ID,
			Bundle:    b.bundle.Path,
			Namespace: ns,
		})
		if adoptErr != nil {
			// 兼容处理：若 shim 未实现或返回错误，记录并继续旧路径
			log.G(ctx).WithError(adoptErr).Warn("AdoptContainer failed; continuing without adopt")
		}
	}

	return &shim{
		bundle: b.bundle,
		client: conn,
	}, nil
}

func (b *binary) Delete(ctx context.Context) (*runtime.Exit, error) {
	log.G(ctx).Info("cleaning up dead shim")

	// On Windows and FreeBSD, the current working directory of the shim should
	// not be the bundle path during the delete operation. Instead, we invoke
	// with the default work dir and forward the bundle path on the cmdline.
	// Windows cannot delete the current working directory while an executable
	// is in use with it. On FreeBSD, fork/exec can fail.
	var bundlePath string
	if gruntime.GOOS != "windows" && gruntime.GOOS != "freebsd" {
		bundlePath = b.bundle.Path
	}
	args := []string{
		"-id", b.bundle.ID,
		"-bundle", b.bundle.Path,
	}
	switch log.GetLevel() {
	case log.DebugLevel, log.TraceLevel:
		args = append(args, "-debug")
	}
	args = append(args, "delete")

	cmd, err := client.Command(ctx,
		&client.CommandConfig{
			Runtime:      b.runtime,
			Address:      b.containerdAddress,
			TTRPCAddress: b.containerdTTRPCAddress,
			Path:         bundlePath,
			Opts:         nil,
			Args:         args,
		})

	if err != nil {
		return nil, err
	}
	var (
		out  = bytes.NewBuffer(nil)
		errb = bytes.NewBuffer(nil)
	)
	cmd.Stdout = out
	cmd.Stderr = errb
	if err := cmd.Run(); err != nil {
		log.G(ctx).WithField("cmd", cmd).WithError(err).Error("failed to delete")
		return nil, fmt.Errorf("%s: %w", errb.String(), err)
	}
	s := errb.String()
	if s != "" {
		log.G(ctx).Warnf("cleanup warnings %s", s)
	}
	var response task.DeleteResponse
	if err := proto.Unmarshal(out.Bytes(), &response); err != nil {
		return nil, err
	}
	if err := b.bundle.Delete(); err != nil {
		return nil, err
	}
	return &runtime.Exit{
		Status:    response.ExitStatus,
		Timestamp: protobuf.FromTimestamp(response.ExitedAt),
		Pid:       response.Pid,
	}, nil
}
