# IOMMUFD Device Plugin

A KubeVirt device plugin that opens and configures `/dev/iommu` (IOMMUFD) and passes the file descriptor to virt-launcher pods via SCM_RIGHTS over a unix socket.

On modern kernels (6.2+), IOMMUFD replaces the legacy VFIO container model. IOMMUFD file descriptor needs to be pre-configured with `IOMMU_OPTION_RLIMIT_MODE` for proper memory pinning accounting during GPU/PCI device passthrough. Since virt-launcher runs unprivileged and cannot open `/dev/iommu` itself, this device plugin handles it.

If `/dev/iommu` is not present on the node, the plugin still accepts allocations and returns a successful empty response, so pods are never rejected due to missing IOMMUFD support.

## Building

1. **Build the binary**:
   ```sh
   make build
   ```

   For a specific architecture:
   ```sh
   make build ARCH=arm64
   ```

2. **Build the container image** (defaults to amd64):
   ```sh
   make image
   ```

   For arm64:
   ```sh
   make image ARCH=arm64
   ```

   To override the image name or tag:
   ```sh
   make image IMAGE=quay.io/myuser/iommufd-device-plugin TAG=v0.1.0
   ```

3. **Build a multi-arch manifest** (after building and pushing both architectures):
   ```sh
   make image ARCH=amd64
   make push ARCH=amd64
   make image ARCH=arm64
   make push ARCH=arm64
   make manifest
   make manifest-push
   ```

## Testing

```sh
make test
```

## Pushing the Image

```sh
make push
```

## Deploying

1. **Deploy the DaemonSet**:
   ```sh
   kubectl apply -f deploy/daemonset.yaml
   ```

   Ensure the `image` in `deploy/daemonset.yaml` matches the image you pushed.

2. **Verify deployment**:
   ```sh
   kubectl get pods -n kube-system | grep iommufd-device-plugin
   ```

3. **Verify the resource is advertised**:
   ```sh
   kubectl get node <node-name> -o jsonpath='{.status.allocatable}' | jq '.["devices.kubevirt.io/iommufd"]'
   ```

## Usage

The plugin registers `devices.kubevirt.io/iommufd` as a Kubernetes extended resource. Request it in pod specifications:

```yaml
resources:
  limits:
    devices.kubevirt.io/iommufd: "1"
```

When allocated, the plugin:

1. Opens `/dev/iommu` via a SELinux-relabeled temporary device node
2. Configures `IOMMU_OPTION_RLIMIT_MODE` (replicating libvirt's `virIOMMUFDOpenDevice`)
3. Creates a one-shot Unix socket and passes the configured FD via `SCM_RIGHTS`
4. Mounts the socket into the container at `/var/run/kubevirt/iommufd.sock`
5. Exposes `/dev/iommu` as a device in the container

The virt-launcher connects to the socket, receives the FD, and passes it to libvirt via `virDomainFDAssociate`.

## SELinux

The plugin handles SELinux automatically. When SELinux is enabled, it relabels temporary device nodes and sockets with `system_u:object_r:container_file_t:s0` so that FDs pass `security_file_receive()` checks when transferred to container processes.
