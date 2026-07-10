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
	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
	"golang.org/x/sys/unix"
)

func TestChooseTmpfsSize(t *testing.T) {
	t.Parallel()

	// BytesToStringMB divides by decimal 1_000_000 (standard decimal MB).
	// To disambiguate and ensure we test both binary bounds and round-number decimal cases:
	tests := []struct {
		name     string
		sfsType  string
		memory   uint64
		expected string
	}{
		{
			name:     "9pfs uses default constant size",
			sfsType:  "9pfs",
			memory:   1024 * 1024 * 1024,
			expected: "65536k",
		},
		{
			name:     "virtiofs with zero memory falls back to DefaultMemory (256MB) which yields 1m as 1MiB overhead is added",
			sfsType:  "virtiofs",
			memory:   0,
			expected: "1m",
		},
		{
			name:     "virtiofs with 100MB (100_000_000 bytes) + 1MiB (1_048_576 bytes) rounds to 101m",
			sfsType:  "virtiofs",
			memory:   100 * 1000 * 1000,
			expected: "101m",
		},
		{
			name:     "virtiofs with 256 MiB (268_435_456 bytes) + 1MiB (1_048_576) rounds to 269m",
			sfsType:  "virtiofs",
			memory:   256 * 1024 * 1024,
			expected: "269m",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := chooseTmpfsSize(tt.sfsType, tt.memory)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestAdjustPathsForSharedfs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		path     string
		expected string
	}{
		{
			name:     "empty path returns empty",
			path:     "",
			expected: "",
		},
		{
			name:     "non-empty path prepended with rootfs path dynamically using the actual Go constant",
			path:     "usr/lib",
			expected: filepath.Join(containerRootfsMountPath, "usr/lib"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := adjustPathsForSharedfs(tt.path)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestSharedfsRootfsGetSharedDirs(t *testing.T) {
	t.Parallel()

	s := sharedfsRootfs{
		sfsType: "virtiofs",
	}

	params, err := s.getSharedDirs()
	assert.NoError(t, err)
	assert.Equal(t, containerRootfsMountPath, params.Path)
	assert.Equal(t, "virtiofs", params.Type)
}

func TestSharedfsRootfsPreStart(t *testing.T) {
	// Hook spawnProcess
	origSpawn := spawnProcessHook
	t.Cleanup(func() { spawnProcessHook = origSpawn })

	t.Run("9pfs returns nil directly", func(t *testing.T) {
		s := sharedfsRootfs{
			sfsType: "9pfs",
		}
		err := s.preStart()
		assert.NoError(t, err)
	})

	t.Run("virtiofs with empty options", func(t *testing.T) {
		var spawnBinary string
		var spawnArgs []string

		spawnProcessHook = func(binaryPath string, args []string) error {
			spawnBinary = binaryPath
			spawnArgs = args
			return nil
		}

		s := sharedfsRootfs{
			sfsType:    "virtiofs",
			sharedPath: "/tmp/shared",
			vfsdConfig: types.ExtraBinConfig{
				Path:    "/usr/bin/virtiofsd",
				Options: "", // empty options
			},
		}

		err := s.preStart()
		assert.NoError(t, err)
		assert.Equal(t, "/usr/bin/virtiofsd", spawnBinary)
		// Should only contain socket-path and shared-dir
		assert.Equal(t, []string{"--socket-path=/tmp/vhostqemu", "--shared-dir", "/tmp/shared"}, spawnArgs)
	})

	t.Run("virtiofs with custom options", func(t *testing.T) {
		var spawnArgs []string

		spawnProcessHook = func(binaryPath string, args []string) error {
			spawnArgs = args
			return nil
		}

		s := sharedfsRootfs{
			sfsType:    "virtiofs",
			sharedPath: "/tmp/shared",
			vfsdConfig: types.ExtraBinConfig{
				Path:    "/usr/bin/virtiofsd",
				Options: "--sandbox chroot --cache=none",
			},
		}

		err := s.preStart()
		assert.NoError(t, err)
		assert.Equal(t, []string{
			"--socket-path=/tmp/vhostqemu",
			"--shared-dir",
			"/tmp/shared",
			"--sandbox",
			"chroot",
			"--cache=none",
		}, spawnArgs)
	})
}

func TestSharedfsRootfsPostSetup(t *testing.T) {
	origMount := mountSyscall
	origStat := statSyscall
	origMkdirAll := mkdirAllHook
	origChmod := osChmodHook
	origOpen := openSyscall
	origClose := closeSyscall
	t.Cleanup(func() {
		mountSyscall = origMount
		statSyscall = origStat
		mkdirAllHook = origMkdirAll
		osChmodHook = origChmod
		openSyscall = origOpen
		closeSyscall = origClose
	})

	t.Run("happy path virtiofs", func(t *testing.T) {
		var mountCalls []mountCall
		var chmodCalled bool

		statSyscall = func(path string, stat *unix.Stat_t) error {
			stat.Mode = unix.S_IFDIR | 0o755
			return nil
		}

		mkdirAllHook = func(path string, perm os.FileMode) error {
			return nil
		}

		osChmodHook = func(name string, mode os.FileMode) error {
			chmodCalled = true
			return nil
		}

		mountSyscall = func(source string, target string, fstype string, flags uintptr, data string) error {
			mountCalls = append(mountCalls, mountCall{source, target, fstype, flags, data})
			return nil
		}

		s := sharedfsRootfs{
			sfsType:     "virtiofs",
			monRootfs:   "/tmp/mon",
			mountedPath: "/tmp/mounted",
			memory:      256 * 1000 * 1000,
			vfsdConfig: types.ExtraBinConfig{
				Path: "/usr/bin/virtiofsd",
			},
		}

		err := s.postSetup()
		assert.NoError(t, err)

		// Expected mount calls:
		// 1. fileFromHost on s.mountedPath (bind mount) -> 2 calls (bind, remount private)
		// 2. fileFromHost on s.vfsdConfig.Path (bind mount) -> 2 calls (bind, remount private)
		// 3. createTmpfs (/tmp mount tmpfs) -> 2 calls (mount tmpfs, remount private)
		assert.Len(t, mountCalls, 6)
		assert.Equal(t, "/tmp/mounted", mountCalls[0].source)
		assert.Equal(t, "/usr/bin/virtiofsd", mountCalls[2].source)
		assert.Equal(t, "tmpfs", mountCalls[4].source)
		assert.True(t, chmodCalled)
	})

	t.Run("happy path 9pfs", func(t *testing.T) {
		var mountCalls []mountCall

		statSyscall = func(path string, stat *unix.Stat_t) error {
			stat.Mode = unix.S_IFDIR | 0o755
			return nil
		}

		mkdirAllHook = func(path string, perm os.FileMode) error {
			return nil
		}

		osChmodHook = func(name string, mode os.FileMode) error {
			return nil
		}

		mountSyscall = func(source string, target string, fstype string, flags uintptr, data string) error {
			mountCalls = append(mountCalls, mountCall{source, target, fstype, flags, data})
			return nil
		}

		s := sharedfsRootfs{
			sfsType:     "9pfs",
			monRootfs:   "/tmp/mon",
			mountedPath: "/tmp/mounted",
			memory:      256 * 1000 * 1000,
		}

		err := s.postSetup()
		assert.NoError(t, err)

		// Expected mount calls:
		// 1. fileFromHost on s.mountedPath (bind mount) -> 2 calls (bind, remount private)
		// (virtiofsd mount skipped because sfsType is 9pfs)
		// 2. createTmpfs (/tmp mount tmpfs) -> 2 calls (mount tmpfs, remount private)
		assert.Len(t, mountCalls, 4)
		assert.Equal(t, "/tmp/mounted", mountCalls[0].source)
		assert.Equal(t, "tmpfs", mountCalls[2].source)
	})

	t.Run("failure on container rootfs mount", func(t *testing.T) {
		statSyscall = func(path string, stat *unix.Stat_t) error {
			return errors.New("stat failed")
		}

		s := sharedfsRootfs{
			sfsType:     "virtiofs",
			monRootfs:   "/tmp/mon",
			mountedPath: "/tmp/mounted",
		}

		err := s.postSetup()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to mount container's rootfs")
	})

	t.Run("failure on volume mounts", func(t *testing.T) {
		statSyscall = func(path string, stat *unix.Stat_t) error {
			// First call (container rootfs) succeeds
			if path == "/tmp/mounted" {
				stat.Mode = unix.S_IFDIR | 0o755
				return nil
			}
			// Second call (volumes stat) fails
			return errors.New("vol stat failed")
		}

		mountSyscall = func(source string, target string, fstype string, flags uintptr, data string) error {
			return nil
		}

		s := sharedfsRootfs{
			sfsType:     "virtiofs",
			monRootfs:   "/tmp/mon",
			mountedPath: "/tmp/mounted",
			mounts: []specs.Mount{
				{
					Type:        "bind",
					Source:      "/vol/src",
					Destination: "/vol/dst",
				},
			},
		}

		err := s.postSetup()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to mount volumes")
	})

	t.Run("failure on virtiofsd bind-mount", func(t *testing.T) {
		var mountCalls []mountCall

		statSyscall = func(path string, stat *unix.Stat_t) error {
			if path == "/tmp/mounted" {
				stat.Mode = unix.S_IFDIR | 0o755
			} else {
				stat.Mode = unix.S_IFREG | 0o755
			}
			return nil
		}

		mkdirAllHook = func(path string, perm os.FileMode) error {
			return nil
		}

		openSyscall = func(path string, mode int, perm uint32) (int, error) {
			return 100, nil
		}

		closeSyscall = func(fd int) error {
			return nil
		}

		mountSyscall = func(source string, target string, fstype string, flags uintptr, data string) error {
			// First two mount calls (for /tmp/mounted bind/remount) should succeed.
			if len(mountCalls) < 2 {
				mountCalls = append(mountCalls, mountCall{source, target, fstype, flags, data})
				return nil
			}
			// Next call (for virtiofsd) fails
			return errors.New("virtiofsd mount failed")
		}

		s := sharedfsRootfs{
			sfsType:     "virtiofs",
			monRootfs:   "/tmp/mon",
			mountedPath: "/tmp/mounted",
			vfsdConfig: types.ExtraBinConfig{
				Path: "/usr/bin/virtiofsd",
			},
		}

		err := s.postSetup()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "could not bind mount")
	})

	t.Run("failure on tmpfs creation", func(t *testing.T) {
		statSyscall = func(path string, stat *unix.Stat_t) error {
			stat.Mode = unix.S_IFDIR | 0o755
			return nil
		}

		mkdirAllHook = func(path string, perm os.FileMode) error {
			if path == "/tmp/mon/tmp" {
				return errors.New("mkdir tmp failed")
			}
			return nil
		}

		mountSyscall = func(source string, target string, fstype string, flags uintptr, data string) error {
			return nil
		}

		s := sharedfsRootfs{
			sfsType:     "9pfs",
			monRootfs:   "/tmp/mon",
			mountedPath: "/tmp/mounted",
		}

		err := s.postSetup()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to create tmpfs")
	})
}
