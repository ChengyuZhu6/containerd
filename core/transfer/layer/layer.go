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

package layer

import (
	"context"
	"fmt"
	"io"

	"github.com/containerd/typeurl/v2"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	transfertypes "github.com/containerd/containerd/api/types/transfer"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/streaming"
	"github.com/containerd/containerd/v2/core/transfer"
	"github.com/containerd/containerd/v2/core/transfer/plugins"
	tstreaming "github.com/containerd/containerd/v2/core/transfer/streaming"
	"github.com/containerd/log"
)

func init() {
	plugins.Register(&transfertypes.LayerStream{}, &Stream{})
}

// StreamOpt defines options when configuring a layer stream
type StreamOpt func(*Stream)

// WithMediaType sets the media type for the layer
func WithMediaType(mediaType string) StreamOpt {
	return func(s *Stream) {
		s.mediaType = mediaType
	}
}

// WithDescriptor sets the descriptor for the layer
func WithDescriptor(desc ocispec.Descriptor) StreamOpt {
	return func(s *Stream) {
		s.desc = desc
	}
}

// NewStream creates a new layer stream source
func NewStream(r io.Reader, opts ...StreamOpt) *Stream {
	s := &Stream{
		stream: r,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// NewStreamFromDescriptor creates a new layer stream source from a descriptor
func NewStreamFromDescriptor(desc ocispec.Descriptor, cs content.Store) *Stream {
	return &Stream{
		desc:    desc,
		content: cs,
	}
}

// Stream represents a layer stream source or destination
type Stream struct {
	stream    io.Reader
	desc      ocispec.Descriptor
	mediaType string
	content   content.Store
}

func (s *Stream) String() string {
	if s.desc.Digest != "" {
		return fmt.Sprintf("Layer Stream (%s)", s.desc.Digest)
	}
	return "Layer Stream"
}

// GetLayer returns the layer descriptor and reader
func (s *Stream) GetLayer(ctx context.Context) (ocispec.Descriptor, io.ReadCloser, error) {
	if s.content != nil {
		ra, err := s.content.ReaderAt(ctx, s.desc)
		if err != nil {
			return ocispec.Descriptor{}, nil, err
		}
		return s.desc, &readerAtWrapper{ra: ra}, nil
	}

	if s.stream == nil {
		return ocispec.Descriptor{}, nil, fmt.Errorf("no stream available")
	}

	if rc, ok := s.stream.(io.ReadCloser); ok {
		return s.desc, rc, nil
	}

	return s.desc, io.NopCloser(s.stream), nil
}

// ImportStream returns the stream for importing
func (s *Stream) ImportStream(context.Context) (io.Reader, string, error) {
	return s.stream, s.mediaType, nil
}

// MarshalAny marshals the layer stream for transfer over RPC
func (s *Stream) MarshalAny(ctx context.Context, sm streaming.StreamCreator) (typeurl.Any, error) {
	sid := tstreaming.GenerateID("layer")
	stream, err := sm.Create(ctx, sid)
	if err != nil {
		return nil, err
	}

	if s.stream != nil {
		tstreaming.SendStream(ctx, s.stream, stream)
	}

	ls := &transfertypes.LayerStream{
		StreamId: sid,
		Desc: &transfertypes.Descriptor{
			MediaType: s.desc.MediaType,
			Digest:    s.desc.Digest.String(),
			Size_:     s.desc.Size,
		},
	}

	return typeurl.MarshalAny(ls)
}

// UnmarshalAny unmarshals the layer stream from RPC
func (s *Stream) UnmarshalAny(ctx context.Context, sm streaming.StreamGetter, a typeurl.Any) error {
	var ls transfertypes.LayerStream
	if err := typeurl.UnmarshalTo(a, &ls); err != nil {
		return err
	}

	stream, err := sm.Get(ctx, ls.StreamId)
	if err != nil {
		log.G(ctx).WithError(err).WithField("stream", ls.StreamId).Debug("failed to get layer stream")
		return err
	}

	s.stream = tstreaming.ReceiveStream(ctx, stream)
	if ls.Desc != nil {
		s.desc = ocispec.Descriptor{
			MediaType: ls.Desc.MediaType,
			Digest:    ls.Desc.Digest,
			Size:      ls.Desc.Size_,
		}
	}

	return nil
}

// readerAtWrapper wraps a ReaderAt to provide a ReadCloser interface
type readerAtWrapper struct {
	ra     content.ReaderAt
	offset int64
}

func (r *readerAtWrapper) Read(p []byte) (n int, err error) {
	n, err = r.ra.ReadAt(p, r.offset)
	r.offset += int64(n)
	return
}

func (r *readerAtWrapper) Close() error {
	return r.ra.Close()
}
