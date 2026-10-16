//go:build windows

package sender

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"unsafe"
)

var (
	user32          = syscall.NewLazyDLL("user32.dll")
	kernel32        = syscall.NewLazyDLL("kernel32.dll")
	enumMonitors    = user32.NewProc("EnumDisplayMonitors")
	getMonitorInfo  = user32.NewProc("GetMonitorInfoW")
	enumWindows     = user32.NewProc("EnumWindows")
	isVisible       = user32.NewProc("IsWindowVisible")
	isIconic        = user32.NewProc("IsIconic")
	getWindowText   = user32.NewProc("GetWindowTextW")
	getWindowPID    = user32.NewProc("GetWindowThreadProcessId")
	createJob       = kernel32.NewProc("CreateJobObjectW")
	setJobInfo      = kernel32.NewProc("SetInformationJobObject")
	assignJob       = kernel32.NewProc("AssignProcessToJobObject")
	openProcess     = kernel32.NewProc("OpenProcess")
	resumeProcess   = syscall.NewLazyDLL("ntdll.dll").NewProc("NtResumeProcess")
	enumerationMu   sync.Mutex
	nativeSources   []Source
	monitorCallback = syscall.NewCallback(collectMonitor)
	windowCallback  = syscall.NewCallback(collectWindow)
)

func platformSupported() error {
	if runtime.GOARCH != "amd64" {
		return errors.New("sender bundle requires Windows x64 (amd64)")
	}
	// Native monitor rectangles must use physical pixels, not DPI-virtualized units.
	user32.NewProc("SetProcessDpiAwarenessContext").Call(^uintptr(3)) // PER_MONITOR_AWARE_V2 (-4)
	return nil
}

type rect struct{ Left, Top, Right, Bottom int32 }
type monitorInfo struct {
	Size          uint32
	Monitor, Work rect
	Flags         uint32
	Device        [32]uint16
}

// Allocate callbacks once: Windows syscall callback slots cannot be freed.
func collectMonitor(monitor, dc, rectangle, data uintptr) uintptr {
	info := monitorInfo{Size: uint32(unsafe.Sizeof(monitorInfo{}))}
	if ok, _, _ := getMonitorInfo.Call(monitor, uintptr(unsafe.Pointer(&info))); ok != 0 {
		r := info.Monitor
		nativeSources = append(nativeSources, Source{ID: "monitor:" + strconv.FormatUint(uint64(monitor), 10), Name: fmt.Sprintf("Monitor %s (%d × %d)", syscall.UTF16ToString(info.Device[:]), r.Right-r.Left, r.Bottom-r.Top), Kind: "monitor", X: int(r.Left), Y: int(r.Top), Width: int(r.Right - r.Left), Height: int(r.Bottom - r.Top)})
	}
	return 1
}

func collectWindow(hwnd, data uintptr) uintptr {
	visible, _, _ := isVisible.Call(hwnd)
	minimized, _, _ := isIconic.Call(hwnd)
	if visible == 0 || minimized != 0 {
		return 1
	}
	var title [512]uint16
	length, _, _ := getWindowText.Call(hwnd, uintptr(unsafe.Pointer(&title[0])), uintptr(len(title)))
	if length == 0 {
		return 1
	}
	var pid uint32
	getWindowPID.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	if pid != 0 {
		nativeSources = append(nativeSources, Source{ID: "window:" + strconv.FormatUint(uint64(hwnd), 10), Name: syscall.UTF16ToString(title[:]), Kind: "window", HWND: hwnd, PID: pid})
	}
	return 1
}

func enumerateSources() ([]Source, error) {
	enumerationMu.Lock()
	defer enumerationMu.Unlock()
	nativeSources = nil
	defer func() { nativeSources = nil }()
	if ok, _, err := enumMonitors.Call(0, 0, monitorCallback, 0); ok == 0 {
		return nil, fmt.Errorf("enumerate monitors: %w", err)
	}
	if ok, _, err := enumWindows.Call(windowCallback, 0); ok == 0 {
		return nil, fmt.Errorf("enumerate windows: %w", err)
	}
	return nativeSources, nil
}

func validateCurrentSource(s Source) error {
	if err := validateSource(s); err != nil {
		return err
	}
	sources, err := enumerateSources()
	if err != nil {
		return err
	}
	for _, current := range sources {
		if current.ID == s.ID && current.PID == s.PID && current.X == s.X && current.Y == s.Y && current.Width == s.Width && current.Height == s.Height {
			return nil
		}
	}
	return errors.New("capture source changed or closed; refresh and select again")
}

func openBrowser(url string) error {
	// Fixed executable and direct argv; URL contains only generated loopback capability.
	cmd := exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", url)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Run()
}

type processGroup struct{ handle syscall.Handle }

func newProcessGroup() (*processGroup, error) {
	h, _, err := createJob.Call(0, 0)
	if h == 0 {
		return nil, err
	}
	g := &processGroup{handle: syscall.Handle(h)}
	// JOBOBJECT_EXTENDED_LIMIT_INFORMATION, Windows amd64/arm64 layout.
	// BasicLimitInformation.LimitFlags at byte 16; full struct is 144 bytes.
	var info [144]byte
	*(*uint32)(unsafe.Pointer(&info[16])) = 0x2000 // JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if ok, _, err := setJobInfo.Call(h, 9, uintptr(unsafe.Pointer(&info[0])), uintptr(len(info))); ok == 0 {
		g.close()
		return nil, err
	}
	return g, nil
}

func (g *processGroup) start(cmd *exec.Cmd) error {
	// shortcut: death before job assignment can leak an inert suspended process;
	// use atomic PROC_THREAD_ATTRIBUTE_JOB_LIST when os/exec exposes it.
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000 | 0x00000004} // CREATE_NO_WINDOW | CREATE_SUSPENDED
	if err := cmd.Start(); err != nil {
		return err
	}
	h, _, err := openProcess.Call(0x0100|0x0001|0x0800, 0, uintptr(cmd.Process.Pid)) // SET_QUOTA | TERMINATE | SUSPEND_RESUME
	if h == 0 {
		cmd.Process.Kill()
		cmd.Wait()
		return err
	}
	defer syscall.CloseHandle(syscall.Handle(h))
	if ok, _, err := assignJob.Call(uintptr(g.handle), h); ok == 0 {
		cmd.Process.Kill()
		cmd.Wait()
		return fmt.Errorf("assign capture to cleanup job: %w", err)
	}
	// Only owned children may execute capture code. NtResumeProcess returns NTSTATUS.
	if status, _, _ := resumeProcess.Call(h); int32(status) < 0 {
		cmd.Process.Kill()
		cmd.Wait()
		return fmt.Errorf("resume capture: NTSTATUS %#x", uint32(status))
	}
	return nil
}

func (g *processGroup) close() {
	if g.handle != 0 {
		syscall.CloseHandle(g.handle)
		g.handle = 0
	}
}
