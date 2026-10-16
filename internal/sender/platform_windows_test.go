//go:build windows

package sender

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func TestJobChildHelper(t *testing.T) {
	marker := os.Getenv("SENDER_JOB_CHILD")
	if marker == "" {
		return
	}
	current, err := syscall.GetCurrentProcess()
	if err != nil {
		t.Fatal(err)
	}
	var owned int32
	ok, _, err := kernel32.NewProc("IsProcessInJob").Call(uintptr(current), 0, uintptr(unsafe.Pointer(&owned)))
	if ok == 0 || owned == 0 {
		t.Fatalf("child executed outside job: %v", err)
	}
	if err := os.WriteFile(marker, []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Minute)
}

func jobTestChild(marker string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestJobChildHelper$")
	cmd.Env = append(os.Environ(), "SENDER_JOB_CHILD="+marker)
	return cmd
}

func waitJobMarker(t *testing.T, marker string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(marker); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("owned child did not execute")
	return 0
}

func TestProcessGroupOwnsBeforeExecutionAndKillsOnClose(t *testing.T) {
	group, err := newProcessGroup()
	if err != nil {
		t.Fatal(err)
	}
	defer group.close()
	marker := filepath.Join(t.TempDir(), "started")
	cmd := jobTestChild(marker)
	if err := group.start(cmd); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	waitJobMarker(t, marker)
	group.close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done: // Job termination may use exit code zero.
	case <-time.After(5 * time.Second):
		t.Fatal("job close left child alive")
	}
}

func TestProcessGroupAssignmentFailureNeverExecutesChild(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	cmd := jobTestChild(marker)
	group := &processGroup{} // Invalid job forces assignment failure after suspended creation.
	if err := group.start(cmd); err == nil {
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatal("accepted invalid job")
	}
	if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
		t.Fatal("failed child was not reaped")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("unassigned child executed: %v", err)
	}
}

func TestJobOwnerHelper(t *testing.T) {
	marker := os.Getenv("SENDER_JOB_OWNER")
	if marker == "" {
		return
	}
	group, err := newProcessGroup()
	if err != nil {
		t.Fatal(err)
	}
	defer group.close()
	if err := group.start(jobTestChild(marker)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Minute)
}

func TestProcessGroupKillsOnAbruptOwnerDeath(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	owner := exec.Command(os.Args[0], "-test.run=^TestJobOwnerHelper$")
	owner.Env = append(os.Environ(), "SENDER_JOB_OWNER="+marker)
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	defer owner.Process.Kill()
	pid := waitJobMarker(t, marker)
	h, _, err := openProcess.Call(0x00100000, 0, uintptr(pid)) // SYNCHRONIZE
	if h == 0 {
		t.Fatal(fmt.Errorf("open owned child: %w", err))
	}
	defer syscall.CloseHandle(syscall.Handle(h))
	if err := owner.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	owner.Wait()
	status, err := syscall.WaitForSingleObject(syscall.Handle(h), 5000)
	if err != nil || status != syscall.WAIT_OBJECT_0 {
		t.Fatalf("owner death left child alive: %d %v", status, err)
	}
}
