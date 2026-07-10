// Copyright (c) 2023-2026, Nubificus LTD
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

package unikontainers

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"golang.org/x/sys/unix"
)

type mountCall struct {
	source string
	target string
	fstype string
	flags  uintptr
	data   string
}

func TestCreateTmpfs(t *testing.T) {
	origMount := mountSyscall
	origMkdirAll := mkdirAllHook
	origOsChmod := osChmodHook
	t.Cleanup(func() {
		mountSyscall = origMount
		mkdirAllHook = origMkdirAll
		osChmodHook = origOsChmod
	})

	tmpDir := "/tmp/mock-root"

	t.Run("successful mount with mode 1777", func(t *testing.T) {
		var calls []mountCall
		var chmodCalled bool
		var chmodMode os.FileMode

		mkdirAllHook = func(path string, perm os.FileMode) error {
			return nil
		}

		mountSyscall = func(source string, target string, fstype string, flags uintptr, data string) error {
			calls = append(calls, mountCall{source, target, fstype, flags, data})
			return nil
		}

		osChmodHook = func(name string, mode os.FileMode) error {
			chmodCalled = true
			chmodMode = mode
			return nil
		}

		err := createTmpfs(tmpDir, "tmp", unix.MS_NOSUID, "1777", "64m")
		assert.NoError(t, err)
		assert.Len(t, calls, 2)

		// Check first mount (mount tmpfs)
		assert.Equal(t, "tmpfs", calls[0].source)
		assert.Equal(t, filepath.Join(tmpDir, "tmp"), calls[0].target)
		assert.Equal(t, uintptr(unix.MS_NOSUID), calls[0].flags)
		assert.Equal(t, "mode=1777,size=64m", calls[0].data)

		// Check second mount (remount private)
		assert.Equal(t, "", calls[1].source)
		assert.Equal(t, uintptr(unix.MS_PRIVATE), calls[1].flags)

		// Check os.Chmod was called
		assert.True(t, chmodCalled)
		assert.Equal(t, os.FileMode(01777), chmodMode)
	})

	t.Run("successful mount without mode 1777", func(t *testing.T) {
		var chmodCalled bool

		mkdirAllHook = func(path string, perm os.FileMode) error {
			return nil
		}

		mountSyscall = func(source string, target string, fstype string, flags uintptr, data string) error {
			return nil
		}

		osChmodHook = func(name string, mode os.FileMode) error {
			chmodCalled = true
			return nil
		}

		err := createTmpfs(tmpDir, "tmp", unix.MS_NOSUID, "0755", "64m")
		assert.NoError(t, err)
		assert.False(t, chmodCalled)
	})

	t.Run("mkdirAll failure", func(t *testing.T) {
		mkdirAllHook = func(path string, perm os.FileMode) error {
			return errors.New("mkdir failed")
		}

		err := createTmpfs(tmpDir, "tmp", unix.MS_NOSUID, "1777", "64m")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "mkdir failed")
	})

	t.Run("mount failure", func(t *testing.T) {
		mkdirAllHook = func(path string, perm os.FileMode) error {
			return nil
		}
		mountSyscall = func(source string, target string, fstype string, flags uintptr, data string) error {
			return errors.New("mount failed")
		}

		err := createTmpfs(tmpDir, "tmp", unix.MS_NOSUID, "1777", "64m")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "mount failed")
	})
}

func TestSetupDev(t *testing.T) {
	origMknod := mknodSyscall
	origStat := statSyscall
	origChmod := chmodSyscall
	origChown := chownSyscall
	origMkdirAll := mkdirAllHook
	origUserNS := runningInUserNSHook
	t.Cleanup(func() {
		mknodSyscall = origMknod
		statSyscall = origStat
		chmodSyscall = origChmod
		chownSyscall = origChown
		mkdirAllHook = origMkdirAll
		runningInUserNSHook = origUserNS
	})

	tmpDir := "/tmp/mock-root"

	t.Run("user-namespace branch", func(t *testing.T) {
		runningInUserNSHook = func() bool {
			return true
		}

		var fileFromHostCalled bool
		statSyscall = func(path string, stat *unix.Stat_t) error {
			stat.Mode = unix.S_IFREG | 0o644
			return nil
		}

		// Mock fileFromHost via statSyscall and mountSyscall
		origMount := mountSyscall
		defer func() { mountSyscall = origMount }()

		mountSyscall = func(source string, target string, fstype string, flags uintptr, data string) error {
			fileFromHostCalled = true
			assert.Equal(t, uintptr(unix.MS_BIND), flags)
			return nil
		}

		err := setupDev(tmpDir, "/dev/null")
		assert.NoError(t, err)
		assert.True(t, fileFromHostCalled)
	})

	t.Run("non-user-namespace happy path", func(t *testing.T) {
		runningInUserNSHook = func() bool {
			return false
		}

		statSyscall = func(path string, stat *unix.Stat_t) error {
			stat.Mode = unix.S_IFCHR | 0o600
			stat.Rdev = unix.Mkdev(1, 3)
			return nil
		}

		mknodSyscall = func(path string, mode uint32, dev int) error {
			assert.Equal(t, filepath.Join(tmpDir, "dev/null"), path)
			assert.Equal(t, uint32(unix.S_IFCHR|0o600), mode)
			return nil
		}

		chmodSyscall = func(path string, mode uint32) error {
			assert.Equal(t, uint32(0o606), mode) // 0o600 | 0o006
			return nil
		}

		chownSyscall = func(path string, uid int, gid int) error {
			return nil
		}

		err := setupDev(tmpDir, "/dev/null")
		assert.NoError(t, err)
	})

	t.Run("statSyscall failure", func(t *testing.T) {
		runningInUserNSHook = func() bool { return false }
		statSyscall = func(path string, stat *unix.Stat_t) error {
			return errors.New("stat failed")
		}

		err := setupDev(tmpDir, "/dev/null")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "stat failed")
	})

	t.Run("not a device node error", func(t *testing.T) {
		runningInUserNSHook = func() bool { return false }
		statSyscall = func(path string, stat *unix.Stat_t) error {
			stat.Mode = unix.S_IFREG | 0o644
			return nil
		}

		err := setupDev(tmpDir, "/dev/regular")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "is not a device node")
	})

	t.Run("mkdirAll failure for deep device path", func(t *testing.T) {
		runningInUserNSHook = func() bool { return false }
		statSyscall = func(path string, stat *unix.Stat_t) error {
			stat.Mode = unix.S_IFCHR | 0o600
			return nil
		}
		mkdirAllHook = func(path string, perm os.FileMode) error {
			return errors.New("mkdirall failed")
		}

		err := setupDev(tmpDir, "/dev/net/tun")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "mkdirall failed")
	})

	t.Run("mknodSyscall failure", func(t *testing.T) {
		runningInUserNSHook = func() bool { return false }
		statSyscall = func(path string, stat *unix.Stat_t) error {
			stat.Mode = unix.S_IFCHR | 0o600
			return nil
		}
		mknodSyscall = func(path string, mode uint32, dev int) error {
			return errors.New("mknod failed")
		}

		err := setupDev(tmpDir, "/dev/null")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "mknod failed")
	})

	t.Run("chmodSyscall failure", func(t *testing.T) {
		runningInUserNSHook = func() bool { return false }
		statSyscall = func(path string, stat *unix.Stat_t) error {
			stat.Mode = unix.S_IFCHR | 0o600
			return nil
		}
		mknodSyscall = func(path string, mode uint32, dev int) error {
			return nil
		}
		chmodSyscall = func(path string, mode uint32) error {
			return errors.New("chmod failed")
		}

		err := setupDev(tmpDir, "/dev/null")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "chmod failed")
	})

	t.Run("chownSyscall failure", func(t *testing.T) {
		runningInUserNSHook = func() bool { return false }
		statSyscall = func(path string, stat *unix.Stat_t) error {
			stat.Mode = unix.S_IFCHR | 0o600
			return nil
		}
		mknodSyscall = func(path string, mode uint32, dev int) error {
			return nil
		}
		chmodSyscall = func(path string, mode uint32) error {
			return nil
		}
		chownSyscall = func(path string, uid, gid int) error {
			return errors.New("chown failed")
		}

		err := setupDev(tmpDir, "/dev/null")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "chown failed")
	})
}

func TestFileFromHost(t *testing.T) {
	origStat := statSyscall
	origMount := mountSyscall
	origChmod := chmodSyscall
	origOpen := openSyscall
	origClose := closeSyscall
	origChown := chownSyscall
	t.Cleanup(func() {
		statSyscall = origStat
		mountSyscall = origMount
		chmodSyscall = origChmod
		openSyscall = origOpen
		closeSyscall = origClose
		chownSyscall = origChown
	})

	tmpDir := t.TempDir()

	t.Run("bind mount directory", func(t *testing.T) {
		var mountSource, mountTarget string
		var mountFlags uintptr

		statSyscall = func(path string, stat *unix.Stat_t) error {
			stat.Mode = unix.S_IFDIR | 0o755
			return nil
		}

		mountSyscall = func(source string, target string, fstype string, flags uintptr, data string) error {
			mountSource = source
			mountTarget = target
			mountFlags = flags
			return nil
		}

		err := fileFromHost(tmpDir, "/etc/config", "", unix.MS_BIND, false)
		assert.NoError(t, err)
		assert.Equal(t, "/etc/config", mountSource)
		assert.Equal(t, filepath.Join(tmpDir, "etc/config"), mountTarget)
		assert.Equal(t, uintptr(unix.MS_BIND), mountFlags)
	})

	t.Run("withCopy is true", func(t *testing.T) {
		srcFile, err := os.CreateTemp(tmpDir, "src")
		assert.NoError(t, err)
		defer srcFile.Close()

		_, err = srcFile.WriteString("hello copy")
		assert.NoError(t, err)

		var chmodCalled, chownCalled bool

		statSyscall = func(path string, stat *unix.Stat_t) error {
			stat.Mode = unix.S_IFREG | 0o644
			return nil
		}
		chmodSyscall = func(path string, mode uint32) error {
			chmodCalled = true
			return nil
		}
		chownSyscall = func(path string, uid, gid int) error {
			chownCalled = true
			return nil
		}

		dstPath := filepath.Join(tmpDir, "dst")
		err = fileFromHost(tmpDir, srcFile.Name(), "dst", 0, true)
		assert.NoError(t, err)

		// Assert file actually copied
		content, err := os.ReadFile(dstPath)
		assert.NoError(t, err)
		assert.Equal(t, "hello copy", string(content))

		assert.True(t, chmodCalled)
		assert.True(t, chownCalled)
	})

	t.Run("remount with flags (e.g. MS_NOSUID)", func(t *testing.T) {
		var mountCalls []mountCall

		statSyscall = func(path string, stat *unix.Stat_t) error {
			stat.Mode = unix.S_IFDIR | 0o755
			return nil
		}

		mountSyscall = func(source string, target string, fstype string, flags uintptr, data string) error {
			mountCalls = append(mountCalls, mountCall{source, target, fstype, flags, data})
			return nil
		}

		// MS_BIND | MS_NOSUID
		err := fileFromHost(tmpDir, "/etc/config", "", unix.MS_BIND|unix.MS_NOSUID, false)
		assert.NoError(t, err)

		// Should make two mount calls: first MS_BIND, second MS_BIND | MS_NOSUID | MS_REMOUNT
		assert.Len(t, mountCalls, 2)
		assert.Equal(t, uintptr(unix.MS_BIND|unix.MS_NOSUID), mountCalls[0].flags)
		assert.Equal(t, uintptr(unix.MS_BIND|unix.MS_NOSUID|unix.MS_REMOUNT), mountCalls[1].flags)
	})
}

func TestMapMountFlag(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		flagStr       string
		expectedFlag  int
		expectedClear bool
		expectedExist bool
	}{
		{"ro", "ro", unix.MS_RDONLY, false, true},
		{"rw", "rw", unix.MS_RDONLY, true, true},
		{"defaults", "defaults", 0, false, true},
		{"noexec", "noexec", unix.MS_NOEXEC, false, true},
		{"exec", "exec", unix.MS_NOEXEC, true, true},
		{"invalid flag", "invalid-flag", 0, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, exist := mapMountFlag(tt.flagStr)
			assert.Equal(t, tt.expectedExist, exist)
			if exist {
				assert.Equal(t, tt.expectedFlag, f.flag)
				assert.Equal(t, tt.expectedClear, f.clear)
			}
		})
	}
}

func TestMapRootfsPropagationFlag(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		val         string
		expected    int
		expectedErr bool
	}{
		{"shared", "shared", unix.MS_SHARED, false},
		{"private", "private", unix.MS_PRIVATE, false},
		{"rslave", "rslave", unix.MS_SLAVE | unix.MS_REC, false},
		{"invalid", "invalid", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := mapRootfsPropagationFlag(tt.val)
			if tt.expectedErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expected, f)
			}
		})
	}
}

func TestPrepareRoot(t *testing.T) {
	origMount := mountSyscall
	t.Cleanup(func() { mountSyscall = origMount })

	t.Run("success", func(t *testing.T) {
		var calls []mountCall
		mountSyscall = func(source string, target string, fstype string, flags uintptr, data string) error {
			calls = append(calls, mountCall{source, target, fstype, flags, data})
			if target == "/run" {
				return nil
			}
			return nil
		}

		err := prepareRoot("/run/container/rootfs", "shared")
		assert.NoError(t, err)
		assert.GreaterOrEqual(t, len(calls), 3)

		// First: remount / with mapped propagation
		assert.Equal(t, "/", calls[0].target)
		assert.Equal(t, uintptr(unix.MS_SHARED), calls[0].flags)

		// Last: bind mount target to itself
		lastCall := calls[len(calls)-1]
		assert.Equal(t, "/run/container/rootfs", lastCall.source)
		assert.Equal(t, "/run/container/rootfs", lastCall.target)
		assert.Equal(t, "bind", lastCall.fstype)
		assert.Equal(t, uintptr(unix.MS_BIND|unix.MS_REC), lastCall.flags)
	})
}

func TestMountVolumes(t *testing.T) {
	origMount := mountSyscall
	origStat := statSyscall
	t.Cleanup(func() {
		mountSyscall = origMount
		statSyscall = origStat
	})

	statSyscall = func(path string, stat *unix.Stat_t) error {
		stat.Mode = unix.S_IFDIR | 0o755
		return nil
	}

	t.Run("flag accumulation and clearing", func(t *testing.T) {
		var mountCalls []mountCall
		mountSyscall = func(source string, target string, fstype string, flags uintptr, data string) error {
			mountCalls = append(mountCalls, mountCall{source, target, fstype, flags, data})
			return nil
		}

		mounts := []specs.Mount{
			{
				Type:        "bind",
				Source:      "/host/data",
				Destination: "/container/data",
				Options:     []string{"ro", "bind", "nodev", "rw"}, // rw should clear ro/RDONLY
			},
		}

		err := mountVolumes("/tmp/rootfs", mounts)
		assert.NoError(t, err)

		// Verification of exact accumulation:
		// Options should result in: MS_BIND | MS_NODEV (accumulated bind + nodev, and ro cleared by rw)
		// Since MS_NODEV is outside basic bind flags, fileFromHost does:
		// 1. bind mount with mFlags (MS_BIND | MS_NODEV)
		// 2. remount to apply options (MS_BIND | MS_NODEV | MS_REMOUNT)
		assert.Len(t, mountCalls, 2)
		assert.Equal(t, "/host/data", mountCalls[0].source)
		assert.Equal(t, "/tmp/rootfs/container/data", mountCalls[0].target)
		assert.Equal(t, uintptr(unix.MS_BIND|unix.MS_NODEV), mountCalls[0].flags)

		assert.Equal(t, "/tmp/rootfs/container/data", mountCalls[1].source)
		assert.Equal(t, "/tmp/rootfs/container/data", mountCalls[1].target)
		assert.Equal(t, uintptr(unix.MS_BIND|unix.MS_NODEV|unix.MS_REMOUNT), mountCalls[1].flags)
	})

	t.Run("propagation flag handling and unknown-flag skipping", func(t *testing.T) {
		var mountCalls []mountCall
		mountSyscall = func(source string, target string, fstype string, flags uintptr, data string) error {
			mountCalls = append(mountCalls, mountCall{source, target, fstype, flags, data})
			return nil
		}

		mounts := []specs.Mount{
			{
				Type:        "bind",
				Source:      "/host/data",
				Destination: "/container/data",
				Options:     []string{"rshared", "unknown-flag"}, // unknown flag is ignored, rshared sets propagation mount
			},
		}

		err := mountVolumes("/tmp/rootfs", mounts)
		assert.NoError(t, err)

		// Expected mount calls:
		// 1. fileFromHost on s.Destination (flags = 0 since only "rshared" and "unknown-flag" are present, neither map to standard mount flags)
		// 2. propagation remount (flags = MS_SHARED | MS_REC)
		assert.Len(t, mountCalls, 2)
		assert.Equal(t, "/host/data", mountCalls[0].source)
		assert.Equal(t, "/tmp/rootfs/container/data", mountCalls[0].target)
		assert.Equal(t, uintptr(0), mountCalls[0].flags)

		assert.Equal(t, "/tmp/rootfs/container/data", mountCalls[1].source)
		assert.Equal(t, "/tmp/rootfs/container/data", mountCalls[1].target)
		assert.Equal(t, uintptr(unix.MS_SHARED|unix.MS_REC), mountCalls[1].flags)
	})
}
