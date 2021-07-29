// Copyright 2017 Google Inc. All Rights Reserved.
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

// Handler for CRI-O containers.
package crio

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/cadvisor/container"
	"github.com/google/cadvisor/container/common"
	containerlibcontainer "github.com/google/cadvisor/container/libcontainer"
	"github.com/google/cadvisor/fs"
	info "github.com/google/cadvisor/info/v1"
	"github.com/opencontainers/runc/libcontainer/cgroups"
)

type crioContainerHandler struct {
	client CrioClient
	name   string

	machineInfoFactory info.MachineInfoFactory

	// Absolute path to the cgroup hierarchies of this container.
	// (e.g.: "cpu" -> "/sys/fs/cgroup/cpu/test")
	cgroupPaths map[string]string

	// the CRI-O storage driver
	storageDriver    storageDriver
	fsInfo           fs.FsInfo
	rootfsStorageDir string

	// Metadata associated with the container.
	envs   map[string]string
	labels map[string]string

	// TODO
	// crio version handling...

	// Image name used for this container.
	image string

	// The network mode of the container
	// TODO

	// Filesystem handler.
	fsHandler common.FsHandler

	// The IP address of the container
	ipAddress string

	includedMetrics container.MetricSet

	reference info.ContainerReference

	libcontainerHandler *containerlibcontainer.Handler
	cgroupManager       cgroups.Manager
	rootFs              string
	pidKnown            bool
}

var _ container.ContainerHandler = &crioContainerHandler{}

// newCrioContainerHandler returns a new container.ContainerHandler
func newCrioContainerHandler(
	client CrioClient,
	name string,
	machineInfoFactory info.MachineInfoFactory,
	fsInfo fs.FsInfo,
	storageDriver storageDriver,
	storageDir string,
	cgroupSubsystems *containerlibcontainer.CgroupSubsystems,
	inHostNamespace bool,
	metadataEnvs []string,
	includedMetrics container.MetricSet,
) (container.ContainerHandler, error) {
	// Create the cgroup paths.
	cgroupPaths := common.MakeCgroupPaths(cgroupSubsystems.MountPoints, name)

	// Generate the equivalent cgroup manager for this container.
	cgroupManager, err := containerlibcontainer.NewCgroupManager(name, cgroupPaths)
	if err != nil {
		return nil, err
	}

	rootFs := "/"
	if !inHostNamespace {
		rootFs = "/rootfs"
	}

	id := ContainerNameToCrioId(name)
	pidKnown := true

	cInfo, err := client.ContainerInfo(id)
	if err != nil {
		return nil, err
	}
	if cInfo.Pid == 0 {
		// If pid is not known yet, network related stats can not be retrieved by the
		// libcontainer handler GetStats().  In this case, the crio handler GetStats()
		// will reattempt to get the pid and, if now known, will construct the libcontainer
		// handler.  This libcontainer handler is then cached and reused without additional
		// calls to crio.
		pidKnown = false
	}

	// passed to fs handler below ...
	// XXX: this is using the full container logpath, as constructed by the CRI
	// /var/log/pods/<pod_uuid>/container_instance.log
	// It's not actually a log dir, as the CRI doesn't have per-container dirs
	// under /var/log/pods/<pod_uuid>/
	// We can't use /var/log/pods/<pod_uuid>/ to count per-container log usage.
	// We use the container log file directly.
	storageLogDir := cInfo.LogPath

	// Determine the rootfs storage dir
	rootfsStorageDir := cInfo.Root
	// TODO(runcom): CRI-O doesn't strip /merged but we need to in order to
	// get device ID from root, otherwise, it's going to error out as overlay
	// mounts doesn't have fixed dev ids.
	rootfsStorageDir = strings.TrimSuffix(rootfsStorageDir, "/merged")
	switch storageDriver {
	case overlayStorageDriver, overlay2StorageDriver:
		// overlay and overlay2 driver are the same "overlay2" driver so treat
		// them the same.
		rootfsStorageDir = filepath.Join(rootfsStorageDir, "diff")
	}

	containerReference := info.ContainerReference{
		Id:        id,
		Name:      name,
		Aliases:   []string{cInfo.Name, id},
		Namespace: CrioNamespace,
	}

	libcontainerHandler := containerlibcontainer.NewHandler(cgroupManager, rootFs, cInfo.Pid, includedMetrics)

	// TODO: extract object mother method
	handler := &crioContainerHandler{
		client:              client,
		name:                name,
		machineInfoFactory:  machineInfoFactory,
		cgroupPaths:         cgroupPaths,
		storageDriver:       storageDriver,
		fsInfo:              fsInfo,
		rootfsStorageDir:    rootfsStorageDir,
		envs:                make(map[string]string),
		labels:              cInfo.Labels,
		includedMetrics:     includedMetrics,
		reference:           containerReference,
		libcontainerHandler: libcontainerHandler,
		cgroupManager:       cgroupManager,
		rootFs:              rootFs,
		pidKnown:            pidKnown,
	}

	handler.image = cInfo.Image
	// TODO: we wantd to know graph driver DeviceId (dont think this is needed now)

	// ignore err and get zero as default, this happens with sandboxes, not sure why...
	// kube isn't sending restart count in labels for sandboxes.
	restartCount, _ := strconv.Atoi(cInfo.Annotations["io.kubernetes.container.restartCount"])
	// Only adds restartcount label if it's greater than 0
	if restartCount > 0 {
		handler.labels["restartcount"] = strconv.Itoa(restartCount)
	}

	handler.ipAddress = cInfo.IP

	// we optionally collect disk usage metrics
	if includedMetrics.Has(container.DiskUsageMetrics) {
		handler.fsHandler = common.NewFsHandler(common.DefaultPeriod, common.NewGeneralFsUsageProvider(
			fsInfo, rootfsStorageDir, storageLogDir))
	}
	// TODO for env vars we wanted to show from container.Config.Env from whitelist
	//for _, exposedEnv := range metadataEnvs {
	//klog.V(4).Infof("TODO env whitelist: %v", exposedEnv)
	//}

	return handler, nil
}

func (h *crioContainerHandler) Start() {
	if h.fsHandler != nil {
		h.fsHandler.Start()
	}
}

func (h *crioContainerHandler) Cleanup() {
	if h.fsHandler != nil {
		h.fsHandler.Stop()
	}
}

func (h *crioContainerHandler) ContainerReference() (info.ContainerReference, error) {
	return h.reference, nil
}

func (h *crioContainerHandler) needNet() bool {
	if h.includedMetrics.Has(container.NetworkUsageMetrics) {
		return h.labels["io.kubernetes.container.name"] == "POD"
	}
	return false
}

func (h *crioContainerHandler) GetSpec() (info.ContainerSpec, error) {
	hasFilesystem := h.includedMetrics.Has(container.DiskUsageMetrics)
	spec, err := common.GetSpec(h.cgroupPaths, h.machineInfoFactory, h.needNet(), hasFilesystem)

	spec.Labels = h.labels
	spec.Envs = h.envs
	spec.Image = h.image

	return spec, err
}

func (h *crioContainerHandler) getFsStats(stats *info.ContainerStats) error {
	mi, err := h.machineInfoFactory.GetMachineInfo()
	if err != nil {
		return err
	}

	if h.includedMetrics.Has(container.DiskIOMetrics) {
		common.AssignDeviceNamesToDiskStats((*common.MachineInfoNamer)(mi), &stats.DiskIo)
	}

	if !h.includedMetrics.Has(container.DiskUsageMetrics) {
		return nil
	}
	var device string
	switch h.storageDriver {
	case overlay2StorageDriver, overlayStorageDriver:
		deviceInfo, err := h.fsInfo.GetDirFsDevice(h.rootfsStorageDir)
		if err != nil {
			return fmt.Errorf("unable to determine device info for dir: %v: %v", h.rootfsStorageDir, err)
		}
		device = deviceInfo.Device
	default:
		return nil
	}

	var (
		limit  uint64
		fsType string
	)

	// crio does not impose any filesystem limits for containers. So use capacity as limit.
	for _, fs := range mi.Filesystems {
		if fs.Device == device {
			limit = fs.Capacity
			fsType = fs.Type
			break
		}
	}

	if fsType == "" {
		return fmt.Errorf("unable to determine fs type for device: %v", device)
	}
	fsStat := info.FsStats{Device: device, Type: fsType, Limit: limit}
	usage := h.fsHandler.Usage()
	fsStat.BaseUsage = usage.BaseUsageBytes
	fsStat.Usage = usage.TotalUsageBytes
	fsStat.Inodes = usage.InodeUsage

	stats.Filesystem = append(stats.Filesystem, fsStat)

	return nil
}

func (h *crioContainerHandler) getLibcontainerHandler() *containerlibcontainer.Handler {
	if h.pidKnown {
		return h.libcontainerHandler
	}

	id := ContainerNameToCrioId(h.name)

	cInfo, err := h.client.ContainerInfo(id)
	if err != nil || cInfo.Pid == 0 {
		return h.libcontainerHandler
	}

	h.pidKnown = true
	h.libcontainerHandler = containerlibcontainer.NewHandler(h.cgroupManager, h.rootFs, cInfo.Pid, h.includedMetrics)

	return h.libcontainerHandler
}

func (h *crioContainerHandler) GetStats() (*info.ContainerStats, error) {
	libcontainerHandler := h.getLibcontainerHandler()
	stats, err := libcontainerHandler.GetStats()
	if err != nil {
		return stats, err
	}
	// Clean up stats for containers that don't have their own network - this
	// includes containers running in Kubernetes pods that use the network of the
	// infrastructure container. This stops metrics being reported multiple times
	// for each container in a pod.
	if !h.needNet() {
		stats.Network = info.NetworkStats{}
	}

	// Get filesystem stats.
	err = h.getFsStats(stats)
	if err != nil {
		return stats, err
	}

	return stats, nil
}

func (h *crioContainerHandler) ListContainers(listType container.ListType) ([]info.ContainerReference, error) {
	// No-op for Docker driver.
	return []info.ContainerReference{}, nil
}

func (h *crioContainerHandler) GetCgroupPath(resource string) (string, error) {
	path, ok := h.cgroupPaths[resource]
	if !ok {
		return "", fmt.Errorf("could not find path for resource %q for container %q", resource, h.reference.Name)
	}
	return path, nil
}

func (h *crioContainerHandler) GetContainerLabels() map[string]string {
	return h.labels
}

func (h *crioContainerHandler) GetContainerIPAddress() string {
	return h.ipAddress
}

func (h *crioContainerHandler) ListProcesses(listType container.ListType) ([]int, error) {
	return h.libcontainerHandler.GetProcesses()
}

func (h *crioContainerHandler) Exists() bool {
	return common.CgroupExists(h.cgroupPaths)
}

func (h *crioContainerHandler) Type() container.ContainerType {
	return container.ContainerTypeCrio
}
