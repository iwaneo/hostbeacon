package helper

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/iwaneo/hostbeacon/agent/internal/config"
)

var (
	refreshOn  = config.Config{PackageListRefresh: true}
	refreshNow = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
)

type testRefresh struct {
	PackageListRefresh
	commands []string
	fail     error
}

// newRefresh makes a PackageListRefresh on temporary files, for a Host whose
// package list was never refreshed by Hostbeacon.
func newRefresh(t *testing.T, manager string) *testRefresh {
	t.Helper()
	locks, state := t.TempDir(), t.TempDir()
	r := &testRefresh{}
	r.PackageListRefresh = PackageListRefresh{
		Manager:                manager,
		Stamp:                  filepath.Join(state, "package-list-refreshed"),
		PackageTaskLock:        filepath.Join(locks, "package-task.lock"),
		PackageManagerLocks:    []string{filepath.Join(locks, "lock-frontend")},
		PackageManagerPIDLocks: []string{filepath.Join(locks, "metadata_lock.pid")},
		Now:                    func() time.Time { return refreshNow },
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			r.commands = append(r.commands, strings.Join(append([]string{name}, args...), " "))
			return nil, r.fail
		},
	}
	return r
}

func (r *testRefresh) stamp(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(r.Stamp)
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func wantResult(t *testing.T, got RefreshResult, err error, want RefreshResult) {
	t.Helper()
	if err != nil || got != want {
		t.Fatalf("result %q, error %v, want %q", got, err, want)
	}
}

func TestRefreshRunsTheListRefreshAndStoresItsTime(t *testing.T) {
	for manager, command := range map[string]string{
		"apt": "apt-get update -q -o APT::Update::Error-Mode=any",
		"dnf": "dnf makecache --refresh -q",
	} {
		t.Run(manager, func(t *testing.T) {
			r := newRefresh(t, manager)
			result, err := r.Refresh(context.Background(), refreshOn)
			wantResult(t, result, err, RefreshDone)
			if !slices.Equal(r.commands, []string{command}) {
				t.Errorf("commands %q, want %q", r.commands, command)
			}
			if got := r.stamp(t); got != "2026-09-29T12:00:00Z\n" {
				t.Errorf("stamp %q", got)
			}
			if last, ok := ReadPackageListStamp(r.Stamp); !ok || !last.Equal(refreshNow) {
				t.Errorf("read stamp %v %v", last, ok)
			}
			if info, _ := os.Stat(r.Stamp); info.Mode().Perm() != 0o644 {
				t.Errorf("stamp mode %v; the network part must read it", info.Mode())
			}
		})
	}
}

func TestRefreshRunsOnlyWhenTheListIsOlderThan24Hours(t *testing.T) {
	r := newRefresh(t, "apt")
	for _, test := range []struct {
		age  time.Duration
		want RefreshResult
	}{
		{time.Hour, RefreshFresh},
		{24*time.Hour - time.Second, RefreshFresh},
		{24 * time.Hour, RefreshDone},
		{30 * 24 * time.Hour, RefreshDone},
		// A stamp from the future (the clock went back) is not trusted.
		{-time.Hour, RefreshDone},
	} {
		os.WriteFile(r.Stamp, []byte(refreshNow.Add(-test.age).Format(time.RFC3339)+"\n"), 0o644)
		r.commands = nil
		result, err := r.Refresh(context.Background(), refreshOn)
		wantResult(t, result, err, test.want)
		if ran := len(r.commands) > 0; ran != (test.want == RefreshDone) {
			t.Errorf("age %v: commands %q", test.age, r.commands)
		}
	}
}

func TestRefreshRunsWhenTheStampCannotBeRead(t *testing.T) {
	r := newRefresh(t, "dnf")
	os.WriteFile(r.Stamp, []byte("yesterday\n"), 0o644)
	result, err := r.Refresh(context.Background(), refreshOn)
	wantResult(t, result, err, RefreshDone)
}

func TestRefreshIsOffWhenTurnedOffInTheHostConfig(t *testing.T) {
	r := newRefresh(t, "apt")
	result, err := r.Refresh(context.Background(), config.Config{PackageListRefresh: false})
	wantResult(t, result, err, RefreshOff)
	if len(r.commands) > 0 || r.stamp(t) != "" {
		t.Errorf("commands %q, stamp %q", r.commands, r.stamp(t))
	}
}

func TestRefreshIsNotSupportedWithoutAptOrDnf(t *testing.T) {
	r := newRefresh(t, "")
	result, err := r.Refresh(context.Background(), refreshOn)
	wantResult(t, result, err, RefreshNotSupported)
	if len(r.commands) > 0 {
		t.Errorf("commands %q", r.commands)
	}
}

func TestRefreshSkipsWhileThePackageTaskLockIsHeld(t *testing.T) {
	r := newRefresh(t, "apt")
	holder, err := os.OpenFile(r.PackageTaskLock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}

	result, err := r.Refresh(context.Background(), refreshOn)
	wantResult(t, result, err, RefreshBusy)
	if len(r.commands) > 0 || r.stamp(t) != "" {
		t.Errorf("commands %q, stamp %q", r.commands, r.stamp(t))
	}

	syscall.Flock(int(holder.Fd()), syscall.LOCK_UN)
	result, err = r.Refresh(context.Background(), refreshOn)
	wantResult(t, result, err, RefreshDone)
}

func TestRefreshSkipsWhileThePackageManagerLockIsHeld(t *testing.T) {
	r := newRefresh(t, "apt")
	holdPackageManagerLock(t, r.PackageManagerLocks[0])
	result, err := r.Refresh(context.Background(), refreshOn)
	wantResult(t, result, err, RefreshBusy)
	if len(r.commands) > 0 {
		t.Errorf("commands %q", r.commands)
	}
}

func TestRefreshSkipsWhileADnf4LockNamesARunningProcess(t *testing.T) {
	r := newRefresh(t, "dnf")
	os.WriteFile(r.PackageManagerPIDLocks[0], []byte("1\n"), 0o644)
	result, err := r.Refresh(context.Background(), refreshOn)
	wantResult(t, result, err, RefreshBusy)
}

func TestRefreshHoldsThePackageTaskLockWhileItRuns(t *testing.T) {
	r := newRefresh(t, "apt")
	var heldWhileRunning bool
	r.Run = func(context.Context, string, ...string) ([]byte, error) {
		release, err := lockPackageTask(r.PackageTaskLock)
		if err == nil {
			release()
		}
		heldWhileRunning = err != nil
		return nil, nil
	}
	result, err := r.Refresh(context.Background(), refreshOn)
	wantResult(t, result, err, RefreshDone)
	if !heldWhileRunning {
		t.Error("the package-task lock was free during the refresh")
	}
	release, err := lockPackageTask(r.PackageTaskLock)
	if err != nil {
		t.Fatalf("the lock is still held after the refresh: %v", err)
	}
	release()
}

func TestFailedRefreshKeepsTheOldStamp(t *testing.T) {
	r := newRefresh(t, "apt")
	old := "2026-09-01T00:00:00Z\n"
	os.WriteFile(r.Stamp, []byte(old), 0o644)
	r.fail = errors.New("exit status 100")
	if _, err := r.Refresh(context.Background(), refreshOn); err == nil {
		t.Fatal("no error for a failed refresh")
	}
	if got := r.stamp(t); got != old {
		t.Errorf("stamp %q, want the old one", got)
	}
}

func TestDetectPackageManager(t *testing.T) {
	root := t.TempDir()
	if got := DetectPackageManager(root); got != "" {
		t.Errorf("empty Host: %q, want none", got)
	}
	os.MkdirAll(filepath.Join(root, "usr", "bin"), 0o755)
	os.WriteFile(filepath.Join(root, "usr", "bin", "dnf"), nil, 0o755)
	if got := DetectPackageManager(root); got != "dnf" {
		t.Errorf("got %q, want dnf", got)
	}
	os.WriteFile(filepath.Join(root, "usr", "bin", "apt-get"), nil, 0o755)
	if got := DetectPackageManager(root); got != "apt" {
		t.Errorf("got %q, want apt", got)
	}
}

func TestRefreshFailsWhenThePackageTaskLockCannotBeOpened(t *testing.T) {
	r := newRefresh(t, "apt")
	// A directory where the lock file should be: opening it fails, and that
	// is not a held lock.
	os.Mkdir(r.PackageTaskLock, 0o755)
	result, err := r.Refresh(context.Background(), refreshOn)
	if err == nil || result == RefreshBusy {
		t.Errorf("result %q, error %v; want an error", result, err)
	}
}
