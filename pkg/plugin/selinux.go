package plugin

import (
	"fmt"
	"log"
	"os"

	"golang.org/x/sys/unix"
)

const (
	// containerFileLabel is the SELinux context that allows container_t
	// processes to receive FDs via SCM_RIGHTS without being blocked by
	// security_file_receive() checks.
	containerFileLabel = "system_u:object_r:container_file_t:s0"
)

// relabelPath sets the SELinux label on the given path to container_file_t:s0
// so that container_t processes can access FDs originating from this file.
//
// When SELinux is enforcing and an FD is passed via SCM_RIGHTS, the kernel
// calls security_file_receive() which checks whether the receiver's context
// (container_t) is allowed to use an FD with the source file's context.
// Files under /dev typically have device_t context which container_t cannot
// access. Relabeling to container_file_t:s0 allows the FD transfer to succeed.
//
// This always attempts the lsetxattr syscall directly rather than trying to
// detect SELinux first. Inside containers, selinuxfs may not be mounted so
// detection via /sys/fs/selinux fails, but lsetxattr still works in
// privileged containers. If SELinux is not present, the syscall returns
// ENOTSUP and we log and continue.
func relabelPath(path string) error {
	label := containerFileLabel + "\x00" // null-terminated as required by xattr
	if err := unix.Lsetxattr(path, "security.selinux", []byte(label), 0); err != nil {
		if err == unix.ENOTSUP || err == unix.ENODATA {
			log.Printf("SELinux xattr not supported on %s (no SELinux), skipping", path)
			return nil
		}
		return fmt.Errorf("failed to relabel %s with %s: %w", path, containerFileLabel, err)
	}
	log.Printf("Relabeled %s with SELinux context %s", path, containerFileLabel)
	return nil
}

// ensureDirWithRelabel creates a directory and attempts SELinux relabeling.
func ensureDirWithRelabel(dir string) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", dir, err)
	}
	return relabelPath(dir)
}
