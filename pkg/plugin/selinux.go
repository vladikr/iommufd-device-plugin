package plugin

import (
	"fmt"
	"log"
	"os"

	"github.com/opencontainers/selinux/go-selinux"
)

const (
	// containerFileLabel is the SELinux context that allows container_t
	// processes to receive FDs via SCM_RIGHTS without being blocked by
	// security_file_receive() checks.
	containerFileLabel = "system_u:object_r:container_file_t:s0"
)

// selinuxState caches the SELinux detection result.
type selinuxState struct {
	enabled    bool
	permissive bool
}

// detectSELinux checks whether SELinux is enabled and in which mode.
func detectSELinux() selinuxState {
	if !selinux.GetEnabled() {
		return selinuxState{enabled: false}
	}
	mode := selinux.EnforceMode()
	return selinuxState{
		enabled:    true,
		permissive: mode == selinux.Permissive,
	}
}

// relabelPath sets the SELinux label on the given path to container_file_t:s0
// so that container_t processes can access FDs originating from this file.
//
// When SELinux is enforcing and an FD is passed via SCM_RIGHTS, the kernel
// calls security_file_receive() which checks whether the receiver's context
// (container_t) is allowed to use an FD with the source file's context.
// Files under /dev typically have device_t context which container_t cannot
// access. Relabeling to container_file_t:s0 allows the FD transfer to succeed.
func relabelPath(path string, continueOnError bool) error {
	if err := selinux.SetFileLabel(path, containerFileLabel); err != nil {
		if continueOnError {
			log.Printf("WARNING: failed to relabel %s (non-fatal in permissive mode): %v", path, err)
			return nil
		}
		return fmt.Errorf("failed to relabel %s with %s: %w", path, containerFileLabel, err)
	}
	log.Printf("Relabeled %s with SELinux context %s", path, containerFileLabel)
	return nil
}

// relabelIfSELinux relabels the given path only if SELinux is enabled.
// In permissive mode, relabeling errors are logged but not returned.
func relabelIfSELinux(se selinuxState, path string) error {
	if !se.enabled {
		return nil
	}
	return relabelPath(path, se.permissive)
}

// ensureDirWithSELinux creates a directory and relabels it if SELinux is enabled.
func ensureDirWithSELinux(se selinuxState, dir string) error {
	if err := os.MkdirAll(dir, 0766); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", dir, err)
	}
	return relabelIfSELinux(se, dir)
}
