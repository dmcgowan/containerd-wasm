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
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/cgroups"
	"github.com/containerd/console"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/mount"
	rproc "github.com/containerd/containerd/runtime/proc"
	"github.com/containerd/containerd/runtime/v2/task"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

type Exit struct {
	Pid    int
	Status int
}

// NewContainer returns a new runc container
func NewContainer(ctx context.Context, platform rproc.Platform, r *task.CreateTaskRequest, ec chan<- Exit) (c *Container, err error) {
	logrus.Infof("new WASM container %s ...", r.Bundle)

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
				logrus.Errorf("failed to mkdir %s: %v", rootfs, err)
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
	rootRemap = ".:/"
	var args []string
	argsLen := len(spec.Process.Args)
	if argsLen > 0 {
		args = append(args, spec.Process.Args[0])
		if argsLen > 1 {
			args = append(args, "--")
			args = append(args, spec.Process.Args[1:]...)
		}
	}

	p := &process{
		id: r.ID,
		stdio: rproc.Stdio{
			Stdin:    r.Stdin,
			Stdout:   r.Stdout,
			Stderr:   r.Stderr,
			Terminal: r.Terminal,
		},
		remaps: []string{
			rootRemap,
		},
		rootfs: rootfs,
		exited: make(chan struct{}),
		ec:     ec,
		env:    spec.Process.Env,
		args:   args,
	}

	container := &Container{
		ID:        r.ID,
		Bundle:    r.Bundle,
		process:   p,
		processes: make(map[string]rproc.Process),
	}

	pid := p.Pid()
	if pid > 0 {
		cg, err := cgroups.Load(cgroups.V1, cgroups.PidPath(pid))
		if err != nil {
			logrus.WithError(err).Errorf("loading cgroup for %d", pid)
		}
		container.cgroup = cg
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
	cgroup    cgroups.Cgroup
	process   rproc.Process
	processes map[string]rproc.Process
}

// All processes in the container
func (c *Container) All() (o []rproc.Process) {
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
func (c *Container) ExecdProcesses() (o []rproc.Process) {
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
func (c *Container) Cgroup() cgroups.Cgroup {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cgroup
}

// CgroupSet sets the cgroup to the container
func (c *Container) CgroupSet(cg cgroups.Cgroup) {
	c.mu.Lock()
	c.cgroup = cg
	c.mu.Unlock()
}

// Process returns the process by id
func (c *Container) Process(id string) (rproc.Process, error) {
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
func (c *Container) ProcessAdd(process rproc.Process) {
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
func (c *Container) Start(ctx context.Context, r *task.StartRequest) (rproc.Process, error) {
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
		cg, err := cgroups.Load(cgroups.V1, cgroups.PidPath(p.Pid()))
		if err != nil {
			logrus.WithError(err).Errorf("loading cgroup for %d", p.Pid())
		}
		c.cgroup = cg
	}
	logrus.Info("returning process", p)
	return p, nil
}

// Delete the container or a process by id
func (c *Container) Delete(ctx context.Context, r *task.DeleteRequest) (rproc.Process, error) {
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
func (c *Container) Exec(ctx context.Context, r *task.ExecProcessRequest) (rproc.Process, error) {
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
func (c *Container) ResizePty(ctx context.Context, r *task.ResizePtyRequest) error {
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
func (c *Container) Kill(ctx context.Context, r *task.KillRequest) error {
	p, err := c.Process(r.ExecID)
	if err != nil {
		return err
	}
	return p.Kill(ctx, r.Signal, r.All)
}

// CloseIO of a process
func (c *Container) CloseIO(ctx context.Context, r *task.CloseIORequest) error {
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
func (c *Container) Checkpoint(ctx context.Context, r *task.CheckpointTaskRequest) error {
	return errors.Wrap(errdefs.ErrNotImplemented, "checkpoint not implemented")
}

// Update the resource information of a running container
func (c *Container) Update(ctx context.Context, r *task.UpdateTaskRequest) error {
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

type process struct {
	mu sync.Mutex

	id         string
	pid        int
	exitStatus int
	exitTime   time.Time
	stdio      rproc.Stdio
	stdin      io.Closer
	process    *os.Process
	exited     chan struct{}
	ec         chan<- Exit

	rootfs string
	remaps []string
	env    []string
	args   []string

	waitError error
}

func (p *process) ID() string {
	return p.id
}

func (p *process) Pid() int {
	return p.pid
}

func (p *process) ExitStatus() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exitStatus
}

func (p *process) ExitedAt() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exitTime
}

func (p *process) Stdin() io.Closer {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stdin
}

func (p *process) Stdio() rproc.Stdio {
	return p.stdio
}

func (p *process) Status(context.Context) (string, error) {
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

func (p *process) Wait() {
	<-p.exited
}

func (p *process) Resize(ws console.WinSize) error {
	return nil
}

func (p *process) Start(context.Context) (err error) {
	var args []string

	args = append(args, "run", "--dir=.")

	// for _, rm := range p.remaps {
	// 	args = append(args, "--mapdir="+rm)
	// }

	for _, env := range p.env {
		// Ignore the PATH env
		if !strings.HasPrefix(env, "PATH=") {
			args = append(args, "--env="+env)
		}
	}
	args = append(args, p.args...)
	logrus.Infof("exec wasmer %s", strings.Join(args, " "))
	cmd := exec.Command("wasmer", args...)
	cmd.Dir = p.rootfs

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

func (p *process) Delete(context.Context) error {
	return nil
}

func (p *process) Kill(context.Context, uint32, bool) error {
	p.mu.Lock()
	running := p.process != nil
	p.mu.Unlock()

	if !running {
		return errors.New("not started")
	}

	return p.process.Kill()
}

func (p *process) SetExited(status int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.exitStatus = status
}
