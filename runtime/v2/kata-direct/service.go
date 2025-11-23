// Copyright The containerd Authors.
// SPDX-License-Identifier: Apache-2.0

package katadirect

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	eventstypes "github.com/containerd/containerd/api/events"
	taskAPI "github.com/containerd/containerd/api/runtime/task/v2"
	"github.com/containerd/containerd/api/types/task"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/namespaces"
	cdruntime "github.com/containerd/containerd/runtime"
	"github.com/containerd/containerd/runtime/v2/shim"
	"github.com/sirupsen/logrus"
	emptypb "google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/kata-containers/kata-containers/src/runtime/pkg/katautils"
	"github.com/kata-containers/kata-containers/src/runtime/pkg/oci"
	vc "github.com/kata-containers/kata-containers/src/runtime/virtcontainers"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

var (
	empty                     = &emptypb.Empty{}
	_     taskAPI.TaskService = (*service)(nil)
)

var serviceLog = logrus.WithFields(logrus.Fields{
	"source": "kata-direct",
	"name":   "kata-direct-runtime",
})

type service struct {
	mu         sync.Mutex
	vci        vc.VC
	sandbox    vc.VCSandbox
	config     *oci.RuntimeConfig
	containers map[string]*container
	events     chan interface{}
	id         string
	namespace  string
	hpid       uint32
	ctx        context.Context
	cancel     context.CancelFunc
	publisher  shim.Publisher
	exitCh     chan struct{}
}

type container struct {
	id       string
	bundle   string
	spec     *specs.Spec
	status   task.Status
	exit     uint32
	exitTime time.Time
	mounted  bool
	cType    vc.ContainerType
}

func New(ctx context.Context, id string, publisher shim.Publisher, shutdown func()) (shim.Shim, error) {
	serviceLog = serviceLog.WithFields(logrus.Fields{
		"sandbox": id,
		"pid":     os.Getpid(),
	})

	ns, found := namespaces.Namespace(ctx)
	if !found {
		return nil, fmt.Errorf("namespace not found in context")
	}

	vci := &vc.VCImpl{}
	vci.SetLogger(ctx, serviceLog)
	katautils.SetLogger(ctx, serviceLog, serviceLog.Logger.Level)

	ctx, cancel := context.WithCancel(ctx)

	s := &service{
		vci:        vci,
		ctx:        ctx,
		cancel:     cancel,
		namespace:  ns,
		id:         id,
		containers: make(map[string]*container),
		events:     make(chan interface{}, 128),
		publisher:  publisher,
		exitCh:     make(chan struct{}),
	}

	go s.forwardEvents()

	serviceLog.Info("kata-direct service created (no shim process)")

	return s, nil
}

func (s *service) forwardEvents() {
	defer func() {
		if r := recover(); r != nil {
			serviceLog.WithField("panic", r).Error("event forwarder panic recovered")
		}
	}()

	for {
		select {
		case evt := <-s.events:
			if evt == nil {
				return
			}
			topic := getTopic(evt)
			if err := s.publisher.Publish(s.ctx, topic, evt); err != nil {
				serviceLog.WithError(err).WithField("topic", topic).Warn("failed to publish event")
			}
		case <-s.exitCh:
			return
		case <-s.ctx.Done():
			return
		}
	}
}

func getTopic(e interface{}) string {
	switch e.(type) {
	case *eventstypes.TaskCreate:
		return cdruntime.TaskCreateEventTopic
	case *eventstypes.TaskStart:
		return cdruntime.TaskStartEventTopic
	case *eventstypes.TaskOOM:
		return cdruntime.TaskOOMEventTopic
	case *eventstypes.TaskExit:
		return cdruntime.TaskExitEventTopic
	case *eventstypes.TaskDelete:
		return cdruntime.TaskDeleteEventTopic
	case *eventstypes.TaskExecAdded:
		return cdruntime.TaskExecAddedEventTopic
	case *eventstypes.TaskExecStarted:
		return cdruntime.TaskExecStartedEventTopic
	case *eventstypes.TaskPaused:
		return cdruntime.TaskPausedEventTopic
	case *eventstypes.TaskResumed:
		return cdruntime.TaskResumedEventTopic
	default:
		serviceLog.WithField("event-type", e).Warn("no topic for event type")
	}
	return cdruntime.TaskUnknownTopic
}

func (s *service) StartShim(ctx context.Context, opts shim.StartOpts) (string, error) {
	serviceLog.Info("StartShim called - kata-direct mode (no actual shim process)")
	return fmt.Sprintf("kata-direct://%s", s.id), nil
}

func (s *service) Cleanup(ctx context.Context) (*taskAPI.DeleteResponse, error) {
	serviceLog.Info("Cleanup called")

	s.mu.Lock()
	defer s.mu.Unlock()

	close(s.exitCh)

	if s.sandbox != nil {
		if err := s.sandbox.Stop(ctx, false); err != nil {
			serviceLog.WithError(err).Warn("failed to stop sandbox during cleanup")
		}
		if err := s.sandbox.Delete(ctx); err != nil {
			serviceLog.WithError(err).Warn("failed to delete sandbox during cleanup")
		}
	}

	s.cancel()

	return &taskAPI.DeleteResponse{
		ExitedAt:   timestamppb.New(time.Now()),
		ExitStatus: 128 + 9, // SIGKILL
	}, nil
}

func (s *service) Create(ctx context.Context, r *taskAPI.CreateTaskRequest) (*taskAPI.CreateTaskResponse, error) {
	serviceLog.WithField("container", r.ID).Info("Create() start")
	defer serviceLog.WithField("container", r.ID).Info("Create() end")

	s.mu.Lock()
	defer s.mu.Unlock()

	type Result struct {
		container *container
		err       error
	}
	resultCh := make(chan Result, 1)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				serviceLog.WithField("panic", r).Error("Create panic recovered")
				resultCh <- Result{nil, fmt.Errorf("create panic: %v", r)}
			}
		}()

		c, err := s.createContainer(ctx, r)
		resultCh <- Result{c, err}
	}()

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("create container timeout: %v", r.ID)
	case res := <-resultCh:
		if res.err != nil {
			return nil, res.err
		}

		container := res.container
		container.status = task.Status_CREATED
		s.containers[r.ID] = container

		s.events <- &eventstypes.TaskCreate{
			ContainerID: r.ID,
			Bundle:      r.Bundle,
			Rootfs:      r.Rootfs,
			IO: &eventstypes.TaskIO{
				Stdin:    r.Stdin,
				Stdout:   r.Stdout,
				Stderr:   r.Stderr,
				Terminal: r.Terminal,
			},
			Checkpoint: r.Checkpoint,
			Pid:        s.hpid,
		}

		return &taskAPI.CreateTaskResponse{
			Pid: s.hpid,
		}, nil
	}
}

func (s *service) Start(ctx context.Context, r *taskAPI.StartRequest) (*taskAPI.StartResponse, error) {
	serviceLog.WithField("container", r.ID).Info("Start() start")
	defer serviceLog.WithField("container", r.ID).Info("Start() end")

	s.mu.Lock()
	defer s.mu.Unlock()

	c, ok := s.containers[r.ID]
	if !ok {
		return nil, errdefs.ToGRPCf(errdefs.ErrNotFound, "container %s not found", r.ID)
	}

	errCh := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				serviceLog.WithField("panic", r).Error("Start panic recovered")
				errCh <- fmt.Errorf("start panic: %v", r)
			}
		}()

		errCh <- s.startContainer(ctx, c)
	}()

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("start container timeout: %v", r.ID)
	case err := <-errCh:
		if err != nil {
			return nil, errdefs.ToGRPC(err)
		}

		s.events <- &eventstypes.TaskStart{
			ContainerID: c.id,
			Pid:         s.hpid,
		}

		return &taskAPI.StartResponse{
			Pid: s.hpid,
		}, nil
	}
}

func (s *service) Delete(ctx context.Context, r *taskAPI.DeleteRequest) (*taskAPI.DeleteResponse, error) {
	serviceLog.WithField("container", r.ID).Info("Delete() start")
	defer serviceLog.WithField("container", r.ID).Info("Delete() end")

	s.mu.Lock()
	defer s.mu.Unlock()

	c, ok := s.containers[r.ID]
	if !ok {
		return nil, errdefs.ToGRPCf(errdefs.ErrNotFound, "container %s not found", r.ID)
	}

	errCh := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				serviceLog.WithField("panic", r).Error("Delete panic recovered")
				errCh <- fmt.Errorf("delete panic: %v", r)
			}
		}()

		errCh <- s.deleteContainer(ctx, c)
	}()

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("delete container timeout: %v", r.ID)
	case err := <-errCh:
		if err != nil {
			return nil, err
		}

		delete(s.containers, r.ID)

		s.events <- &eventstypes.TaskDelete{
			ContainerID: c.id,
			Pid:         s.hpid,
			ExitStatus:  c.exit,
			ExitedAt:    timestamppb.New(c.exitTime),
		}

		return &taskAPI.DeleteResponse{
			ExitStatus: c.exit,
			ExitedAt:   timestamppb.New(c.exitTime),
			Pid:        s.hpid,
		}, nil
	}
}

func (s *service) State(ctx context.Context, r *taskAPI.StateRequest) (*taskAPI.StateResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, ok := s.containers[r.ID]
	if !ok {
		return nil, errdefs.ToGRPCf(errdefs.ErrNotFound, "container %s not found", r.ID)
	}

	return &taskAPI.StateResponse{
		ID:         c.id,
		Bundle:     c.bundle,
		Pid:        s.hpid,
		Status:     c.status,
		Stdin:      "",
		Stdout:     "",
		Stderr:     "",
		Terminal:   false,
		ExitStatus: c.exit,
		ExitedAt:   timestamppb.New(c.exitTime),
	}, nil
}

func (s *service) Kill(ctx context.Context, r *taskAPI.KillRequest) (*emptypb.Empty, error) {
	serviceLog.WithField("container", r.ID).WithField("signal", r.Signal).Info("Kill()")

	s.mu.Lock()
	defer s.mu.Unlock()

	c, ok := s.containers[r.ID]
	if !ok {
		return nil, errdefs.ToGRPCf(errdefs.ErrNotFound, "container %s not found", r.ID)
	}

	if s.sandbox == nil {
		return nil, fmt.Errorf("sandbox not found")
	}

	errCh := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				serviceLog.WithField("panic", r).Error("Kill panic recovered")
				errCh <- fmt.Errorf("kill panic: %v", r)
			}
		}()

		if c.cType.IsSandbox() {
			errCh <- s.sandbox.Kill(ctx, r.Signal, r.All)
		} else {
			errCh <- s.sandbox.KillContainer(ctx, c.id, r.Signal, r.All)
		}
	}()

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("kill container timeout: %v", r.ID)
	case err := <-errCh:
		if err != nil {
			return nil, errdefs.ToGRPC(err)
		}
		return empty, nil
	}
}

func (s *service) Pids(ctx context.Context, r *taskAPI.PidsRequest) (*taskAPI.PidsResponse, error) {
	return &taskAPI.PidsResponse{
		Processes: []*task.ProcessInfo{
			{
				Pid: s.hpid,
			},
		},
	}, nil
}

func (s *service) Pause(ctx context.Context, r *taskAPI.PauseRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.sandbox == nil {
		return nil, fmt.Errorf("sandbox not found")
	}

	if err := s.sandbox.Pause(ctx); err != nil {
		return nil, errdefs.ToGRPC(err)
	}

	s.events <- &eventstypes.TaskPaused{
		ContainerID: r.ID,
	}

	return empty, nil
}

func (s *service) Resume(ctx context.Context, r *taskAPI.ResumeRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.sandbox == nil {
		return nil, fmt.Errorf("sandbox not found")
	}

	if err := s.sandbox.Resume(ctx); err != nil {
		return nil, errdefs.ToGRPC(err)
	}

	s.events <- &eventstypes.TaskResumed{
		ContainerID: r.ID,
	}

	return empty, nil
}

func (s *service) Exec(ctx context.Context, r *taskAPI.ExecProcessRequest) (*emptypb.Empty, error) {
	return nil, errdefs.ToGRPC(errdefs.ErrNotImplemented)
}

func (s *service) ResizePty(ctx context.Context, r *taskAPI.ResizePtyRequest) (*emptypb.Empty, error) {
	return nil, errdefs.ToGRPC(errdefs.ErrNotImplemented)
}

func (s *service) CloseIO(ctx context.Context, r *taskAPI.CloseIORequest) (*emptypb.Empty, error) {
	return empty, nil
}

func (s *service) Checkpoint(ctx context.Context, r *taskAPI.CheckpointTaskRequest) (*emptypb.Empty, error) {
	return nil, errdefs.ToGRPC(errdefs.ErrNotImplemented)
}

func (s *service) Update(ctx context.Context, r *taskAPI.UpdateTaskRequest) (*emptypb.Empty, error) {
	return nil, errdefs.ToGRPC(errdefs.ErrNotImplemented)
}

func (s *service) Wait(ctx context.Context, r *taskAPI.WaitRequest) (*taskAPI.WaitResponse, error) {
	return nil, errdefs.ToGRPC(errdefs.ErrNotImplemented)
}

func (s *service) Stats(ctx context.Context, r *taskAPI.StatsRequest) (*taskAPI.StatsResponse, error) {
	return nil, errdefs.ToGRPC(errdefs.ErrNotImplemented)
}

func (s *service) Connect(ctx context.Context, r *taskAPI.ConnectRequest) (*taskAPI.ConnectResponse, error) {
	return &taskAPI.ConnectResponse{
		ShimPid: uint32(os.Getpid()),
		TaskPid: s.hpid,
	}, nil
}

func (s *service) Shutdown(ctx context.Context, r *taskAPI.ShutdownRequest) (*emptypb.Empty, error) {
	s.cancel()
	return empty, nil
}
