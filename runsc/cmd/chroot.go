// Copyright 2019 The gVisor Authors.
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

package cmd

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/runsc/cmd/util"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/specutils"
)

// mountInChroot creates the destination mount point in the given chroot and
// mounts the source.
func mountInChroot(chroot, src, dst, typ string, flags uint32) error {
	chrootDst := filepath.Join(chroot, dst)
	log.Infof("Mounting %q at %q", src, chrootDst)

	if err := specutils.SafeSetupAndMount(src, chrootDst, typ, flags, "/proc"); err != nil {
		return fmt.Errorf("error mounting %q at %q: %v", src, chrootDst, err)
	}
	return nil
}

func pivotRoot(root string) error {
	if err := os.Chdir(root); err != nil {
		return fmt.Errorf("error changing working directory: %v", err)
	}
	// pivot_root(new_root, put_old) moves the root filesystem (old_root)
	// of the calling process to the directory put_old and makes new_root
	// the new root filesystem of the calling process.
	//
	// pivot_root(".", ".") makes a mount of the working directory the new
	// root filesystem, so it will be moved in "/" and then the old_root
	// will be moved to "/" too. The parent mount of the old_root will be
	// new_root, so after umounting the old_root, we will see only
	// the new_root in "/".
	if err := unix.PivotRoot(".", "."); err != nil {
		return fmt.Errorf("pivot_root failed, make sure that the root mount has a parent: %v", err)
	}

	if err := unix.Unmount(".", unix.MNT_DETACH); err != nil {
		return fmt.Errorf("error umounting the old root file system: %v", err)
	}
	return nil
}

func copyFile(dst, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = out.ReadFrom(in)
	return err
}

// setUpChroot creates an empty directory with runsc mounted at /runsc and proc
// mounted at /proc.
func setUpChroot(pidns bool, spec *specs.Spec, conf *config.Config) error {
	// We are a new mount namespace, so we can use /tmp as a directory to
	// construct a new root.
	chroot := os.TempDir()

	log.Infof("Setting up sandbox chroot in %q", chroot)

	// Convert all shared mounts into slave to be sure that nothing will be
	// propagated outside of our namespace.
	if err := specutils.SafeMount("", "/", "", unix.MS_SLAVE|unix.MS_REC, "", "/proc"); err != nil {
		return fmt.Errorf("error converting mounts: %v", err)
	}

	if err := specutils.SafeMount("runsc-root", chroot, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "", "/proc"); err != nil {
		return fmt.Errorf("error mounting tmpfs in chroot: %v", err)
	}

	if err := os.Mkdir(filepath.Join(chroot, "etc"), 0755); err != nil {
		return fmt.Errorf("error creating /etc in chroot: %v", err)
	}

	if err := copyFile(filepath.Join(chroot, "etc/localtime"), "/etc/localtime"); err != nil {
		log.Warningf("Failed to copy /etc/localtime: %v. UTC timezone will be used.", err)
	}

	if pidns {
		flags := uint32(unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC | unix.MS_RDONLY)
		if err := mountInChroot(chroot, "proc", "/proc", "proc", flags); err != nil {
			return fmt.Errorf("error mounting proc in chroot: %v", err)
		}
	} else {
		if err := mountInChroot(chroot, "/proc", "/proc", "bind", unix.MS_BIND|unix.MS_RDONLY|unix.MS_REC); err != nil {
			return fmt.Errorf("error mounting proc in chroot: %v", err)
		}
	}

	if err := tpuProxyUpdateChroot(chroot, spec, conf); err != nil {
		return fmt.Errorf("error configuring chroot for TPU devices: %w", err)
	}

	if err := specutils.SafeMount("", chroot, "", unix.MS_REMOUNT|unix.MS_RDONLY|unix.MS_BIND, "", "/proc"); err != nil {
		return fmt.Errorf("error remounting chroot in read-only: %v", err)
	}

	return pivotRoot(chroot)
}

// Mount the path that dest points to for TPU at chroot, the mounted path is returned in absolute form.
func mountTPUSyslinkInChroot(chroot, dest, relativePath string, validator func(link string) bool) (string, error) {
	src, err := os.Readlink(dest)
	if err != nil {
		return "", fmt.Errorf("error reading %v: %v", src, err)
	}
	// Ensure the link is in the form we expect.
	if !validator(src) {
		return "", fmt.Errorf("unexpected link %q -> %q", dest, src)
	}
	path, err := filepath.Abs(path.Join(filepath.Dir(dest), src, relativePath))
	if err != nil {
		return "", fmt.Errorf("error parsing path %q: %v", src, err)
	}
	if err := mountInChroot(chroot, path, path, "bind", unix.MS_BIND|unix.MS_RDONLY); err != nil {
		return "", fmt.Errorf("error mounting %q in chroot: %v", dest, err)
	}
	return path, nil
}

func mountTPUDeviceInfoInChroot(chroot, devicePath, sysfsFormat, pciDeviceFormat string) error {
	deviceNum, valid, err := util.ExtractTpuDeviceMinor(devicePath)
	if err != nil {
		return fmt.Errorf("extracting TPU device minor: %w", err)
	}
	if !valid {
		return nil
	}
	// Multiple paths link to the /sys/devices/pci0000:00/<pci_address>
	// directory that contains all relevant sysfs accel/vfio device info that we need
	// bind mounted into the sandbox chroot. We can construct this path by
	// reading the link below, which points to
	//   * /sys/devices/pci0000:00/<pci_address>/accel/accel#
	//   * or /sys/devices/pci0000:00/<pci_address>/vfio-dev/vfio# for VFIO-based TPU
	// and traversing up 2 directories.
	// The sysDevicePath itself is a soft link to the deivce directory.
	sysDevicePath := fmt.Sprintf(sysfsFormat, deviceNum)
	sysPCIDeviceDir, err := mountTPUSyslinkInChroot(chroot, sysDevicePath, "../..", func(link string) bool {
		sysDeviceLinkMatcher := regexp.MustCompile(fmt.Sprintf(pciDeviceFormat, deviceNum))
		return sysDeviceLinkMatcher.MatchString(link)
	})
	if err != nil {
		return err
	}

	// Mount the device's IOMMU group if available.
	iommuGroupPath := path.Join(sysPCIDeviceDir, "iommu_group")
	if _, err := os.Stat(iommuGroupPath); err == nil {
		if _, err := mountTPUSyslinkInChroot(chroot, iommuGroupPath, "", func(link string) bool {
			return fmt.Sprintf("../../../kernel/iommu_groups/%d", deviceNum) == link
		}); err != nil {
			return err
		}
	}
	return nil
}

func tpuProxyUpdateChroot(chroot string, spec *specs.Spec, conf *config.Config) error {
	if !specutils.TPUProxyIsEnabled(spec, conf) {
		return nil
	}
	// When a path glob is added to pathGlobToSysfsFormat, the corresponding pciDeviceFormat has to be added to pathGlobToPciDeviceFormat.
	pathGlobToSysfsFormat := map[string]string{
		"/dev/accel*": "/sys/class/accel/accel%d",
		"/dev/vfio/*": "/sys/class/vfio-dev/vfio%d"}
	pathGlobToPciDeviceFormat := map[string]string{
		"/dev/accel*": `../../devices/pci0000:00/(\d+:\d+:\d+\.\d+)/accel/accel%d`,
		"/dev/vfio/*": `../../devices/pci0000:00/(\d+:\d+:\d+\.\d+)/vfio-dev/vfio%d`}
	// Bind mount device info directories for all TPU devices on the host.
	// For v4 TPU, the directory /sys/devices/pci0000:00/<pci_address>/accel/accel# is mounted;
	// For v5e TPU, the directory /sys/devices/pci0000:00/<pci_address>/vfio-dev/vfio# is mounted.
	for pathGlob, sysfsFormat := range pathGlobToSysfsFormat {
		paths, err := filepath.Glob(pathGlob)
		if err != nil {
			return fmt.Errorf("enumerating TPU device files: %w", err)
		}
		for _, devPath := range paths {
			if err := mountTPUDeviceInfoInChroot(chroot, devPath, sysfsFormat, pathGlobToPciDeviceFormat[pathGlob]); err != nil {
				return err
			}
		}
	}
	return nil
}
