package plugin

import (
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	iommuDevicePath = "/dev/iommu"

	// IOMMUFDContainerSocketPath is the fixed path inside virt-launcher pods
	// where the IOMMUFD socket is mounted.
	IOMMUFDContainerSocketPath = "/var/run/kubevirt/iommufd.sock"

	// socketAcceptTimeout is the maximum time to wait for a client to connect
	// to the IOMMUFD socket before cleaning up resources.
	socketAcceptTimeout = 60 * time.Second

	// IOMMU_OPTION is the ioctl number for the IOMMUFD OPTION command.
	// Defined in Linux uAPI as _IO(IOMMUFD_TYPE, IOMMUFD_CMD_OPTION)
	// where IOMMUFD_TYPE = ';' (0x3B) and IOMMUFD_CMD_OPTION = 0x87.
	//
	//nolint:stylecheck,revive
	IOMMU_OPTION = 0x3B87
)

// iommuOption mirrors the kernel's struct iommu_option from
// include/uapi/linux/iommufd.h.
type iommuOption struct {
	Size     uint32
	OptionID uint32
	Op       uint16
	Reserved uint16
	ObjectID uint32
	Val64    uint64
}

// supportsIOMMUFD checks if /dev/iommu exists on the host.
func supportsIOMMUFD() bool {
	_, err := os.Stat(iommuDevicePath)
	return err == nil
}

// openAndConfigureIOMMUFD opens /dev/iommu via a relabeled temporary device
// node and sets IOMMU_OPTION_RLIMIT_MODE. This replicates libvirt's
// virIOMMUFDOpenDevice + virIOMMUFDSetRLimitMode.
//
// The temporary device node approach is required for SELinux: when an FD is
// passed via SCM_RIGHTS, the kernel calls security_file_receive() on the
// receiving end. If the FD was opened from /dev/iommu (context device_t),
// container_t is not allowed to receive it. By creating a temporary char
// device with the same major/minor and relabeling it to container_file_t:s0,
// the resulting FD carries a context that container_t can receive.
func openAndConfigureIOMMUFD(se selinuxState, socketDir string, uniqueID string) (int, error) {
	fd, err := openUnprivilegedIOMMUFD(se, socketDir, uniqueID)
	if err != nil {
		return -1, err
	}

	// Set IOMMU_OPTION_RLIMIT_MODE = true
	// This enables per-process RLIMIT_MEMLOCK accounting for IOMMU mappings,
	// matching what libvirt expects when managing IOMMUFD-backed devices.
	option := iommuOption{
		Size:     uint32(unsafe.Sizeof(iommuOption{})),
		OptionID: 0, // IOMMU_OPTION_RLIMIT_MODE
		Op:       0, // IOMMU_OPTION_OP_SET
		Val64:    1, // enable rlimit mode
	}

	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		uintptr(fd),
		uintptr(IOMMU_OPTION),
		uintptr(unsafe.Pointer(&option)),
	)
	if errno != 0 {
		unix.Close(fd)
		return -1, fmt.Errorf("IOMMU_OPTION ioctl failed: %v", errno)
	}

	log.Printf("Opened and configured IOMMUFD (fd=%d, rlimit_mode=true)", fd)
	return fd, nil
}

// openUnprivilegedIOMMUFD creates a temporary device node for /dev/iommu,
// relabels it with the container-friendly SELinux context, and returns an FD
// that virt-launcher is allowed to receive via SCM_RIGHTS.
func openUnprivilegedIOMMUFD(se selinuxState, socketDir string, uniqueID string) (int, error) {
	// Get major/minor of the real /dev/iommu
	var stat unix.Stat_t
	if err := unix.Stat(iommuDevicePath, &stat); err != nil {
		return -1, fmt.Errorf("failed to stat %s: %w", iommuDevicePath, err)
	}

	// Create temporary char device node inside the socket dir
	tmpNodePath := filepath.Join(socketDir, fmt.Sprintf("iommu-tmp-%s.dev", uniqueID))
	os.Remove(tmpNodePath)

	if err := unix.Mknod(tmpNodePath, unix.S_IFCHR|0600, int(stat.Rdev)); err != nil {
		return -1, fmt.Errorf("mknod failed for temporary iommu node: %w", err)
	}
	defer os.Remove(tmpNodePath)

	// Relabel the temporary node so the FD carries a container-friendly context
	if err := relabelIfSELinux(se, tmpNodePath); err != nil {
		return -1, fmt.Errorf("failed to relabel temporary iommu node: %w", err)
	}

	// Open the relabeled node — the FD now carries the correct SELinux context
	f, err := os.OpenFile(tmpNodePath, os.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("failed to open relabeled iommu node: %w", err)
	}

	// Extract the raw FD
	fd, err := unix.Dup(int(f.Fd()))
	f.Close()
	if err != nil {
		return -1, fmt.Errorf("dup failed: %w", err)
	}

	log.Printf("Created unprivileged IOMMUFD from relabeled node (fd=%d)", fd)
	return fd, nil
}

// createIOMMUFDSocket creates a one-shot Unix domain socket that transfers
// the IOMMUFD file descriptor to a connecting client via SCM_RIGHTS.
// The socket and its directory are relabeled for SELinux if needed.
// Returns the host-side socket path.
func createIOMMUFDSocket(iommuFD int, se selinuxState, socketDir string, uniqueID string) (string, error) {
	if err := ensureDirWithSELinux(se, socketDir); err != nil {
		return "", err
	}

	hostSocketPath := filepath.Join(socketDir, fmt.Sprintf("iommufd-%s.sock", uniqueID))

	// Remove stale socket file if it exists
	os.Remove(hostSocketPath)

	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: hostSocketPath, Net: "unix"})
	if err != nil {
		return "", fmt.Errorf("failed to listen on %s: %w", hostSocketPath, err)
	}

	// Allow virt-launcher to connect
	if err := os.Chmod(hostSocketPath, 0666); err != nil {
		listener.Close()
		os.Remove(hostSocketPath)
		return "", fmt.Errorf("failed to chmod socket %s: %w", hostSocketPath, err)
	}

	// Relabel the socket file itself
	if err := relabelIfSELinux(se, hostSocketPath); err != nil {
		listener.Close()
		os.Remove(hostSocketPath)
		return "", fmt.Errorf("failed to relabel socket: %w", err)
	}

	log.Printf("IOMMUFD socket created at %s, waiting for connection", hostSocketPath)

	// One-shot goroutine: accept one connection, send FD, clean up
	go func() {
		defer listener.Close()
		defer os.Remove(hostSocketPath)
		defer unix.Close(iommuFD)

		// Set deadline to prevent goroutine leak if client never connects
		if err := listener.SetDeadline(time.Now().Add(socketAcceptTimeout)); err != nil {
			log.Printf("ERROR: failed to set deadline on IOMMUFD socket: %v", err)
			return
		}

		conn, err := listener.AcceptUnix()
		if err != nil {
			log.Printf("ERROR: IOMMUFD socket accept failed: %v", err)
			return
		}
		defer conn.Close()

		log.Printf("IOMMUFD connection accepted, sending FD %d", iommuFD)

		rights := unix.UnixRights(iommuFD)
		if _, _, err := conn.WriteMsgUnix([]byte{0}, rights, nil); err != nil {
			log.Printf("ERROR: IOMMUFD WriteMsgUnix failed: %v", err)
			return
		}

		// Wait for ACK to keep connection alive until receiver has processed
		ack := make([]byte, 1)
		if _, err := conn.Read(ack); err != nil {
			log.Printf("WARNING: IOMMUFD ACK read failed (non-fatal): %v", err)
		} else {
			log.Printf("IOMMUFD FD successfully passed and ACK received (fd=%d)", iommuFD)
		}
	}()

	return hostSocketPath, nil
}
