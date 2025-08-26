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
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/core/transfer"
	"github.com/containerd/containerd/v2/pkg/progress"
)

type dockerProgressDisplay struct {
	out    io.Writer
	mutex  sync.RWMutex
	layers map[string]*layerProgress
	done   map[string]bool
	start  time.Time
	isTTY  bool
}

type layerProgress struct {
	id       string
	digest   string
	status   string
	current  int64
	total    int64
	lastTime time.Time
}

// NewDockerDisplay creates a new docker-style progress display
func NewDockerDisplay(out io.Writer) *dockerProgressDisplay {
	isTTY := false
	if f, ok := out.(*os.File); ok {
		// Simple TTY detection - check if it's stdout/stderr
		isTTY = f.Fd() == 1 || f.Fd() == 2
	}

	return &dockerProgressDisplay{
		out:    out,
		layers: make(map[string]*layerProgress),
		done:   make(map[string]bool),
		start:  time.Now(),
		isTTY:  isTTY,
	}
}

func (d *dockerProgressDisplay) updateProgress(p transfer.Progress) {
	d.mutex.Lock()
	defer d.mutex.Unlock()

	// Skip if no descriptor
	if p.Desc == nil {
		return
	}

	// Create layer ID from digest
	layerID := shortenDigest(p.Desc.Digest.String())

	// Update or create layer progress
	if layer, exists := d.layers[layerID]; exists {
		layer.status = p.Event
		layer.current = p.Progress
		layer.total = p.Total
		layer.lastTime = time.Now()
	} else {
		d.layers[layerID] = &layerProgress{
			id:       layerID,
			digest:   p.Desc.Digest.String(),
			status:   p.Event,
			current:  p.Progress,
			total:    p.Total,
			lastTime: time.Now(),
		}
	}

	// Mark as done if complete
	if p.Event == "complete" || p.Event == "done" {
		d.done[layerID] = true
	}
}

func (d *dockerProgressDisplay) render() {
	d.mutex.RLock()
	defer d.mutex.RUnlock()

	if d.isTTY {
		// Clear previous output and move cursor to top
		fmt.Fprint(d.out, "\033[2J\033[H")
	}

	// Sort layers by ID for consistent output
	var layerIDs []string
	for id := range d.layers {
		layerIDs = append(layerIDs, id)
	}
	sort.Strings(layerIDs)

	// Display each layer
	for _, id := range layerIDs {
		layer := d.layers[id]
		d.renderLayer(layer)
	}

	if !d.isTTY {
		fmt.Fprint(d.out, "\n")
	}
}

func (d *dockerProgressDisplay) renderLayer(layer *layerProgress) {
	switch layer.status {
	case "waiting":
		fmt.Fprintf(d.out, "%s: Waiting\n", layer.id)
	case "resolving":
		fmt.Fprintf(d.out, "%s: Pulling fs layer\n", layer.id)
	case "downloading":
		if layer.total > 0 {
			percent := float64(layer.current) / float64(layer.total) * 100
			bar := d.createProgressBar(layer.current, layer.total, 50)
			fmt.Fprintf(d.out, "%s: Downloading %s %.1f%% %s/%s",
				layer.id, bar, percent,
				progress.Bytes(layer.current), progress.Bytes(layer.total))

			speed := d.calculateSpeed(layer)
			if speed != "" {
				fmt.Fprintf(d.out, " %s", speed)
			}

			eta := d.calculateETA(layer)
			if eta > 0 {
				fmt.Fprintf(d.out, " %s", eta.Truncate(time.Second))
			}
			fmt.Fprintf(d.out, "\n")
		} else {
			fmt.Fprintf(d.out, "%s: Downloading\n", layer.id)
		}
	case "extracting":
		if layer.total > 0 {
			percent := float64(layer.current) / float64(layer.total) * 100
			bar := d.createProgressBar(layer.current, layer.total, 50)
			fmt.Fprintf(d.out, "%s: Extracting %s %.1f%% %s/%s\n",
				layer.id, bar, percent,
				progress.Bytes(layer.current), progress.Bytes(layer.total))
		} else {
			fmt.Fprintf(d.out, "%s: Extracting\n", layer.id)
		}
	case "complete", "done":
		if d.done[layer.id] {
			fmt.Fprintf(d.out, "%s: Pull complete\n", layer.id)
		}
	default:
		fmt.Fprintf(d.out, "%s: %s\n", layer.id, layer.status)
	}
}

func (d *dockerProgressDisplay) createProgressBar(current, total int64, width int) string {
	if total <= 0 {
		return strings.Repeat(" ", width)
	}

	filled := int(float64(current) / float64(total) * float64(width))
	if filled > width {
		filled = width
	}

	bar := "["
	bar += strings.Repeat("=", filled)
	if filled < width {
		bar += ">"
		bar += strings.Repeat(" ", width-filled-1)
	}
	bar += "]"
	return bar
}

func (d *dockerProgressDisplay) calculateSpeed(layer *layerProgress) string {
	elapsed := time.Since(d.start)
	if elapsed.Seconds() == 0 || layer.current == 0 {
		return ""
	}

	bytesPerSec := float64(layer.current) / elapsed.Seconds()
	return fmt.Sprintf("(%s/s)", progress.Bytes(int64(bytesPerSec)))
}

func (d *dockerProgressDisplay) calculateETA(layer *layerProgress) time.Duration {
	if layer.total <= 0 || layer.current <= 0 {
		return 0
	}

	elapsed := time.Since(d.start)
	if elapsed.Seconds() == 0 {
		return 0
	}

	bytesPerSec := float64(layer.current) / elapsed.Seconds()
	remaining := layer.total - layer.current
	if bytesPerSec > 0 {
		return time.Duration(float64(remaining)/bytesPerSec) * time.Second
	}
	return 0
}

func shortenDigest(digest string) string {
	if strings.HasPrefix(digest, "sha256:") && len(digest) >= 19 {
		return digest[7:19]
	}
	if len(digest) > 12 {
		return digest[:12]
	}
	return digest
}

// DockerProgressHandler creates a progress handler that mimics docker pull output
func DockerProgressHandler(ctx context.Context, out io.Writer) (transfer.ProgressFunc, func()) {
	ctx, cancel := context.WithCancel(ctx)
	display := NewDockerDisplay(out)

	pc := make(chan transfer.Progress, 100)
	closeC := make(chan struct{})

	progressFunc := func(p transfer.Progress) {
		select {
		case pc <- p:
		case <-ctx.Done():
		}
	}

	done := func() {
		cancel()
		<-closeC
	}

	go func() {
		defer close(closeC)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case p := <-pc:
				display.updateProgress(p)
			case <-ticker.C:
				display.render()
			case <-ctx.Done():
				// Final render
				display.render()
				return
			}
		}
	}()

	return progressFunc, done
}
