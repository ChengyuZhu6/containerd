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

package unpack

import (
	"reflect"
	"testing"

	"github.com/containerd/containerd/mount"
)

func TestBindToOverlay(t *testing.T) {
	testCases := []struct {
		name   string
		mounts []mount.Mount
		expect []mount.Mount
	}{
		{
			name: "single bind mount",
			mounts: []mount.Mount{
				{
					Type:    "bind",
					Source:  "/path/to/source",
					Options: []string{"ro", "rbind"},
				},
			},
			expect: []mount.Mount{
				{
					Type:   "overlay",
					Source: "overlay",
					Options: []string{
						"ro",
						"upperdir=/path/to/source",
					},
				},
			},
		},
		{
			name: "overlay mount",
			mounts: []mount.Mount{
				{
					Type:   "overlay",
					Source: "overlay",
					Options: []string{
						"lowerdir=/path/to/lower",
						"upperdir=/path/to/upper",
					},
				},
			},
			expect: []mount.Mount{
				{
					Type:   "overlay",
					Source: "overlay",
					Options: []string{
						"lowerdir=/path/to/lower",
						"upperdir=/path/to/upper",
					},
				},
			},
		},
		{
			name: "multiple mounts",
			mounts: []mount.Mount{
				{
					Type:   "bind",
					Source: "/path/to/source1",
				},
				{
					Type:   "bind",
					Source: "/path/to/source2",
				},
			},
			expect: []mount.Mount{
				{
					Type:   "bind",
					Source: "/path/to/source1",
				},
				{
					Type:   "bind",
					Source: "/path/to/source2",
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := bindToOverlay(tc.mounts)
			if !reflect.DeepEqual(result, tc.expect) {
				t.Errorf("unexpected result: got %v, want %v", result, tc.expect)
			}
		})
	}
}
