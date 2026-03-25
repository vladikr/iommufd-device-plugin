package plugin

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/google/uuid"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

const (
	deviceNamespace   = "devices.kubevirt.io"
	deviceName        = "iommufd"
	maxDevices        = 110
	connectionTimeout = 5 * time.Second
	kubeletSocket     = "/var/lib/kubelet/device-plugins/kubelet.sock"
	devicePluginPath  = "/var/lib/kubelet/device-plugins/"
)

// IOMMUFDDevicePlugin implements the Kubernetes device plugin API for IOMMUFD.
// It always advertises maxDevices virtual devices. On Allocate, if /dev/iommu
// is present, it opens and configures the FD and passes it via SCM_RIGHTS.
// If /dev/iommu is absent, it returns a successful empty response so pods
// are never rejected due to missing IOMMUFD support.
type IOMMUFDDevicePlugin struct {
	devs         []*pluginapi.Device
	server       *grpc.Server
	socketPath   string
	resourceName string
	socketDir    string
	selinux      selinuxState
	stop         <-chan struct{}
	health       chan string
	done         chan struct{}
	deregistered chan struct{}
	initialized  bool
	lock         sync.Mutex
}

func NewIOMMUFDDevicePlugin(socketDir string) *IOMMUFDDevicePlugin {
	socketPath := filepath.Join(devicePluginPath, fmt.Sprintf("kubevirt-%s.sock", deviceName))
	resourceName := fmt.Sprintf("%s/%s", deviceNamespace, deviceName)

	devs := make([]*pluginapi.Device, maxDevices)
	for i := 0; i < maxDevices; i++ {
		devs[i] = &pluginapi.Device{
			ID:     deviceName + strconv.Itoa(i),
			Health: pluginapi.Healthy,
		}
	}

	se := detectSELinux()
	if se.enabled {
		mode := "enforcing"
		if se.permissive {
			mode = "permissive"
		}
		log.Printf("SELinux detected (mode=%s), will relabel IOMMUFD nodes and sockets", mode)
	} else {
		log.Printf("SELinux not detected, skipping relabeling")
	}

	return &IOMMUFDDevicePlugin{
		devs:         devs,
		socketPath:   socketPath,
		resourceName: resourceName,
		socketDir:    socketDir,
		selinux:      se,
		health:       make(chan string),
	}
}

func (dp *IOMMUFDDevicePlugin) Start(stop <-chan struct{}) error {
	dp.stop = stop
	dp.done = make(chan struct{})
	dp.deregistered = make(chan struct{})

	if err := dp.cleanup(); err != nil {
		return err
	}

	sock, err := net.Listen("unix", dp.socketPath)
	if err != nil {
		return fmt.Errorf("error creating GRPC server socket: %v", err)
	}

	dp.server = grpc.NewServer()
	defer dp.stopPlugin()

	pluginapi.RegisterDevicePluginServer(dp.server, dp)

	errChan := make(chan error, 2)

	go func() {
		errChan <- dp.server.Serve(sock)
	}()

	if err := waitForGRPCServer(dp.socketPath, connectionTimeout); err != nil {
		return fmt.Errorf("error starting the GRPC server: %v", err)
	}

	if err := dp.register(); err != nil {
		return fmt.Errorf("error registering with device plugin manager: %v", err)
	}

	go func() {
		errChan <- dp.healthCheck()
	}()

	dp.setInitialized(true)
	log.Printf("%s device plugin started (resource=%s, devices=%d)", deviceName, dp.resourceName, maxDevices)
	return <-errChan
}

func (dp *IOMMUFDDevicePlugin) stopPlugin() {
	defer func() {
		select {
		case <-dp.done:
		default:
			close(dp.done)
		}
	}()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	select {
	case <-dp.deregistered:
	case <-ticker.C:
	}
	dp.server.Stop()
	dp.setInitialized(false)
	dp.cleanup()
}

func (dp *IOMMUFDDevicePlugin) register() error {
	ctx, cancel := context.WithTimeout(context.Background(), connectionTimeout)
	defer cancel()
	conn, err := grpc.DialContext(ctx, "unix://"+kubeletSocket,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		return fmt.Errorf("error connecting to kubelet: %v", err)
	}
	defer conn.Close()

	client := pluginapi.NewRegistrationClient(conn)
	req := &pluginapi.RegisterRequest{
		Version:      pluginapi.Version,
		Endpoint:     path.Base(dp.socketPath),
		ResourceName: dp.resourceName,
	}

	_, err = client.Register(context.Background(), req)
	return err
}

func (dp *IOMMUFDDevicePlugin) GetDevicePluginOptions(_ context.Context, _ *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	return &pluginapi.DevicePluginOptions{PreStartRequired: false}, nil
}

func (dp *IOMMUFDDevicePlugin) ListAndWatch(_ *pluginapi.Empty, s pluginapi.DevicePlugin_ListAndWatchServer) error {
	s.Send(&pluginapi.ListAndWatchResponse{Devices: dp.devs})

	for {
		select {
		case h := <-dp.health:
			for _, dev := range dp.devs {
				dev.Health = h
			}
			s.Send(&pluginapi.ListAndWatchResponse{Devices: dp.devs})
		case <-dp.stop:
		case <-dp.done:
		}

		select {
		case <-dp.stop:
			break
		case <-dp.done:
			break
		default:
			continue
		}
		break
	}

	// Deregister by sending empty list
	s.Send(&pluginapi.ListAndWatchResponse{Devices: []*pluginapi.Device{}})
	close(dp.deregistered)
	return nil
}

// Allocate is called when a pod requests devices.kubevirt.io/iommufd.
// If /dev/iommu exists: opens, configures RLIMIT_MODE, creates a socket for
// FD passing, and mounts it into the container.
// If /dev/iommu does not exist: returns a successful empty response so the
// pod is not rejected.
func (dp *IOMMUFDDevicePlugin) Allocate(_ context.Context, r *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	log.Printf("Allocate called: %d container request(s)", len(r.ContainerRequests))

	response := &pluginapi.AllocateResponse{}

	for range r.ContainerRequests {
		containerResponse := &pluginapi.ContainerAllocateResponse{}

		if !supportsIOMMUFD() {
			log.Printf("IOMMUFD not supported on this node (/dev/iommu not found), returning empty response")
			response.ContainerResponses = append(response.ContainerResponses, containerResponse)
			continue
		}

		// Expose /dev/iommu to the container
		containerResponse.Devices = []*pluginapi.DeviceSpec{
			{
				HostPath:      iommuDevicePath,
				ContainerPath: iommuDevicePath,
				Permissions:   "mrw",
			},
		}

		socketID := uuid.New().String()
		iommuFD, err := openAndConfigureIOMMUFD(dp.selinux, dp.socketDir, socketID)
		if err != nil {
			log.Printf("WARNING: failed to open/configure IOMMUFD: %v (returning without FD)", err)
			response.ContainerResponses = append(response.ContainerResponses, containerResponse)
			continue
		}

		hostSocketPath, err := createIOMMUFDSocket(iommuFD, dp.selinux, dp.socketDir, socketID)
		if err != nil {
			log.Printf("WARNING: failed to create IOMMUFD socket: %v", err)
			unix.Close(iommuFD)
			response.ContainerResponses = append(response.ContainerResponses, containerResponse)
			continue
		}

		containerResponse.Mounts = []*pluginapi.Mount{
			{
				HostPath:      hostSocketPath,
				ContainerPath: IOMMUFDContainerSocketPath,
				ReadOnly:      false,
			},
		}

		response.ContainerResponses = append(response.ContainerResponses, containerResponse)
	}

	return response, nil
}

func (dp *IOMMUFDDevicePlugin) GetPreferredAllocation(_ context.Context, _ *pluginapi.PreferredAllocationRequest) (*pluginapi.PreferredAllocationResponse, error) {
	return &pluginapi.PreferredAllocationResponse{}, nil
}

func (dp *IOMMUFDDevicePlugin) PreStartContainer(_ context.Context, _ *pluginapi.PreStartContainerRequest) (*pluginapi.PreStartContainerResponse, error) {
	return &pluginapi.PreStartContainerResponse{}, nil
}

func (dp *IOMMUFDDevicePlugin) healthCheck() error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("failed to create fsnotify watcher: %v", err)
	}
	defer watcher.Close()

	// Watch /dev for iommu device appearance/disappearance
	if err := watcher.Add("/dev"); err != nil {
		return fmt.Errorf("failed to watch /dev: %v", err)
	}

	// Check initial state - but don't mark unhealthy since we still want
	// pods to be schedulable even without /dev/iommu
	if supportsIOMMUFD() {
		log.Printf("/dev/iommu is present")
	} else {
		log.Printf("/dev/iommu is not present (plugin will still accept allocations)")
	}

	// Watch device plugin socket directory
	socketDir := filepath.Dir(dp.socketPath)
	if err := watcher.Add(socketDir); err != nil {
		return fmt.Errorf("failed to watch device plugin socket dir: %v", err)
	}
	if _, err := os.Stat(dp.socketPath); err != nil {
		return fmt.Errorf("device plugin socket not found: %v", err)
	}

	for {
		select {
		case <-dp.stop:
			return nil
		case err := <-watcher.Errors:
			log.Printf("ERROR: watcher error: %v", err)
		case event := <-watcher.Events:
			if event.Name == iommuDevicePath {
				if event.Op == fsnotify.Create {
					log.Printf("/dev/iommu appeared")
				} else if event.Op == fsnotify.Remove || event.Op == fsnotify.Rename {
					log.Printf("/dev/iommu disappeared")
				}
				// We intentionally do NOT change device health here.
				// The plugin should always report healthy so pods are schedulable.
			} else if event.Name == dp.socketPath && event.Op == fsnotify.Remove {
				log.Printf("device socket removed, kubelet probably restarted")
				return nil
			}
		}
	}
}

func (dp *IOMMUFDDevicePlugin) cleanup() error {
	if err := os.Remove(dp.socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (dp *IOMMUFDDevicePlugin) GetInitialized() bool {
	dp.lock.Lock()
	defer dp.lock.Unlock()
	return dp.initialized
}

func (dp *IOMMUFDDevicePlugin) setInitialized(v bool) {
	dp.lock.Lock()
	defer dp.lock.Unlock()
	dp.initialized = v
}

func waitForGRPCServer(socketPath string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	for {
		conn, err := grpc.DialContext(ctx, "unix://"+socketPath,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithBlock(),
		)
		if err == nil {
			conn.Close()
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("timed out waiting for gRPC server at %s", socketPath)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
