// Copyright The containerd Authors.
// SPDX-License-Identifier: Apache-2.0

package katadirect

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
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

// Default timeouts for various operations
const (
	defaultOperationTimeout = 30 * time.Second
	defaultCleanupTimeout   = 10 * time.Second
)

var (
	empty                     = &emptypb.Empty{}
	_     taskAPI.TaskService = (*service)(nil)
)

// Global logger initialization - only done once to avoid race conditions
var (
	globalLoggerOnce sync.Once
	globalLogger     *logrus.Entry
)

func getGlobalLogger() *logrus.Entry {
	globalLoggerOnce.Do(func() {
		globalLogger = logrus.WithFields(logrus.Fields{
			"source": "kata-direct",
			"name":   "kata-direct-runtime",
		})
	})
	return globalLogger
}

type serviceOptions struct {
	configPath string
}

type service struct {
	mu         sync.RWMutex // Changed to RWMutex for better read concurrency
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
	cleaned    bool
	cleanupWg  sync.WaitGroup // Tracks ongoing cleanup operations for orphan resources
	configPath string
	log        *logrus.Entry // per-service logger
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
	stdin    string
	stdout   string
	stderr   string
	terminal bool

	// IO management
	ioMu        sync.Mutex // Protects ioAttached and prevents concurrent handleIO
	ioWg        sync.WaitGroup
	ioAttached  bool               // Whether IO has been attached
	stdinCloser io.Closer          // Reference to stdin stream for CloseIO
	stdinFifo   io.Closer          // Reference to stdin FIFO file for closing when stdout/stderr done
	ioCancel    context.CancelFunc // Cancel function to stop IO goroutines

	// Exit channel for Wait() - like kata-shim-v2
	// exitCh is closed (not sent to) when process exits - this allows multiple waiters
	exitCh   chan struct{} // Channel closed when process exits (broadcast signal)
	exitIOch chan struct{} // Channel to signal IO streams closed
}

// Global virtcontainers initialization - only done once
var vcLoggerOnce sync.Once

// getSandbox safely returns the current sandbox reference (read-only access)
func (s *service) getSandbox() vc.VCSandbox {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sandbox
}

// setSandbox safely sets the sandbox reference
func (s *service) setSandbox(sandbox vc.VCSandbox) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sandbox = sandbox
}

// clearSandbox safely clears the sandbox reference
func (s *service) clearSandbox() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sandbox = nil
}

// withOperationTimeout wraps a context with the default operation timeout if no deadline is set
func withOperationTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, defaultOperationTimeout)
}

// withCleanupTimeout creates a context with cleanup timeout
func withCleanupTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), defaultCleanupTimeout)
}

func New(ctx context.Context, id string, publisher shim.Publisher, shutdown func(), opts *serviceOptions) (_ shim.Shim, retErr error) {
	fmt.Fprintf(os.Stderr, "[kata-direct] New service called for id=%s\n", id)
	defer func() {
		if r := recover(); r != nil {
			retErr = fmt.Errorf("New service panic: %v", r)
			fmt.Fprintf(os.Stderr, "[kata-direct] New service panic: %v\n", r)
		}
	}()

	// Create per-service logger with sandbox-specific fields
	log := getGlobalLogger().WithFields(logrus.Fields{
		"sandbox": id,
		"pid":     os.Getpid(),
	})

	ns, found := namespaces.Namespace(ctx)
	if !found {
		return nil, fmt.Errorf("namespace not found in context")
	}

	if opts == nil {
		opts = &serviceOptions{}
	}

	vci := &vc.VCImpl{}
	// Initialize global virtcontainers logger only once with a generic logger
	// IMPORTANT: Use getGlobalLogger() (without sandbox ID) for global vc/katautils loggers
	// Using a sandbox-specific logger here would cause state leakage between sandboxes
	// because katautils.SetLogger sets a package-level global logger that persists
	// across sandbox lifecycles.
	vcLoggerOnce.Do(func() {
		globalLog := getGlobalLogger()
		vci.SetLogger(ctx, globalLog)
		katautils.SetLogger(ctx, globalLog, globalLog.Logger.Level)
	})

	// Use independent context for service lifecycle, not tied to request context
	svcCtx, cancel := context.WithCancel(context.Background())

	s := &service{
		vci:        vci,
		ctx:        svcCtx,
		cancel:     cancel,
		namespace:  ns,
		id:         id,
		containers: make(map[string]*container),
		events:     make(chan interface{}, 128),
		publisher:  publisher,
		exitCh:     make(chan struct{}),
		configPath: opts.configPath,
		log:        log,
	}

	go s.forwardEvents()

	s.log.Info("kata-direct service created (no shim process)")

	return s, nil
}

func (s *service) forwardEvents() {
	defer func() {
		if r := recover(); r != nil {
			s.log.WithField("panic", r).Error("event forwarder panic recovered")
		}
	}()

	for {
		select {
		case evt := <-s.events:
			if evt == nil {
				return
			}
			topic := getTopic(s, evt)
			if err := s.publisher.Publish(s.ctx, topic, evt); err != nil {
				s.log.WithError(err).WithField("topic", topic).Warn("failed to publish event")
			}
		case <-s.exitCh:
			return
		case <-s.ctx.Done():
			return
		}
	}
}

func getTopic(s *service, e interface{}) string {
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
		s.log.WithField("event-type", e).Warn("no topic for event type")
	}
	return cdruntime.TaskUnknownTopic
}

func (s *service) StartShim(ctx context.Context, opts shim.StartOpts) (string, error) {
	s.log.Info("StartShim called - kata-direct mode (no actual shim process)")
	return fmt.Sprintf("kata-direct://%s", s.id), nil
}

func (s *service) Cleanup(ctx context.Context) (*taskAPI.DeleteResponse, error) {
	s.log.Info("Cleanup called")

	s.mu.Lock()

	if s.cleaned {
		s.mu.Unlock()
		s.log.Debug("Cleanup invoked after already completed, waiting for pending cleanups")
		// Wait for any ongoing orphan cleanup operations to complete
		// This prevents race conditions where callers retry before cleanup finishes
		s.cleanupWg.Wait()
		return &taskAPI.DeleteResponse{
			ExitedAt:   timestamppb.New(time.Now()),
			ExitStatus: 0,
		}, nil
	}
	s.cleaned = true

	close(s.exitCh)

	if s.sandbox != nil {
		if err := s.sandbox.Stop(ctx, false); err != nil {
			s.log.WithError(err).Warn("failed to stop sandbox during cleanup")
		}
		if err := s.sandbox.Delete(ctx); err != nil {
			s.log.WithError(err).Warn("failed to delete sandbox during cleanup")
		}
	}

	s.cancel()
	s.mu.Unlock()

	// Wait for any orphan cleanup operations that may have started
	// (e.g., from createSandbox detecting s.cleaned=true)
	s.cleanupWg.Wait()

	return &taskAPI.DeleteResponse{
		ExitedAt:   timestamppb.New(time.Now()),
		ExitStatus: 128 + 9, // SIGKILL
	}, nil
}

func (s *service) Create(ctx context.Context, r *taskAPI.CreateTaskRequest) (*taskAPI.CreateTaskResponse, error) {
	s.log.WithField("container", r.ID).Info("Create() start")
	defer s.log.WithField("container", r.ID).Info("Create() end")

	// Check if service is already cleaned up before starting
	s.mu.RLock()
	if s.cleaned {
		s.mu.RUnlock()
		return nil, fmt.Errorf("service already cleaned up")
	}
	s.mu.RUnlock()

	type Result struct {
		container *container
		err       error
	}
	resultCh := make(chan Result, 1)

	// NOTE: Do NOT hold the lock while calling createContainer
	// because createSandbox (called by createContainer) needs to acquire the lock
	// to safely assign s.sandbox and s.hpid
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.log.WithField("panic", r).Error("Create panic recovered")
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

		// Acquire lock to update container map and get hpid
		s.mu.Lock()
		container := res.container
		container.status = task.Status_CREATED
		s.containers[r.ID] = container
		hpid := s.hpid
		s.mu.Unlock()

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
			Pid:        hpid,
		}

		return &taskAPI.CreateTaskResponse{
			Pid: hpid,
		}, nil
	}
}

func (s *service) Start(ctx context.Context, r *taskAPI.StartRequest) (*taskAPI.StartResponse, error) {
	s.log.WithField("container", r.ID).Info("Start() start")
	defer s.log.WithField("container", r.ID).Info("Start() end")

	// Get container under lock, but release before calling startContainer
	// to avoid deadlock (startContainer calls getSandbox which needs RLock)
	s.mu.Lock()
	c, ok := s.containers[r.ID]
	s.mu.Unlock()

	if !ok {
		return nil, errdefs.ToGRPCf(errdefs.ErrNotFound, "container %s not found", r.ID)
	}

	errCh := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.log.WithField("panic", r).Error("Start panic recovered")
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
	s.log.WithField("container", r.ID).Info("Delete() start")
	defer s.log.WithField("container", r.ID).Info("Delete() end")

	// Get container under lock, but release before calling deleteContainer
	// to avoid deadlock (deleteContainer calls getSandbox which needs RLock)
	s.mu.Lock()
	c, ok := s.containers[r.ID]
	s.mu.Unlock()

	if !ok {
		return nil, errdefs.ToGRPCf(errdefs.ErrNotFound, "container %s not found", r.ID)
	}

	errCh := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.log.WithField("panic", r).Error("Delete panic recovered")
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

		// Re-acquire lock to remove from map
		s.mu.Lock()
		delete(s.containers, r.ID)
		s.mu.Unlock()

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
	s.mu.RLock()
	defer s.mu.RUnlock()

	c, ok := s.containers[r.ID]
	if !ok {
		return nil, errdefs.ToGRPCf(errdefs.ErrNotFound, "container %s not found", r.ID)
	}

	return &taskAPI.StateResponse{
		ID:         c.id,
		Bundle:     c.bundle,
		Pid:        s.hpid,
		Status:     c.status,
		Stdin:      c.stdin,
		Stdout:     c.stdout,
		Stderr:     c.stderr,
		Terminal:   c.terminal,
		ExitStatus: c.exit,
		ExitedAt:   timestamppb.New(c.exitTime),
	}, nil
}

func (s *service) Kill(ctx context.Context, r *taskAPI.KillRequest) (*emptypb.Empty, error) {
	s.log.WithField("container", r.ID).WithField("signal", r.Signal).Info("Kill()")

	s.mu.RLock()
	c, ok := s.containers[r.ID]
	sandbox := s.sandbox
	s.mu.RUnlock()

	if !ok {
		return nil, errdefs.ToGRPCf(errdefs.ErrNotFound, "container %s not found", r.ID)
	}

	if sandbox == nil {
		return nil, fmt.Errorf("sandbox not found")
	}

	// Add timeout if not already set
	opCtx, cancel := withOperationTimeout(ctx)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.log.WithField("panic", r).Error("Kill panic recovered")
				errCh <- fmt.Errorf("kill panic: %v", r)
			}
		}()

		errCh <- sandbox.KillContainer(opCtx, c.id, syscall.Signal(r.Signal), r.All)
	}()

	select {
	case <-opCtx.Done():
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
	sandbox := s.getSandbox()
	if sandbox == nil {
		return nil, fmt.Errorf("sandbox not found")
	}

	opCtx, cancel := withOperationTimeout(ctx)
	defer cancel()

	if err := sandbox.PauseContainer(opCtx, r.ID); err != nil {
		return nil, errdefs.ToGRPC(err)
	}

	s.events <- &eventstypes.TaskPaused{
		ContainerID: r.ID,
	}

	return empty, nil
}

func (s *service) Resume(ctx context.Context, r *taskAPI.ResumeRequest) (*emptypb.Empty, error) {
	sandbox := s.getSandbox()
	if sandbox == nil {
		return nil, fmt.Errorf("sandbox not found")
	}

	opCtx, cancel := withOperationTimeout(ctx)
	defer cancel()

	if err := sandbox.ResumeContainer(opCtx, r.ID); err != nil {
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
	s.mu.RLock()
	c, ok := s.containers[r.ID]
	sandbox := s.sandbox
	s.mu.RUnlock()

	if !ok {
		return nil, errdefs.ToGRPCf(errdefs.ErrNotFound, "container %s not found", r.ID)
	}

	if sandbox == nil {
		return nil, fmt.Errorf("sandbox not found")
	}

	execID := r.ExecID
	if execID == "" {
		execID = c.id
	}

	opCtx, cancel := withOperationTimeout(ctx)
	defer cancel()

	if err := sandbox.WinsizeProcess(opCtx, c.id, execID, r.Height, r.Width); err != nil {
		return nil, errdefs.ToGRPC(err)
	}

	return empty, nil
}

func (s *service) CloseIO(ctx context.Context, r *taskAPI.CloseIORequest) (*emptypb.Empty, error) {
	s.mu.RLock()
	c, ok := s.containers[r.ID]
	s.mu.RUnlock()

	if !ok {
		return nil, errdefs.ToGRPCf(errdefs.ErrNotFound, "container %s not found", r.ID)
	}

	// Close stdin if it exists - use container's ioMu for IO state
	c.ioMu.Lock()
	if c.stdinCloser != nil {
		if err := c.stdinCloser.Close(); err != nil {
			s.log.WithError(err).WithField("container", r.ID).Warn("failed to close stdin")
		}
		c.stdinCloser = nil
	}
	c.ioMu.Unlock()

	return empty, nil
}

func (s *service) Checkpoint(ctx context.Context, r *taskAPI.CheckpointTaskRequest) (*emptypb.Empty, error) {
	return nil, errdefs.ToGRPC(errdefs.ErrNotImplemented)
}

func (s *service) Update(ctx context.Context, r *taskAPI.UpdateTaskRequest) (*emptypb.Empty, error) {
	return nil, errdefs.ToGRPC(errdefs.ErrNotImplemented)
}

func (s *service) Wait(ctx context.Context, r *taskAPI.WaitRequest) (*taskAPI.WaitResponse, error) {
	s.log.WithField("container", r.ID).Debug("Wait() called")

	s.mu.RLock()
	c, ok := s.containers[r.ID]
	if !ok {
		s.mu.RUnlock()
		return nil, errdefs.ToGRPCf(errdefs.ErrNotFound, "container %s not found", r.ID)
	}

	// If already stopped, return cached status
	if c.status == task.Status_STOPPED && !c.exitTime.IsZero() {
		exitedAt := timestamppb.New(c.exitTime)
		exitCode := c.exit
		s.mu.RUnlock()
		s.log.WithField("container", r.ID).WithField("exit", exitCode).Debug("Wait() returning cached exit status")
		return &taskAPI.WaitResponse{
			ExitStatus: exitCode,
			ExitedAt:   exitedAt,
		}, nil
	}

	// Get the exit channel - this channel is CLOSED (not sent to) when process exits
	// Closing a channel allows all waiters to be notified simultaneously (broadcast)
	exitCh := c.exitCh
	s.mu.RUnlock()

	if exitCh == nil {
		return nil, fmt.Errorf("container %s exit channel not initialized", r.ID)
	}

	s.log.WithField("container", r.ID).Debug("Wait() waiting on exitCh")

	// Wait for exit channel to be closed (broadcast signal)
	select {
	case <-exitCh:
		// Channel was closed, process has exited
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	// Read exit status from container (set by waitContainerProcess before closing exitCh)
	s.mu.RLock()
	exitCode := c.exit
	exitedAt := timestamppb.New(c.exitTime)
	s.mu.RUnlock()

	s.log.WithField("container", r.ID).WithField("exit", exitCode).Debug("Wait() got exit code")

	return &taskAPI.WaitResponse{
		ExitStatus: exitCode,
		ExitedAt:   exitedAt,
	}, nil
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
