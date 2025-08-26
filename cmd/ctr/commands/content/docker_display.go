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

package content

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/pkg/progress"
)

type dockerContentDisplay struct {
	out    io.Writer
	mutex  sync.RWMutex
	layers map[string]*contentLayerProgress
	start  time.Time
	isTTY  bool
}

type contentLayerProgress struct {
	id       string
	digest   string
	status   StatusInfoStatus
	current  int64
	total    int64
	lastTime time.Time
}

// NewDockerContentDisplay creates a new docker-style progress display for content
func NewDockerContentDisplay(out io.Writer) *dockerContentDisplay {
	isTTY := false
	if f, ok := out.(*os.File); ok {
		// Simple TTY detection - check if it's stdout/stderr
		isTTY = f.Fd() == 1 || f.Fd() == 2
	}

	return &dockerContentDisplay{
		out:    out,
		layers: make(map[string]*contentLayerProgress),
		start:  time.Now(),
		isTTY:  isTTY,
	}
}

func (d *dockerContentDisplay) updateProgress(status StatusInfo) {
	d.mutex.Lock()
	defer d.mutex.Unlock()

	// Create layer ID from ref
	layerID := shortenRef(status.Ref)

	// Update or create layer progress
	if layer, exists := d.layers[layerID]; exists {
		layer.status = status.Status
		layer.current = status.Offset
		layer.total = status.Total
		layer.lastTime = time.Now()
	} else {
		d.layers[layerID] = &contentLayerProgress{
			id:       layerID,
			digest:   status.Ref,
			status:   status.Status,
			current:  status.Offset,
			total:    status.Total,
			lastTime: time.Now(),
		}
	}
}

func (d *dockerContentDisplay) render() {
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

func (d *dockerContentDisplay) renderLayer(layer *contentLayerProgress) {
	switch layer.status {
	case StatusWaiting:
		fmt.Fprintf(d.out, "%s: Waiting\n", layer.id)
	case StatusResolving:
		fmt.Fprintf(d.out, "%s: Pulling fs layer\n", layer.id)
	case StatusDownloading:
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
			fmt.Fprintf(d.out, "\n")
		} else {
			fmt.Fprintf(d.out, "%s: Downloading\n", layer.id)
		}
	case StatusDone:
		fmt.Fprintf(d.out, "%s: Download complete\n", layer.id)
	case StatusExists:
		fmt.Fprintf(d.out, "%s: Already exists\n", layer.id)
	default:
		fmt.Fprintf(d.out, "%s: %s\n", layer.id, layer.status)
	}
}

func (d *dockerContentDisplay) createProgressBar(current, total int64, width int) string {
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

func (d *dockerContentDisplay) calculateSpeed(layer *contentLayerProgress) string {
	elapsed := time.Since(d.start)
	if elapsed.Seconds() == 0 || layer.current == 0 {
		return ""
	}

	bytesPerSec := float64(layer.current) / elapsed.Seconds()
	return fmt.Sprintf("(%s/s)", progress.Bytes(int64(bytesPerSec)))
}

func shortenRef(ref string) string {
	// Extract digest from ref if present
	parts := strings.Split(ref, ":")
	if len(parts) >= 2 {
		digest := parts[len(parts)-1]
		if len(digest) > 12 {
			return digest[:12]
		}
		return digest
	}
	if len(ref) > 12 {
		return ref[:12]
	}
	return ref
}

// DockerShowProgress displays progress in docker-style format
func DockerShowProgress(ctx context.Context, ongoing *Jobs, cs content.Store, out io.Writer) {
	display := NewDockerContentDisplay(out)

	var (
		ticker   = time.NewTicker(100 * time.Millisecond)
		start    = time.Now()
		statuses = map[string]StatusInfo{}
		done     bool
	)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			resolved := StatusResolved
			if !ongoing.IsResolved() {
				resolved = StatusResolving
			}
			statuses[ongoing.name] = StatusInfo{
				Ref:    ongoing.name,
				Status: resolved,
			}
			display.updateProgress(statuses[ongoing.name])

			activeSeen := map[string]struct{}{}
			if !done {
				active, err := cs.ListStatuses(ctx, "")
				if err != nil {
					continue
				}
				// update status of active entries!
				for _, active := range active {
					status := StatusInfo{
						Ref:       active.Ref,
						Status:    StatusDownloading,
						Offset:    active.Offset,
						Total:     active.Total,
						StartedAt: active.StartedAt,
						UpdatedAt: active.UpdatedAt,
					}
					statuses[active.Ref] = status
					display.updateProgress(status)
					activeSeen[active.Ref] = struct{}{}
				}
			}

			// now, update the items in jobs that are not in active
			for _, j := range ongoing.Jobs() {
				key := fmt.Sprintf("sha256:%s", j.Digest.Encoded())
				if _, ok := activeSeen[key]; ok {
					continue
				}

				status, ok := statuses[key]
				if !done && (!ok || status.Status == StatusDownloading) {
					info, err := cs.Info(ctx, j.Digest)
					if err != nil {
						status = StatusInfo{
							Ref:    key,
							Status: StatusWaiting,
						}
					} else if info.CreatedAt.After(start) {
						status = StatusInfo{
							Ref:       key,
							Status:    StatusDone,
							Offset:    info.Size,
							Total:     info.Size,
							UpdatedAt: info.CreatedAt,
						}
					} else {
						status = StatusInfo{
							Ref:    key,
							Status: StatusExists,
						}
					}
					statuses[key] = status
					display.updateProgress(status)
				} else if done {
					if ok {
						if status.Status != StatusDone && status.Status != StatusExists {
							status.Status = StatusDone
							statuses[key] = status
							display.updateProgress(status)
						}
					} else {
						status = StatusInfo{
							Ref:    key,
							Status: StatusDone,
						}
						statuses[key] = status
						display.updateProgress(status)
					}
				}
			}

			display.render()

			if done {
				return
			}
		case <-ctx.Done():
			done = true // allow ui to update once more
		}
	}
}
