// Copyright 2018 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package kernel provides an emulation of the Linux kernel.
//
// See README.md for a detailed overview.
//
// Lock order (outermost locks must be taken first):
//
// Kernel.extMu
//   ThreadGroup.timerMu
//     ktime.Timer.mu (for IntervalTimer)
//       TaskSet.mu
//         SignalHandlers.mu
//           Task.mu
//
// Locking SignalHandlers.mu in multiple SignalHandlers requires locking
// TaskSet.mu exclusively first. Locking Task.mu in multiple Tasks at the same
// time requires locking all of their signal mutexes first.
package kernel

import (
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"gvisor.googlesource.com/gvisor/pkg/abi/linux"
	"gvisor.googlesource.com/gvisor/pkg/cpuid"
	"gvisor.googlesource.com/gvisor/pkg/log"
	"gvisor.googlesource.com/gvisor/pkg/sentry/arch"
	"gvisor.googlesource.com/gvisor/pkg/sentry/context"
	"gvisor.googlesource.com/gvisor/pkg/sentry/fs"
	"gvisor.googlesource.com/gvisor/pkg/sentry/fs/timerfd"
	"gvisor.googlesource.com/gvisor/pkg/sentry/hostcpu"
	"gvisor.googlesource.com/gvisor/pkg/sentry/inet"
	"gvisor.googlesource.com/gvisor/pkg/sentry/kernel/auth"
	"gvisor.googlesource.com/gvisor/pkg/sentry/kernel/epoll"
	"gvisor.googlesource.com/gvisor/pkg/sentry/kernel/sched"
	ktime "gvisor.googlesource.com/gvisor/pkg/sentry/kernel/time"
	"gvisor.googlesource.com/gvisor/pkg/sentry/limits"
	"gvisor.googlesource.com/gvisor/pkg/sentry/loader"
	"gvisor.googlesource.com/gvisor/pkg/sentry/mm"
	"gvisor.googlesource.com/gvisor/pkg/sentry/platform"
	"gvisor.googlesource.com/gvisor/pkg/sentry/socket/netlink/port"
	sentrytime "gvisor.googlesource.com/gvisor/pkg/sentry/time"
	"gvisor.googlesource.com/gvisor/pkg/sentry/uniqueid"
	"gvisor.googlesource.com/gvisor/pkg/state"
	"gvisor.googlesource.com/gvisor/pkg/tcpip"
)

// Kernel represents an emulated Linux kernel. It must be initialized by calling
// Init() or LoadFrom().
//
// +stateify savable
type Kernel struct {
	// extMu serializes external changes to the Kernel with calls to
	// Kernel.SaveTo. (Kernel.SaveTo requires that the state of the Kernel
	// remains frozen for the duration of the call; it requires that the Kernel
	// is paused as a precondition, which ensures that none of the tasks
	// running within the Kernel can affect its state, but extMu is required to
	// ensure that concurrent users of the Kernel *outside* the Kernel's
	// control cannot affect its state by calling e.g.
	// Kernel.SendExternalSignal.)
	extMu sync.Mutex `state:"nosave"`

	// started is true if Start has been called. Unless otherwise specified,
	// all Kernel fields become immutable once started becomes true.
	started bool `state:"nosave"`

	// All of the following fields are immutable unless otherwise specified.

	// Platform is the platform that is used to execute tasks in the
	// created Kernel. It is embedded so that Kernel can directly serve as
	// Platform in mm logic and also serve as platform.MemoryProvider in
	// filemem S/R logic.
	platform.Platform `state:"nosave"`

	// See InitKernelArgs for the meaning of these fields.
	featureSet        *cpuid.FeatureSet
	timekeeper        *Timekeeper
	tasks             *TaskSet
	rootUserNamespace *auth.UserNamespace
	networkStack      inet.Stack `state:"nosave"`
	applicationCores  uint
	useHostCores      bool
	extraAuxv         []arch.AuxEntry
	vdso              *loader.VDSO
	rootUTSNamespace  *UTSNamespace
	rootIPCNamespace  *IPCNamespace

	// mounts holds the state of the virtual filesystem. mounts is initially
	// nil, and must be set by calling Kernel.SetRootMountNamespace before
	// Kernel.CreateProcess can succeed.
	mounts *fs.MountNamespace

	// globalInit is the thread group whose leader has ID 1 in the root PID
	// namespace. globalInit is stored separately so that it is accessible even
	// after all tasks in the thread group have exited, such that ID 1 is no
	// longer mapped.
	//
	// globalInit is mutable until it is assigned by the first successful call
	// to CreateProcess, and is protected by extMu.
	globalInit *ThreadGroup

	// realtimeClock is a ktime.Clock based on timekeeper's Realtime.
	realtimeClock *timekeeperClock

	// monotonicClock is a ktime.Clock based on timekeeper's Monotonic.
	monotonicClock *timekeeperClock

	// syslog is the kernel log.
	syslog syslog

	// cpuClock is incremented every linux.ClockTick. cpuClock is used to
	// measure task CPU usage, since sampling monotonicClock twice on every
	// syscall turns out to be unreasonably expensive. This is similar to how
	// Linux does task CPU accounting on x86 (CONFIG_IRQ_TIME_ACCOUNTING),
	// although Linux also uses scheduler timing information to improve
	// resolution (kernel/sched/cputime.c:cputime_adjust()), which we can't do
	// since "preeemptive" scheduling is managed by the Go runtime, which
	// doesn't provide this information.
	//
	// cpuClock is mutable, and is accessed using atomic memory operations.
	cpuClock uint64

	// cpuClockTicker increments cpuClock.
	cpuClockTicker *ktime.Timer `state:"nosave"`

	// fdMapUids is an ever-increasing counter for generating FDMap uids.
	//
	// fdMapUids is mutable, and is accessed using atomic memory operations.
	fdMapUids uint64

	// uniqueID is used to generate unique identifiers.
	//
	// uniqueID is mutable, and is accessed using atomic memory operations.
	uniqueID uint64

	// nextInotifyCookie is a monotonically increasing counter used for
	// generating unique inotify event cookies.
	//
	// nextInotifyCookie is mutable, and is accesed using atomic memory
	// operations.
	nextInotifyCookie uint32

	// netlinkPorts manages allocation of netlink socket port IDs.
	netlinkPorts *port.Manager

	// exitErr is the error causing the sandbox to exit, if any. It is
	// protected by extMu.
	exitErr error `state:"nosave"`

	// danglingEndpoints is used to save / restore tcpip.DanglingEndpoints.
	danglingEndpoints struct{} `state:".([]tcpip.Endpoint)"`
}

// InitKernelArgs holds arguments to Init.
type InitKernelArgs struct {
	// FeatureSet is the emulated CPU feature set.
	FeatureSet *cpuid.FeatureSet

	// Timekeeper manages time for all tasks in the system.
	Timekeeper *Timekeeper

	// RootUserNamespace is the root user namespace.
	RootUserNamespace *auth.UserNamespace

	// NetworkStack is the TCP/IP network stack. NetworkStack may be nil.
	NetworkStack inet.Stack

	// ApplicationCores is the number of logical CPUs visible to sandboxed
	// applications. The set of logical CPU IDs is [0, ApplicationCores); thus
	// ApplicationCores is analogous to Linux's nr_cpu_ids, the index of the
	// most significant bit in cpu_possible_mask + 1.
	ApplicationCores uint

	// If UseHostCores is true, Task.CPU() returns the task goroutine's CPU
	// instead of a virtualized CPU number, and Task.CopyToCPUMask() is a
	// no-op. If ApplicationCores is less than hostcpu.MaxPossibleCPU(), it
	// will be overridden.
	UseHostCores bool

	// ExtraAuxv contains additional auxiliary vector entries that are added to
	// each process by the ELF loader.
	ExtraAuxv []arch.AuxEntry

	// Vdso holds the VDSO and its parameter page.
	Vdso *loader.VDSO

	// RootUTSNamespace is the root UTS namepsace.
	RootUTSNamespace *UTSNamespace

	// RootIPCNamespace is the root IPC namepsace.
	RootIPCNamespace *IPCNamespace
}

// Init initialize the Kernel with no tasks.
//
// Callers must manually set Kernel.Platform before caling Init.
func (k *Kernel) Init(args InitKernelArgs) error {
	if args.FeatureSet == nil {
		return fmt.Errorf("FeatureSet is nil")
	}
	if args.Timekeeper == nil {
		return fmt.Errorf("Timekeeper is nil")
	}
	if args.RootUserNamespace == nil {
		return fmt.Errorf("RootUserNamespace is nil")
	}
	if args.ApplicationCores == 0 {
		return fmt.Errorf("ApplicationCores is 0")
	}

	k.featureSet = args.FeatureSet
	k.timekeeper = args.Timekeeper
	k.tasks = newTaskSet()
	k.rootUserNamespace = args.RootUserNamespace
	k.rootUTSNamespace = args.RootUTSNamespace
	k.rootIPCNamespace = args.RootIPCNamespace
	k.networkStack = args.NetworkStack
	k.applicationCores = args.ApplicationCores
	if args.UseHostCores {
		k.useHostCores = true
		maxCPU, err := hostcpu.MaxPossibleCPU()
		if err != nil {
			return fmt.Errorf("Failed to get maximum CPU number: %v", err)
		}
		minAppCores := uint(maxCPU) + 1
		if k.applicationCores < minAppCores {
			log.Infof("UseHostCores enabled: increasing ApplicationCores from %d to %d", k.applicationCores, minAppCores)
			k.applicationCores = minAppCores
		}
	}
	k.extraAuxv = args.ExtraAuxv
	k.vdso = args.Vdso
	k.realtimeClock = &timekeeperClock{tk: args.Timekeeper, c: sentrytime.Realtime}
	k.monotonicClock = &timekeeperClock{tk: args.Timekeeper, c: sentrytime.Monotonic}
	k.netlinkPorts = port.New()

	return nil
}

// SaveTo saves the state of k to w.
//
// Preconditions: The kernel must be paused throughout the call to SaveTo.
func (k *Kernel) SaveTo(w io.Writer) error {
	saveStart := time.Now()
	ctx := k.SupervisorContext()

	// Do not allow other Kernel methods to affect it while it's being saved.
	k.extMu.Lock()
	defer k.extMu.Unlock()

	// Stop time.
	k.pauseTimeLocked()
	defer k.resumeTimeLocked()

	// Flush write operations on open files so data reaches backing storage.
	if err := k.tasks.flushWritesToFiles(ctx); err != nil {
		return err
	}

	// Remove all epoll waiter objects from underlying wait queues.
	// NOTE: for programs to resume execution in future snapshot scenarios,
	// we will need to re-establish these waiter objects after saving.
	k.tasks.unregisterEpollWaiters()

	// Clear the dirent cache before saving because Dirents must be Loaded in a
	// particular order (parents before children), and Loading dirents from a cache
	// breaks that order.
	k.mounts.FlushMountSourceRefs()

	// Ensure that all pending asynchronous work is complete:
	//   - inode and mount release
	//   - asynchronuous IO
	fs.AsyncBarrier()

	// Once all fs work has completed (flushed references have all been released),
	// reset mount mappings. This allows individual mounts to save how inodes map
	// to filesystem resources. Without this, fs.Inodes cannot be restored.
	fs.SaveInodeMappings()

	// Discard unsavable mappings, such as those for host file descriptors.
	// This must be done after waiting for "asynchronous fs work", which
	// includes async I/O that may touch application memory.
	if err := k.invalidateUnsavableMappings(ctx); err != nil {
		return fmt.Errorf("failed to invalidate unsavable mappings: %v", err)
	}

	// Save the kernel state.
	kernelStart := time.Now()
	var stats state.Stats
	if err := state.Save(w, k, &stats); err != nil {
		return err
	}
	log.Infof("Kernel save stats: %s", &stats)
	log.Infof("Kernel save took [%s].", time.Since(kernelStart))

	// Save the memory state.
	//
	// FIXME: In the future, this should not be dispatched via
	// an abstract memory type. This should be dispatched to a single
	// memory implementation that belongs to the kernel. (There is
	// currently a single implementation anyways, it just needs to be
	// "unabstracted" and reparented appropriately.)
	memoryStart := time.Now()
	if err := k.Platform.Memory().SaveTo(w); err != nil {
		return err
	}
	log.Infof("Memory save took [%s].", time.Since(memoryStart))

	log.Infof("Overall save took [%s].", time.Since(saveStart))

	return nil
}

func (ts *TaskSet) flushWritesToFiles(ctx context.Context) error {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	for t := range ts.Root.tids {
		if fdmap := t.FDMap(); fdmap != nil {
			for _, desc := range fdmap.files {
				if flags := desc.file.Flags(); !flags.Write {
					continue
				}
				if sattr := desc.file.Dirent.Inode.StableAttr; !fs.IsFile(sattr) && !fs.IsDir(sattr) {
					continue
				}
				// Here we need all metadata synced.
				syncErr := desc.file.Fsync(ctx, 0, fs.FileMaxOffset, fs.SyncAll)
				if err := fs.SaveFileFsyncError(syncErr); err != nil {
					name, _ := desc.file.Dirent.FullName(nil /* root */)
					return fmt.Errorf("%q was not sufficiently synced: %v", name, err)
				}
			}
		}
	}
	return nil
}

// Preconditions: The kernel must be paused.
func (k *Kernel) invalidateUnsavableMappings(ctx context.Context) error {
	invalidated := make(map[*mm.MemoryManager]struct{})
	k.tasks.mu.RLock()
	defer k.tasks.mu.RUnlock()
	for t := range k.tasks.Root.tids {
		// We can skip locking Task.mu here since the kernel is paused.
		if mm := t.tc.MemoryManager; mm != nil {
			if _, ok := invalidated[mm]; !ok {
				if err := mm.InvalidateUnsavable(ctx); err != nil {
					return err
				}
				invalidated[mm] = struct{}{}
			}
		}
		// I really wish we just had a sync.Map of all MMs...
		if r, ok := t.runState.(*runSyscallAfterExecStop); ok {
			if err := r.tc.MemoryManager.InvalidateUnsavable(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}

func (ts *TaskSet) unregisterEpollWaiters() {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	for t := range ts.Root.tids {
		if fdmap := t.FDMap(); fdmap != nil {
			for _, desc := range fdmap.files {
				if desc.file != nil {
					if e, ok := desc.file.FileOperations.(*epoll.EventPoll); ok {
						e.UnregisterEpollWaiters()
					}
				}
			}
		}
	}
}

// LoadFrom returns a new Kernel loaded from args.
func (k *Kernel) LoadFrom(r io.Reader, p platform.Platform, net inet.Stack) error {
	loadStart := time.Now()
	if p == nil {
		return fmt.Errorf("Platform is nil")
	}

	k.Platform = p
	k.networkStack = net

	initAppCores := k.applicationCores

	// Load the kernel state.
	kernelStart := time.Now()
	var stats state.Stats
	if err := state.Load(r, k, &stats); err != nil {
		return err
	}
	log.Infof("Kernel load stats: %s", &stats)
	log.Infof("Kernel load took [%s].", time.Since(kernelStart))

	// Load the memory state.
	//
	// See the note in SaveTo.
	memoryStart := time.Now()
	if err := k.Platform.Memory().LoadFrom(r); err != nil {
		return err
	}
	log.Infof("Memory load took [%s].", time.Since(memoryStart))

	// Ensure that all pending asynchronous work is complete:
	//   - namedpipe opening
	//   - inode file opening
	if err := fs.AsyncErrorBarrier(); err != nil {
		return err
	}

	tcpip.AsyncLoading.Wait()

	log.Infof("Overall load took [%s]", time.Since(loadStart))

	// Applications may size per-cpu structures based on k.applicationCores, so
	// it can't change across save/restore. When we are virtualizing CPU
	// numbers, this isn't a problem. However, when we are exposing host CPU
	// assignments, we can't tolerate an increase in the number of host CPUs,
	// which could result in getcpu(2) returning CPUs that applications expect
	// not to exist.
	if k.useHostCores && initAppCores > k.applicationCores {
		return fmt.Errorf("UseHostCores enabled: can't increase ApplicationCores from %d to %d after restore", k.applicationCores, initAppCores)
	}

	return nil
}

// Destroy releases resources owned by k.
//
// Preconditions: There must be no task goroutines running in k.
func (k *Kernel) Destroy() {
	if k.mounts != nil {
		k.mounts.DecRef()
		k.mounts = nil
	}
}

// UniqueID returns a unique identifier.
func (k *Kernel) UniqueID() uint64 {
	id := atomic.AddUint64(&k.uniqueID, 1)
	if id == 0 {
		panic("unique identifier generator wrapped around")
	}
	return id
}

// CreateProcessArgs holds arguments to kernel.CreateProcess.
type CreateProcessArgs struct {
	// Filename is the filename to load.
	//
	// If this is provided as "", then the file will be guessed via Argv[0].
	Filename string

	// Argvv is a list of arguments.
	Argv []string

	// Envv is a list of environment variables.
	Envv []string

	// WorkingDirectory is the initial working directory.
	//
	// This defaults to the root if empty.
	WorkingDirectory string

	// Credentials is the initial credentials.
	Credentials *auth.Credentials

	// FDMap is the initial set of file descriptors. If CreateProcess succeeds,
	// it takes a reference on FDMap.
	FDMap *FDMap

	// Umask is the initial umask.
	Umask uint

	// Limits is the initial resource limits.
	Limits *limits.LimitSet

	// MaxSymlinkTraversals is the maximum number of symlinks to follow
	// during resolution.
	MaxSymlinkTraversals uint

	// UTSNamespace is the initial UTS namespace.
	UTSNamespace *UTSNamespace

	// IPCNamespace is the initial IPC namespace.
	IPCNamespace *IPCNamespace

	// Root optionally contains the dirent that serves as the root for the
	// process. If nil, the mount namespace's root is used as the process'
	// root.
	//
	// Anyone setting Root must donate a reference (i.e. increment it) to
	// keep it alive until it is decremented by CreateProcess.
	Root *fs.Dirent
}

// NewContext returns a context.Context that represents the task that will be
// created by args.NewContext(k).
func (args *CreateProcessArgs) NewContext(k *Kernel) *createProcessContext {
	return &createProcessContext{
		Logger: log.Log(),
		k:      k,
		args:   args,
	}
}

// createProcessContext is a context.Context that represents the context
// associated with a task that is being created.
type createProcessContext struct {
	context.NoopSleeper
	log.Logger
	k    *Kernel
	args *CreateProcessArgs
}

// Value implements context.Context.Value.
func (ctx *createProcessContext) Value(key interface{}) interface{} {
	switch key {
	case CtxKernel:
		return ctx.k
	case CtxPIDNamespace:
		// "The new task ... is in the root PID namespace." -
		// Kernel.CreateProcess
		return ctx.k.tasks.Root
	case CtxUTSNamespace:
		return ctx.args.UTSNamespace
	case CtxIPCNamespace:
		return ctx.args.IPCNamespace
	case auth.CtxCredentials:
		return ctx.args.Credentials
	case fs.CtxRoot:
		if ctx.k.mounts == nil {
			return nil
		}
		return ctx.k.mounts.Root()
	case ktime.CtxRealtimeClock:
		return ctx.k.RealtimeClock()
	case limits.CtxLimits:
		return ctx.args.Limits
	case platform.CtxPlatform:
		return ctx.k
	case uniqueid.CtxGlobalUniqueID:
		return ctx.k.UniqueID()
	case uniqueid.CtxInotifyCookie:
		return ctx.k.GenerateInotifyCookie()
	default:
		return nil
	}
}

// CreateProcess creates a new task in a new thread group with the given
// options. The new task has no parent and is in the root PID namespace.
//
// If k.Start() has already been called, the created task will begin running
// immediately. Otherwise, it will be started when k.Start() is called.
//
// CreateProcess has no analogue in Linux; it is used to create the initial
// application task, as well as processes started by the control server.
func (k *Kernel) CreateProcess(args CreateProcessArgs) (*ThreadGroup, error) {
	k.extMu.Lock()
	defer k.extMu.Unlock()
	log.Infof("EXEC: %v", args.Argv)

	if k.mounts == nil {
		return nil, fmt.Errorf("no kernel MountNamespace")
	}

	tg := NewThreadGroup(k.tasks.Root, NewSignalHandlers(), linux.SIGCHLD, args.Limits, k.monotonicClock)
	ctx := args.NewContext(k)

	// Grab the root directory.
	root := args.Root
	if root == nil {
		root = fs.RootFromContext(ctx)
	}
	defer root.DecRef()
	args.Root = nil

	// Grab the working directory.
	wd := root // Default.
	if args.WorkingDirectory != "" {
		var err error
		wd, err = k.mounts.FindInode(ctx, root, nil, args.WorkingDirectory, args.MaxSymlinkTraversals)
		if err != nil {
			return nil, fmt.Errorf("failed to find initial working directory %q: %v", args.WorkingDirectory, err)
		}
		defer wd.DecRef()
	}

	if args.Filename == "" {
		// Was anything provided?
		if len(args.Argv) == 0 {
			return nil, fmt.Errorf("no filename or command provided")
		}
		if !filepath.IsAbs(args.Argv[0]) {
			return nil, fmt.Errorf("'%s' is not an absolute path", args.Argv[0])
		}
		args.Filename = args.Argv[0]
	}

	// Create a fresh task context.
	tc, err := k.LoadTaskImage(ctx, k.mounts, root, wd, args.MaxSymlinkTraversals, args.Filename, args.Argv, args.Envv, k.featureSet)
	if err != nil {
		return nil, err
	}
	tr := newTaskResources(args.FDMap, newFSContext(root, wd, args.Umask))
	// NewTask unconditionally takes ownership of tr, so we never have to call
	// tr.release.

	// Create the task.
	config := &TaskConfig{
		Kernel:         k,
		ThreadGroup:    tg,
		TaskContext:    tc,
		TaskResources:  tr,
		Credentials:    args.Credentials,
		UTSNamespace:   args.UTSNamespace,
		IPCNamespace:   args.IPCNamespace,
		AllowedCPUMask: sched.NewFullCPUSet(k.applicationCores),
	}
	t, err := k.tasks.NewTask(config)
	if err != nil {
		return nil, err
	}

	// Success.
	if k.started {
		tid := k.tasks.Root.IDOfTask(t)
		t.Start(tid)
	} else if k.globalInit == nil {
		k.globalInit = tg
	}
	return tg, nil
}

// Start starts execution of all tasks in k.
//
// Preconditions: Start may be called exactly once.
func (k *Kernel) Start() error {
	k.extMu.Lock()
	defer k.extMu.Unlock()

	if k.globalInit == nil {
		return fmt.Errorf("kernel contains no tasks")
	}
	if k.started {
		return fmt.Errorf("kernel already started")
	}

	k.started = true
	k.cpuClockTicker = ktime.NewTimer(k.monotonicClock, kernelCPUClockListener{k})
	k.cpuClockTicker.Swap(ktime.Setting{
		Enabled: true,
		Period:  linux.ClockTick,
	})
	// If k was created by LoadKernelFrom, timers were stopped during
	// Kernel.SaveTo and need to be resumed. If k was created by NewKernel,
	// this is a no-op.
	k.resumeTimeLocked()
	// Start task goroutines.
	k.tasks.mu.RLock()
	defer k.tasks.mu.RUnlock()
	for t, tid := range k.tasks.Root.tids {
		t.Start(tid)
	}
	return nil
}

// pauseTimeLocked pauses all Timers and Timekeeper updates.
//
// Preconditions: Any task goroutines running in k must be stopped. k.extMu
// must be locked.
func (k *Kernel) pauseTimeLocked() {
	// k.cpuClockTicker may be nil since Kernel.SaveTo() may be called before
	// Kernel.Start().
	if k.cpuClockTicker != nil {
		k.cpuClockTicker.Pause()
	}

	// By precondition, nothing else can be interacting with PIDNamespace.tids
	// or FDMap.files, so we can iterate them without synchronization. (We
	// can't hold the TaskSet mutex when pausing thread group timers because
	// thread group timers call ThreadGroup.SendSignal, which takes the TaskSet
	// mutex, while holding the Timer mutex.)
	for t := range k.tasks.Root.tids {
		if t == t.tg.leader {
			t.tg.tm.pause()
		}
		// This means we'll iterate ThreadGroups and FDMaps shared by multiple
		// tasks repeatedly, but ktime.Timer.Pause is idempotent so this is
		// harmless.
		for _, it := range t.tg.timers {
			it.PauseTimer()
		}
		if fdm := t.tr.FDMap; fdm != nil {
			for _, desc := range fdm.files {
				if tfd, ok := desc.file.FileOperations.(*timerfd.TimerOperations); ok {
					tfd.PauseTimer()
				}
			}
		}
	}
	k.timekeeper.PauseUpdates()
}

// resumeTimeLocked resumes all Timers and Timekeeper updates. If
// pauseTimeLocked has not been previously called, resumeTimeLocked has no
// effect.
//
// Preconditions: Any task goroutines running in k must be stopped. k.extMu
// must be locked.
func (k *Kernel) resumeTimeLocked() {
	if k.cpuClockTicker != nil {
		k.cpuClockTicker.Resume()
	}

	k.timekeeper.ResumeUpdates()
	for t := range k.tasks.Root.tids {
		if t == t.tg.leader {
			t.tg.tm.resume()
		}
		for _, it := range t.tg.timers {
			it.ResumeTimer()
		}
		if fdm := t.tr.FDMap; fdm != nil {
			for _, desc := range fdm.files {
				if tfd, ok := desc.file.FileOperations.(*timerfd.TimerOperations); ok {
					tfd.ResumeTimer()
				}
			}
		}
	}
}

// WaitExited blocks until all tasks in k have exited.
func (k *Kernel) WaitExited() {
	k.tasks.liveGoroutines.Wait()
}

// Kill requests that all tasks in k immediately exit as if group exiting with
// status es. Kill does not wait for tasks to exit.
func (k *Kernel) Kill(es ExitStatus) {
	k.extMu.Lock()
	defer k.extMu.Unlock()
	k.tasks.Kill(es)
}

// Pause requests that all tasks in k temporarily stop executing, and blocks
// until all tasks in k have stopped. Multiple calls to Pause nest and require
// an equal number of calls to Unpause to resume execution.
func (k *Kernel) Pause() {
	k.extMu.Lock()
	k.tasks.BeginExternalStop()
	k.extMu.Unlock()
	k.tasks.runningGoroutines.Wait()
}

// Unpause ends the effect of a previous call to Pause. If Unpause is called
// without a matching preceding call to Pause, Unpause may panic.
func (k *Kernel) Unpause() {
	k.extMu.Lock()
	defer k.extMu.Unlock()
	k.tasks.EndExternalStop()
}

// SendExternalSignal injects a signal into the kernel.
//
// context is used only for debugging to describe how the signal was received.
//
// Preconditions: Kernel must have an init process.
func (k *Kernel) SendExternalSignal(info *arch.SignalInfo, context string) {
	k.extMu.Lock()
	defer k.extMu.Unlock()
	k.sendExternalSignal(info, context)
}

// FeatureSet returns the FeatureSet.
func (k *Kernel) FeatureSet() *cpuid.FeatureSet {
	return k.featureSet
}

// Timekeeper returns the Timekeeper.
func (k *Kernel) Timekeeper() *Timekeeper {
	return k.timekeeper
}

// TaskSet returns the TaskSet.
func (k *Kernel) TaskSet() *TaskSet {
	return k.tasks
}

// RootUserNamespace returns the root UserNamespace.
func (k *Kernel) RootUserNamespace() *auth.UserNamespace {
	return k.rootUserNamespace
}

// RootUTSNamespace returns the root UTSNamespace.
func (k *Kernel) RootUTSNamespace() *UTSNamespace {
	return k.rootUTSNamespace
}

// RootIPCNamespace returns the root IPCNamespace.
func (k *Kernel) RootIPCNamespace() *IPCNamespace {
	return k.rootIPCNamespace
}

// RootMountNamespace returns the MountNamespace.
func (k *Kernel) RootMountNamespace() *fs.MountNamespace {
	k.extMu.Lock()
	defer k.extMu.Unlock()
	return k.mounts
}

// SetRootMountNamespace sets the MountNamespace.
func (k *Kernel) SetRootMountNamespace(mounts *fs.MountNamespace) {
	k.extMu.Lock()
	defer k.extMu.Unlock()
	k.mounts = mounts
}

// NetworkStack returns the network stack. NetworkStack may return nil if no
// network stack is available.
func (k *Kernel) NetworkStack() inet.Stack {
	return k.networkStack
}

// GlobalInit returns the thread group with ID 1 in the root PID namespace, or
// nil if no such thread group exists. GlobalInit may return a thread group
// containing no tasks if the thread group has already exited.
func (k *Kernel) GlobalInit() *ThreadGroup {
	k.extMu.Lock()
	defer k.extMu.Unlock()
	return k.globalInit
}

// ApplicationCores returns the number of CPUs visible to sandboxed
// applications.
func (k *Kernel) ApplicationCores() uint {
	return k.applicationCores
}

// RealtimeClock returns the application CLOCK_REALTIME clock.
func (k *Kernel) RealtimeClock() ktime.Clock {
	return k.realtimeClock
}

// MonotonicClock returns the application CLOCK_MONOTONIC clock.
func (k *Kernel) MonotonicClock() ktime.Clock {
	return k.monotonicClock
}

// CPUClockNow returns the current value of k.cpuClock.
func (k *Kernel) CPUClockNow() uint64 {
	return atomic.LoadUint64(&k.cpuClock)
}

// Syslog returns the syslog.
func (k *Kernel) Syslog() *syslog {
	return &k.syslog
}

// GenerateInotifyCookie generates a unique inotify event cookie.
//
// Returned values may overlap with previously returned values if the value
// space is exhausted. 0 is not a valid cookie value, all other values
// representable in a uint32 are allowed.
func (k *Kernel) GenerateInotifyCookie() uint32 {
	id := atomic.AddUint32(&k.nextInotifyCookie, 1)
	// Wrap-around is explicitly allowed for inotify event cookies.
	if id == 0 {
		id = atomic.AddUint32(&k.nextInotifyCookie, 1)
	}
	return id
}

// NetlinkPorts returns the netlink port manager.
func (k *Kernel) NetlinkPorts() *port.Manager {
	return k.netlinkPorts
}

// ExitError returns the sandbox error that caused the kernel to exit.
func (k *Kernel) ExitError() error {
	k.extMu.Lock()
	defer k.extMu.Unlock()
	return k.exitErr
}

// SetExitError sets the sandbox error that caused the kernel to exit, if one is
// not already set.
func (k *Kernel) SetExitError(err error) {
	k.extMu.Lock()
	defer k.extMu.Unlock()
	if k.exitErr == nil {
		k.exitErr = err
	}
}

// NowNanoseconds implements tcpip.Clock.NowNanoseconds.
func (k *Kernel) NowNanoseconds() int64 {
	now, err := k.timekeeper.GetTime(sentrytime.Realtime)
	if err != nil {
		panic("Kernel.NowNanoseconds: " + err.Error())
	}
	return now
}

// SupervisorContext returns a Context with maximum privileges in k. It should
// only be used by goroutines outside the control of the emulated kernel
// defined by e.
//
// Callers are responsible for ensuring that the returned Context is not used
// concurrently with changes to the Kernel.
func (k *Kernel) SupervisorContext() context.Context {
	return supervisorContext{
		Logger: log.Log(),
		k:      k,
	}
}

type supervisorContext struct {
	context.NoopSleeper
	log.Logger
	k *Kernel
}

// Value implements context.Context.
func (ctx supervisorContext) Value(key interface{}) interface{} {
	switch key {
	case CtxCanTrace:
		// The supervisor context can trace anything. (None of
		// supervisorContext's users are expected to invoke ptrace, but ptrace
		// permissions are required for certain file accesses.)
		return func(*Task, bool) bool { return true }
	case CtxKernel:
		return ctx.k
	case CtxPIDNamespace:
		return ctx.k.tasks.Root
	case CtxUTSNamespace:
		return ctx.k.rootUTSNamespace
	case CtxIPCNamespace:
		return ctx.k.rootIPCNamespace
	case auth.CtxCredentials:
		// The supervisor context is global root.
		return auth.NewRootCredentials(ctx.k.rootUserNamespace)
	case fs.CtxRoot:
		return ctx.k.mounts.Root()
	case ktime.CtxRealtimeClock:
		return ctx.k.RealtimeClock()
	case limits.CtxLimits:
		// No limits apply.
		return limits.NewLimitSet()
	case platform.CtxPlatform:
		return ctx.k
	case uniqueid.CtxGlobalUniqueID:
		return ctx.k.UniqueID()
	case uniqueid.CtxInotifyCookie:
		return ctx.k.GenerateInotifyCookie()
	default:
		return nil
	}
}

type kernelCPUClockListener struct {
	k *Kernel
}

// Notify implements ktime.TimerListener.Notify.
func (l kernelCPUClockListener) Notify(exp uint64) {
	// Only increment cpuClock by 1 regardless of the number of expirations.
	// This approximately compensates for cases where thread throttling or bad
	// Go runtime scheduling prevents the cpuClockTicker goroutine, and
	// presumably task goroutines as well, from executing for a long period of
	// time. It's also necessary to prevent CPU clocks from seeing large
	// discontinuous jumps.
	atomic.AddUint64(&l.k.cpuClock, 1)
}

// Destroy implements ktime.TimerListener.Destroy.
func (l kernelCPUClockListener) Destroy() {
}
