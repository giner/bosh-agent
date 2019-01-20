package disk

import (
	"strings"
	"syscall"
	"time"
	"fmt"
	"path/filepath"

	bosherr "github.com/cloudfoundry/bosh-utils/errors"
	boshsys "github.com/cloudfoundry/bosh-utils/system"
)

type linuxMounter struct {
	fs                boshsys.FileSystem
	runner            boshsys.CmdRunner
	mountsSearcher    MountsSearcher
	maxUnmountRetries int
	unmountRetrySleep time.Duration
}

func NewLinuxMounter(
	fs boshsys.FileSystem,
	runner boshsys.CmdRunner,
	mountsSearcher MountsSearcher,
	unmountRetrySleep time.Duration,
) Mounter {
	return linuxMounter{
		fs:                fs,
		runner:            runner,
		mountsSearcher:    mountsSearcher,
		maxUnmountRetries: 600,
		unmountRetrySleep: unmountRetrySleep,
	}
}

func (m linuxMounter) Mount(partitionPath, mountPoint string, mountOptions ...string) error {
	return m.MountFilesystem(partitionPath, mountPoint, "", mountOptions...)
}

func (m linuxMounter) MountFilesystem(partitionPath, mountPoint, fstype string, mountOptions ...string) error {
	shouldMount, err := m.shouldMount(partitionPath, mountPoint)
	if !shouldMount {
		return err
	}

	if err != nil {
		return bosherr.WrapError(err, "Checking whether partition should be mounted")
	}

	mountArgs := []string{partitionPath, mountPoint}
	if fstype != "" {
		mountArgs = append(mountArgs, "-t", fstype)
	}

	for _, mountOption := range mountOptions {
		mountArgs = append(mountArgs, "-o", mountOption)
	}

	_, _, _, err = m.runner.RunCommand("mount", mountArgs...)
	if err != nil {
		return bosherr.WrapError(err, "Shelling out to mount")
	}

	return nil
}

func (m linuxMounter) RemountAsReadonly(mountPoint string) error {
	return m.Remount(mountPoint, mountPoint, "ro")
}

func (m linuxMounter) Remount(fromMountPoint, toMountPoint string, mountOptions ...string) error {
	partitionPath, found, err := m.IsMountPoint(fromMountPoint)
	if err != nil || !found {
		return bosherr.WrapErrorf(err, "Error finding device for mount point %s", fromMountPoint)
	}

	_, err = m.Unmount(fromMountPoint)
	if err != nil {
		return bosherr.WrapErrorf(err, "Unmounting %s", fromMountPoint)
	}

	return m.Mount(partitionPath, toMountPoint, mountOptions...)
}

func (m linuxMounter) SwapOn(partitionPath string) (err error) {
	out, _, _, _ := m.runner.RunCommand("swapon", "-s")

	for i, swapOnLines := range strings.Split(out, "\n") {
		swapOnFields := strings.Fields(swapOnLines)

		switch {
		case i == 0:
			continue
		case len(swapOnFields) == 0:
			continue
		case swapOnFields[0] == partitionPath:
			return nil
		}
	}

	_, _, _, err = m.runner.RunCommand("swapon", partitionPath)
	if err != nil {
		return bosherr.WrapError(err, "Shelling out to swapon")
	}

	return nil
}

func (m linuxMounter) Unmount(partitionOrMountPoint string) (bool, error) {
	isMounted, err := m.IsMounted(partitionOrMountPoint)
	if err != nil || !isMounted {
		return false, err
	}

	_, _, _, err = m.runner.RunCommand("umount", partitionOrMountPoint)

	for i := 1; i < m.maxUnmountRetries && err != nil; i++ {
		time.Sleep(m.unmountRetrySleep)
		_, _, _, err = m.runner.RunCommand("umount", partitionOrMountPoint)
	}

	return err == nil, err
}

func (m linuxMounter) Detach(realPath string) (bool, error) {
	isMounted, err := m.IsMounted(realPath)
	if err != nil || isMounted {
		return false, err
	}

	stat := syscall.Stat_t{}
	_ = syscall.Stat(realPath, &stat)
	blockDevicePath, err := m.fs.Readlink(fmt.Sprintf("/sys/dev/block/%d:%d", stat.Rdev/256, stat.Rdev%256))
	if err != nil {
		return false, err
	}

	// blockDevicePath can point to either partition (.../block/sda/sda1) or physical device (.../block/sda)
	// Make sure we get physical one
	physicalDeviceName := fmt.Sprintf("/sys/block/%s/device/delete", filepath.Base(blockDevicePath))
	parent := filepath.Base(filepath.Dir(blockDevicePath))
	if parent != "block" {
		physicalDeviceName = parent
	}

	deletePhysicalDevicePath := fmt.Sprintf("/sys/block/%s/device/delete", physicalDeviceName)
	err = m.fs.WriteFileString(deletePhysicalDevicePath, "1")
	return err == nil, err
}

func (m linuxMounter) IsMountPoint(path string) (string, bool, error) {
	mounts, err := m.mountsSearcher.SearchMounts()
	if err != nil {
		return "", false, bosherr.WrapError(err, "Searching mounts")
	}

	for _, mount := range mounts {
		if mount.MountPoint == path {
			return mount.PartitionPath, true, nil
		}
	}

	return "", false, nil
}

func (m linuxMounter) IsMounted(partitionOrMountPoint string) (bool, error) {
	mounts, err := m.mountsSearcher.SearchMounts()
	if err != nil {
		return false, bosherr.WrapError(err, "Searching mounts")
	}

	for _, mount := range mounts {
		if mount.PartitionPath == partitionOrMountPoint || mount.MountPoint == partitionOrMountPoint {
			return true, nil
		}
	}

	return false, nil
}

func (m linuxMounter) shouldMount(partitionPath, mountPoint string) (bool, error) {
	mounts, err := m.mountsSearcher.SearchMounts()
	if err != nil {
		return false, bosherr.WrapError(err, "Searching mounts")
	}

	for _, mount := range mounts {
		switch {
		case mount.PartitionPath == partitionPath && mount.MountPoint == mountPoint:
			return false, nil
		case mount.PartitionPath == partitionPath && mount.MountPoint != mountPoint && partitionPath != "tmpfs":
			return false, bosherr.Errorf("Device %s is already mounted to %s, can't mount to %s",
				mount.PartitionPath, mount.MountPoint, mountPoint)
		case mount.MountPoint == mountPoint && partitionPath != "":
			return false, bosherr.Errorf("Device %s is already mounted to %s, can't mount %s",
				mount.PartitionPath, mount.MountPoint, partitionPath)
		}
	}

	return true, nil
}

func (m linuxMounter) RemountInPlace(mountPoint string, mountOptions ...string) (err error) {
	found, err := m.IsMounted(mountPoint)
	if err != nil || !found {
		return bosherr.WrapErrorf(err, "Error finding existing mount point %s", mountPoint)
	}

	return m.Mount("", mountPoint, append([]string{"remount"}, mountOptions...)...)
}
