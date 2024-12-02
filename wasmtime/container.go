//go:build linux
// +build linux

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

package wasmtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/cgroups/v3/cgroup1"
	"github.com/containerd/console"
	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/stdio"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

type Exit struct {
	Pid    int
	Status int
}

// NewContainer returns a new runc container
func NewContainer(ctx context.Context, platform stdio.Platform, r *taskAPI.CreateTaskRequest, ec chan<- Exit) (c *Container, err error) {
	//ns, err := namespaces.NamespaceRequired(ctx)
	//if err != nil {
	//	return nil, errors.Wrap(err, "create namespace")
	//}

	//var opts options.Options
	//if r.Options != nil {
	//	v, err := typeurl.UnmarshalAny(r.Options)
	//	if err != nil {
	//		return nil, err
	//	}
	//	// TODO: Use custom options type
	//	opts = *v.(*options.Options)
	//}

	//if err := WriteRuntime(r.Bundle, opts.BinaryName); err != nil {
	//	return nil, err
	//}

	b, err := ioutil.ReadFile(filepath.Join(r.Bundle, "config.json"))
	if err != nil {
		return nil, errors.Wrap(err, "failed to read spec")
	}

	var spec specs.Spec
	if err := json.Unmarshal(b, &spec); err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal spec")
	}

	if spec.Process == nil {
		return nil, errors.Wrapf(errdefs.ErrInvalidArgument, "no process specification")
	}

	var rootRemap string

	rootfs := ""
	for _, m := range r.Rootfs {
		if m.Type == "bind" {
			continue
		}
		if rootfs == "" {
			rootfs = filepath.Join(r.Bundle, "rootfs")
			if err := os.MkdirAll(rootfs, 0711); err != nil {
				return nil, err
			}
		}
	}

	defer func() {
		if err != nil && rootfs != "" {
			if err2 := mount.UnmountAll(rootfs, 0); err2 != nil {
				logrus.WithError(err2).Warn("failed to cleanup rootfs mount")
			}
		}
	}()

	if rootfs != "" {
		for _, rm := range r.Rootfs {
			m := &mount.Mount{
				Type:    rm.Type,
				Source:  rm.Source,
				Options: rm.Options,
			}
			if err := m.Mount(rootfs); err != nil {
				return nil, errors.Wrapf(err, "failed to mount rootfs component %v", m)
			}
		}
	} else if len(r.Rootfs) > 0 {
		rootfs = r.Rootfs[0].Source
	} else if spec.Root != nil && spec.Root.Path != "" {
		rootfs = spec.Root.Path
	} else {
		return nil, errors.Wrapf(errdefs.ErrInvalidArgument, "no root provided")
	}
	rootRemap = fmt.Sprintf("%s::/", rootfs) // --dir <HOST_DIR[::GUEST_DIR]> (wasmtime v27)
	if len(spec.Process.Args) > 0 {
		// TODO: bound this
		spec.Process.Args[0] = filepath.Join(rootfs, spec.Process.Args[0])
	}

	p := &Process{
		id: r.ID,
		stdio: stdio.Stdio{
			Stdin:    r.Stdin,
			Stdout:   r.Stdout,
			Stderr:   r.Stderr,
			Terminal: r.Terminal,
		},
		remaps: []string{
			rootRemap,
		},
		exited: make(chan struct{}),
		ec:     ec,
		env:    spec.Process.Env,
		args:   spec.Process.Args,
	}

	container := &Container{
		ID:        r.ID,
		Bundle:    r.Bundle,
		process:   p,
		processes: make(map[string]*Process),
	}

	pid := p.Pid()
	if pid > 0 {
		cg, err := cgroup1.Load(cgroup1.PidPath(pid))
		if err != nil {
			logrus.WithError(err).Errorf("loading cgroup for %d", pid)
			container.cgroup = cg
		}
	}
	logrus.Infof("process created: %#v", p)

	return container, nil
}

//// ReadRuntime reads the runtime information from the path
//func ReadRuntime(path string) (string, error) {
//	data, err := ioutil.ReadFile(filepath.Join(path, "runtime"))
//	if err != nil {
//		return "", err
//	}
//	return string(data), nil
//}
//
//// WriteRuntime writes the runtime information into the path
//func WriteRuntime(path, runtime string) error {
//	return ioutil.WriteFile(filepath.Join(path, "runtime"), []byte(runtime), 0600)
//}

// Container for operating on a runc container and its processes
type Container struct {
	mu sync.Mutex

	// ID of the container
	ID string
	// Bundle path
	Bundle string
	// Root Remap
	RootRemap string

	ec        chan<- Exit
	cgroup    cgroup1.Cgroup
	process   *Process
	processes map[string]*Process
}

// All processes in the container
func (c *Container) All() (o []*Process) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, p := range c.processes {
		o = append(o, p)
	}
	if c.process != nil {
		o = append(o, c.process)
	}
	return o
}

// ExecdProcesses added to the container
func (c *Container) ExecdProcesses() (o []*Process) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range c.processes {
		o = append(o, p)
	}
	return o
}

// Pid of the main process of a container
func (c *Container) Pid() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.process.Pid()
}

// Cgroup of the container
func (c *Container) Cgroup() cgroup1.Cgroup {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cgroup
}

// CgroupSet sets the cgroup to the container
func (c *Container) CgroupSet(cg cgroup1.Cgroup) {
	c.mu.Lock()
	c.cgroup = cg
	c.mu.Unlock()
}

// Process returns the process by id
func (c *Container) Process(id string) (*Process, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if id == "" {
		if c.process == nil {
			return nil, errors.Wrapf(errdefs.ErrFailedPrecondition, "container must be created")
		}
		return c.process, nil
	}
	p, ok := c.processes[id]
	if !ok {
		return nil, errors.Wrapf(errdefs.ErrNotFound, "process does not exist %s", id)
	}
	return p, nil
}

// ProcessExists returns true if the process by id exists
func (c *Container) ProcessExists(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.processes[id]
	return ok
}

// ProcessAdd adds a new process to the container
func (c *Container) ProcessAdd(process *Process) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.processes[process.ID()] = process
}

// ProcessRemove removes the process by id from the container
func (c *Container) ProcessRemove(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.processes, id)
}

// Start a container process
func (c *Container) Start(ctx context.Context, r *taskAPI.StartRequest) (*Process, error) {
	logrus.Info("starting")
	p, err := c.Process(r.ExecID)
	if err != nil {
		return nil, err
	}
	logrus.Info("got process %#v", p)
	if err := p.Start(ctx); err != nil {
		return nil, err
	}

	logrus.Info("done starting", p)
	if c.Cgroup() == nil && p.Pid() > 0 {
		cg, err := cgroup1.Load(cgroup1.PidPath(p.Pid()))
		if err != nil {
			logrus.WithError(err).Errorf("loading cgroup for %d", p.Pid())
		}
		c.cgroup = cg
	}
	logrus.Info("returning process", p)
	return p, nil
}

// Delete the container or a process by id
func (c *Container) Delete(ctx context.Context, r *taskAPI.DeleteRequest) (*Process, error) {
	p, err := c.Process(r.ExecID)
	if err != nil {
		return nil, err
	}
	if err := p.Delete(ctx); err != nil {
		return nil, err
	}
	if r.ExecID != "" {
		c.ProcessRemove(r.ExecID)
	}
	return p, nil
}

// Exec an additional process
func (c *Container) Exec(ctx context.Context, r *taskAPI.ExecProcessRequest) (*Process, error) {
	return nil, errors.Wrap(errdefs.ErrNotImplemented, "exec not implemented")
}

// Pause the container
func (c *Container) Pause(ctx context.Context) error {
	return errors.Wrap(errdefs.ErrNotImplemented, "pause not implemented")
}

// Resume the container
func (c *Container) Resume(ctx context.Context) error {
	return errors.Wrap(errdefs.ErrNotImplemented, "resume not implemented")
}

// ResizePty of a process
func (c *Container) ResizePty(ctx context.Context, r *taskAPI.ResizePtyRequest) error {
	p, err := c.Process(r.ExecID)
	if err != nil {
		return err
	}
	ws := console.WinSize{
		Width:  uint16(r.Width),
		Height: uint16(r.Height),
	}
	return p.Resize(ws)
}

// Kill a process
func (c *Container) Kill(ctx context.Context, r *taskAPI.KillRequest) error {
	p, err := c.Process(r.ExecID)
	if err != nil {
		return err
	}
	return p.Kill(ctx, r.Signal, r.All)
}

// CloseIO of a process
func (c *Container) CloseIO(ctx context.Context, r *taskAPI.CloseIORequest) error {
	p, err := c.Process(r.ExecID)
	if err != nil {
		return err
	}
	if stdin := p.Stdin(); stdin != nil {
		if err := stdin.Close(); err != nil {
			return errors.Wrap(err, "close stdin")
		}
	}
	return nil
}

// Checkpoint the container
func (c *Container) Checkpoint(ctx context.Context, r *taskAPI.CheckpointTaskRequest) error {
	return errors.Wrap(errdefs.ErrNotImplemented, "checkpoint not implemented")
}

// Update the resource information of a running container
func (c *Container) Update(ctx context.Context, r *taskAPI.UpdateTaskRequest) error {
	return errors.Wrap(errdefs.ErrNotImplemented, "update not implemented")
}

// HasPid returns true if the container owns a specific pid
func (c *Container) HasPid(pid int) bool {
	if c.Pid() == pid {
		return true
	}
	for _, p := range c.All() {
		if p.Pid() == pid {
			return true
		}
	}
	return false
}

type Process struct {
	mu sync.Mutex

	id         string
	pid        int
	exitStatus int
	exitTime   time.Time
	stdio      stdio.Stdio
	stdin      io.Closer
	process    *os.Process
	exited     chan struct{}
	ec         chan<- Exit

	remaps []string
	env    []string
	args   []string

	waitError error
}

func (p *Process) ID() string {
	return p.id
}

func (p *Process) Pid() int {
	return p.pid
}

func (p *Process) ExitStatus() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exitStatus
}

func (p *Process) ExitedAt() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exitTime
}

func (p *Process) Stdin() io.Closer {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stdin
}

func (p *Process) Stdio() stdio.Stdio {
	return p.stdio
}

func (p *Process) Status(context.Context) (string, error) {
	select {
	case <-p.exited:
	default:
		p.mu.Lock()
		running := p.process != nil
		p.mu.Unlock()
		if running {
			return "running", nil
		}
		return "created", nil
	}

	return "stopped", nil
}

func (p *Process) Wait() {
	<-p.exited
}

func (p *Process) Resize(ws console.WinSize) error {
	return nil
}

func (p *Process) Start(context.Context) (err error) {
	var args []string
	for _, rm := range p.remaps {
		args = append(args, "--dir="+rm)
	}
	for _, env := range p.env {
		args = append(args, "--env="+env)
	}
	args = append(args, p.args...)
	cmd := exec.Command("wasmtime", args...)

	var in io.Closer
	var closers []io.Closer
	if p.stdio.Stdin != "" {
		stdin, err := os.OpenFile(p.stdio.Stdin, os.O_RDONLY, 0)
		if err != nil {
			return errors.Wrapf(err, "unable to open stdin: %s", p.stdio.Stdin)
		}
		defer func() {
			if err != nil {
				stdin.Close()
			}
		}()
		cmd.Stdin = stdin
		in = stdin
		closers = append(closers, stdin)
	}

	if p.stdio.Stdout != "" {
		stdout, err := os.OpenFile(p.stdio.Stdout, os.O_WRONLY, 0)
		if err != nil {
			return errors.Wrapf(err, "unable to open stdout: %s", p.stdio.Stdout)
		}
		defer func() {
			if err != nil {
				stdout.Close()
			}
		}()
		cmd.Stdout = stdout
		closers = append(closers, stdout)
	}

	if p.stdio.Stderr != "" {
		stderr, err := os.OpenFile(p.stdio.Stderr, os.O_WRONLY, 0)
		if err != nil {
			return errors.Wrapf(err, "unable to open stderr: %s", p.stdio.Stderr)
		}
		defer func() {
			if err != nil {
				stderr.Close()
			}
		}()
		cmd.Stderr = stderr
		closers = append(closers, stderr)
	}

	p.mu.Lock()
	if p.process != nil {
		return errors.Wrap(errdefs.ErrFailedPrecondition, "already running")
	}
	if err := cmd.Start(); err != nil {
		p.mu.Unlock()
		return err
	}
	p.process = cmd.Process
	p.stdin = in
	p.mu.Unlock()

	go func() {
		waitStatus, err := p.process.Wait()
		p.mu.Lock()
		p.exitTime = time.Now()
		if err != nil {
			p.exitStatus = -1
			logrus.WithError(err).Errorf("wait returned error")
		} else if waitStatus != nil {
			// TODO: Make this cross platform
			p.exitStatus = int(waitStatus.Sys().(syscall.WaitStatus))
		}
		p.mu.Unlock()

		close(p.exited)

		p.ec <- Exit{
			Pid:    p.pid,
			Status: p.exitStatus,
		}

		for _, c := range closers {
			c.Close()
		}
	}()

	return nil
}

func (p *Process) Delete(context.Context) error {
	return nil
}

func (p *Process) Kill(context.Context, uint32, bool) error {
	p.mu.Lock()
	running := p.process != nil
	p.mu.Unlock()

	if !running {
		return errors.New("not started")
	}

	return p.process.Kill()
}

func (p *Process) SetExited(status int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.exitStatus = status
}
