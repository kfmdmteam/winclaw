//go:build windows

package security

import (
	"fmt"
	"unsafe"

	"github.com/google/uuid"
	"golang.org/x/sys/windows"
)

// Windows job object information class identifiers.
const (
	jobObjectExtendedLimitInformation  = 9
	jobObjectCPURateControlInformation = 15
)

// Limit flags for JOBOBJECT_BASIC_LIMIT_INFORMATION.LimitFlags.
const (
	jobLimitJobMemory   = 0x00000200 // JOB_OBJECT_LIMIT_JOB_MEMORY
	jobLimitKillOnClose = 0x00002000 // JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
)

// CPU rate control flags for JOBOBJECT_CPU_RATE_CONTROL_INFORMATION.ControlFlags.
const (
	cpuRateControlEnable = 0x1 // JOB_OBJECT_CPU_RATE_CONTROL_ENABLE
	cpuRateControlHard   = 0x4 // JOB_OBJECT_CPU_RATE_CONTROL_HARD_CAP
)

// ioCounters mirrors the Windows IO_COUNTERS struct embedded in
// JOBOBJECT_EXTENDED_LIMIT_INFORMATION. All fields are present to keep the
// struct layout ABI-correct on both 32-bit and 64-bit Windows.
type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

// basicLimitInfo mirrors JOBOBJECT_BASIC_LIMIT_INFORMATION.
// Field sizes and alignment match the Windows ABI on amd64.
type basicLimitInfo struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	_                       uint32 // padding on amd64 before pointer-sized fields
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	_                       uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

// extendedLimitInfo mirrors JOBOBJECT_EXTENDED_LIMIT_INFORMATION.
type extendedLimitInfo struct {
	BasicLimitInformation basicLimitInfo
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

// cpuRateControlInfo mirrors JOBOBJECT_CPU_RATE_CONTROL_INFORMATION.
// The second field is a union; we treat it as CpuRate (a uint32 percentage
// expressed in hundredths of a percent, i.e. 1–10000).
type cpuRateControlInfo struct {
	ControlFlags uint32
	CpuRate      uint32 // first member of the anonymous union
}

var (
	modkernel32                  = windows.NewLazySystemDLL("kernel32.dll")
	procCreateJobObjectW         = modkernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject  = modkernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject = modkernel32.NewProc("AssignProcessToJobObject")
)

// JobObject wraps a Windows job object handle.
type JobObject struct {
	handle windows.Handle
	name   string
}

// NewJobObject creates a new named Windows job object. The effective name is
// always "WinClaw-<uuid>" (when name is empty) or "WinClaw-<name>" to prevent
// collisions between concurrent instances. KillOnJobClose is applied
// immediately so that all child processes are terminated when Close is called.
func NewJobObject(name string) (*JobObject, error) {
	jobName := "WinClaw-" + uuid.New().String()
	if name != "" {
		jobName = "WinClaw-" + name
	}

	namePtr, err := windows.UTF16PtrFromString(jobName)
	if err != nil {
		return nil, fmt.Errorf("jobobj: encode name: %w", err)
	}

	h, _, e := procCreateJobObjectW.Call(0, uintptr(unsafe.Pointer(namePtr)))
	if h == 0 {
		return nil, fmt.Errorf("jobobj: CreateJobObjectW: %w", e)
	}

	jo := &JobObject{handle: windows.Handle(h), name: jobName}

	if err := jo.setKillOnClose(); err != nil {
		_ = jo.Close()
		return nil, err
	}

	return jo, nil
}

// setKillOnClose applies JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE with no other
// limits, ensuring child processes are killed when the job handle is closed.
func (jo *JobObject) setKillOnClose() error {
	info := extendedLimitInfo{}
	info.BasicLimitInformation.LimitFlags = jobLimitKillOnClose

	r, _, e := procSetInformationJobObject.Call(
		uintptr(jo.handle),
		jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uintptr(unsafe.Sizeof(info)),
	)
	if r == 0 {
		return fmt.Errorf("jobobj: SetInformationJobObject (kill-on-close): %w", e)
	}
	return nil
}

// AssignProcess assigns the process identified by handle to this job object.
// Once assigned the process is subject to all limits configured on the job.
func (jo *JobObject) AssignProcess(handle windows.Handle) error {
	r, _, e := procAssignProcessToJobObject.Call(
		uintptr(jo.handle),
		uintptr(handle),
	)
	if r == 0 {
		return fmt.Errorf("jobobj: AssignProcessToJobObject: %w", e)
	}
	return nil
}

// SetLimits configures memory and CPU limits for the job object.
//   - maxMemoryMB: maximum committed memory for all processes in the job
//     combined, in mebibytes. Pass 0 to leave the memory limit unchanged.
//   - cpuRatePct: hard CPU cap as a percentage 1–100. Pass 0 to leave the
//     CPU limit unchanged.
func (jo *JobObject) SetLimits(maxMemoryMB uint64, cpuRatePct uint32) error {
	if maxMemoryMB > 0 {
		info := extendedLimitInfo{}
		info.BasicLimitInformation.LimitFlags = jobLimitKillOnClose | jobLimitJobMemory
		info.JobMemoryLimit = uintptr(maxMemoryMB * 1024 * 1024)

		r, _, e := procSetInformationJobObject.Call(
			uintptr(jo.handle),
			jobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)),
			uintptr(unsafe.Sizeof(info)),
		)
		if r == 0 {
			return fmt.Errorf("jobobj: SetInformationJobObject (memory): %w", e)
		}
	}

	if cpuRatePct > 0 {
		if cpuRatePct > 100 {
			cpuRatePct = 100
		}
		// Windows expresses the rate in hundredths of a percent (range 1–10000).
		info := cpuRateControlInfo{
			ControlFlags: cpuRateControlEnable | cpuRateControlHard,
			CpuRate:      cpuRatePct * 100,
		}

		r, _, e := procSetInformationJobObject.Call(
			uintptr(jo.handle),
			jobObjectCPURateControlInformation,
			uintptr(unsafe.Pointer(&info)),
			uintptr(unsafe.Sizeof(info)),
		)
		if r == 0 {
			return fmt.Errorf("jobobj: SetInformationJobObject (cpu): %w", e)
		}
	}

	return nil
}

// Close releases the job object handle. Because KillOnJobClose was set at
// creation time, all child processes still running in the job are terminated.
// Subsequent calls to Close are no-ops.
func (jo *JobObject) Close() error {
	if jo.handle == 0 {
		return nil
	}
	if err := windows.CloseHandle(jo.handle); err != nil {
		return fmt.Errorf("jobobj: CloseHandle: %w", err)
	}
	jo.handle = 0
	return nil
}
