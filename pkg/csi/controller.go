/*
Copyright 2023 sergelogvinov.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package csi

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	pxapi "github.com/Telmate/proxmox-api-go/proxmox"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/kubernetes-csi/csi-lib-utils/protosanitizer"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	proxmox "github.com/sergelogvinov/proxmox-csi-plugin/pkg/proxmox"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/cloud-provider-openstack/pkg/util"
	"k8s.io/klog"
)

const (
	vmID = 9999
)

var controllerCaps = []csi.ControllerServiceCapability_RPC_Type{
	csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
	csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
	csi.ControllerServiceCapability_RPC_GET_CAPACITY,
	csi.ControllerServiceCapability_RPC_EXPAND_VOLUME,
	csi.ControllerServiceCapability_RPC_GET_VOLUME,
}

type controllerService struct {
	cluster     proxmox.Client
	volumeLocks sync.Mutex
}

// NewControllerService returns a new controller service
func NewControllerService(cloudConfig string) (*controllerService, error) {
	cfg, err := proxmox.ReadFromFileCloudConfig(cloudConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to read config: %v", err)
	}

	cluster, err := proxmox.NewClient(&cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create proxmox client: %v", err)
	}

	return &controllerService{
		cluster: *cluster,
	}, nil
}

// CreateVolume creates a volume
//
//lint:gocyclo
func (d *controllerService) CreateVolume(_ context.Context, request *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	klog.V(4).Infof("CreateVolume: called with args %+v", protosanitizer.StripSecrets(*request))

	volName := request.GetName()
	if len(volName) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume Name cannot be empty")
	}

	volCapabilities := request.GetVolumeCapabilities()
	if volCapabilities == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume Capabilities cannot be empty")
	}

	// Volume Size - Default is 10 GiB
	volSizeBytes := int64(DefaultVolumeSize * 1024 * 1024 * 1024)
	if request.GetCapacityRange() != nil {
		volSizeBytes = request.GetCapacityRange().GetRequiredBytes()
	}

	volSizeGB := int(util.RoundUpSize(volSizeBytes, 1024*1024*1024))

	volCtx := make(map[string]string)
	for k, v := range request.GetParameters() {
		volCtx[k] = v
	}

	accessibleTopology := request.GetAccessibilityRequirements().GetPreferred()

	var (
		region string
		zone   string
		node   string
	)

	if len(accessibleTopology) != 0 {
		for _, t := range accessibleTopology {
			if t.GetSegments()[corev1.LabelTopologyRegion] != "" {
				region = t.GetSegments()[corev1.LabelTopologyRegion]
			}

			if t.GetSegments()[corev1.LabelTopologyZone] != "" {
				zone = t.GetSegments()[corev1.LabelTopologyZone]
			}

			if t.GetSegments()[corev1.LabelHostname] != "" {
				node = t.GetSegments()[corev1.LabelHostname]
			}

			if region != "" && (zone != "" || node != "") {
				break
			}
		}
	}

	if region == "" || zone == "" {
		klog.Errorf("CreateVolume: region or zone is empty: accessibleTopology=%+v", accessibleTopology)

		return nil, status.Error(codes.InvalidArgument, "region or zone is empty")
	}

	cl, err := d.cluster.GetProxmoxCluster(region)
	if err != nil {
		klog.Errorf("failed to get proxmox cluster: %v", err)

		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	volumeName := fmt.Sprintf("vm-%d-%s", vmID, volName)
	vol := proxmox.NewVolume(region, zone, volCtx[StorageIDKey], volumeName)

	diskParams := map[string]interface{}{
		"vmid":     vmID,
		"filename": volumeName,
		"size":     fmt.Sprintf("%dG", volSizeGB),
	}

	err = cl.CreateVMDisk(vol.Node(), vol.Storage(), fmt.Sprintf("%s:%s", volCtx[StorageIDKey], volumeName), diskParams)
	if err != nil {
		klog.Errorf("failed to create vm disk: %v", err)

		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	volume := csi.Volume{
		VolumeId:      vol.VolumeID(),
		VolumeContext: volCtx,
		ContentSource: request.GetVolumeContentSource(),
		CapacityBytes: int64(volSizeGB * 1024 * 1024 * 1024),
		AccessibleTopology: []*csi.Topology{
			{
				Segments: map[string]string{
					corev1.LabelTopologyRegion: region,
					corev1.LabelTopologyZone:   zone,
				},
			},
		},
	}

	return &csi.CreateVolumeResponse{Volume: &volume}, nil
}

// DeleteVolume deletes a volume.
func (d *controllerService) DeleteVolume(ctx context.Context, request *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	klog.V(4).Infof("DeleteVolume: called with args %+v", protosanitizer.StripSecrets(*request))

	volumeID := request.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "DeleteVolume Volume ID must be provided")
	}

	vol, err := proxmox.NewVolumeFromVolumeID(volumeID)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	cl, err := d.cluster.GetProxmoxCluster(vol.Cluster())
	if err != nil {
		klog.Errorf("failed to get proxmox cluster: %v", err)

		return nil, status.Error(codes.Internal, err.Error())
	}

	exist, err := isPvcExists(cl, volumeID)
	if err != nil {
		klog.Errorf("failed to check if pvc exists: %v", err)

		return nil, status.Error(codes.Internal, err.Error())
	}

	if !exist {
		klog.V(3).Infof("DeleteVolume: volume %s is already deleted.", volumeID)

		return &csi.DeleteVolumeResponse{}, nil
	}

	vmr := pxapi.NewVmRef(vmID)
	vmr.SetNode(vol.Node())
	vmr.SetVmType("qemu")

	_, err = cl.DeleteVolume(vmr, vol.Storage(), vol.PVC())
	if err != nil {
		klog.Errorf("failed to delete volume: %v", err)

		return nil, status.Error(codes.Internal, err.Error())
	}

	klog.V(4).Infof("DeleteVolume: successfully deleted volume %s", vol.PVC())

	return &csi.DeleteVolumeResponse{}, nil
}

// ControllerGetCapabilities get controller capabilities.
func (d *controllerService) ControllerGetCapabilities(ctx context.Context, request *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	klog.V(4).Infof("ControllerGetCapabilities: called with args %+v", protosanitizer.StripSecrets(*request))

	caps := []*csi.ControllerServiceCapability{}

	for _, cap := range controllerCaps {
		c := &csi.ControllerServiceCapability{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{
					Type: cap,
				},
			},
		}
		caps = append(caps, c)
	}

	return &csi.ControllerGetCapabilitiesResponse{Capabilities: caps}, nil
}

// ControllerPublishVolume publish a volume
func (d *controllerService) ControllerPublishVolume(ctx context.Context, request *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	klog.V(4).Infof("ControllerPublishVolume: called with args %+v", protosanitizer.StripSecrets(*request))

	volumeID := request.GetVolumeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "VolumeID must be provided")
	}

	nodeID := request.GetNodeId()
	if nodeID == "" {
		return nil, status.Error(codes.InvalidArgument, "NodeID must be provided")
	}

	if request.VolumeCapability == nil {
		return nil, status.Error(codes.InvalidArgument, "VolumeCapability must be provided")
	}

	region, _, storageName, pvc, err := parseVolumeID(volumeID)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	cl, err := d.cluster.GetProxmoxCluster(region)
	if err != nil {
		klog.Errorf("failed to get proxmox cluster: %v", err)

		return nil, status.Error(codes.Internal, err.Error())
	}

	vm, err := cl.GetVmRefByName(nodeID)
	if err != nil {
		klog.Errorf("failed to get vm ref by name: %v", err)

		return nil, status.Error(codes.Internal, err.Error())
	}

	options := map[string]string{
		"backup":   "0",
		"iothread": "1",
	}

	if request.Readonly {
		options["ro"] = "1"
	}

	d.volumeLocks.Lock()
	defer d.volumeLocks.Unlock()

	pvInfo, err := attachVolume(cl, vm, storageName, pvc, options)
	if err != nil {
		klog.Errorf("failed to attach volume: %v", err)

		return nil, status.Error(codes.Internal, err.Error())
	}

	return &csi.ControllerPublishVolumeResponse{PublishContext: pvInfo}, nil
}

// ControllerUnpublishVolume unpublish a volume
func (d *controllerService) ControllerUnpublishVolume(ctx context.Context, request *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	klog.V(4).Infof("ControllerUnpublishVolume: called with args %+v", protosanitizer.StripSecrets(*request))

	volumeID := request.GetVolumeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "VolumeID must be provided")
	}

	nodeID := request.GetNodeId()
	if nodeID == "" {
		return nil, status.Error(codes.InvalidArgument, "NodeID must be provided")
	}

	vol, err := proxmox.NewVolumeFromVolumeID(volumeID)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	cl, err := d.cluster.GetProxmoxCluster(vol.Cluster())
	if err != nil {
		klog.Errorf("failed to get proxmox cluster: %v", err)

		return nil, status.Error(codes.Internal, err.Error())
	}

	vm, err := cl.GetVmRefByName(nodeID)
	if err != nil {
		klog.Errorf("failed to get vm ref by name: %v", err)

		return nil, status.Error(codes.Internal, err.Error())
	}

	if detachVolume(cl, vm, vol.PVC()) != nil {
		klog.Errorf("failed to detachVolume vm config: %v, vmr=%+v", err, vm)

		return nil, status.Error(codes.Internal, err.Error())
	}

	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

// ValidateVolumeCapabilities validate volume capabilities
func (d *controllerService) ValidateVolumeCapabilities(ctx context.Context, request *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	klog.V(4).Infof("ValidateVolumeCapabilities: called with args %+v", protosanitizer.StripSecrets(*request))

	return nil, status.Error(codes.Unimplemented, "")
}

// ListVolumes list volumes
func (d *controllerService) ListVolumes(ctx context.Context, request *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {
	klog.V(4).Infof("ListVolumes: called with args %+v", protosanitizer.StripSecrets(*request))

	return nil, status.Error(codes.Unimplemented, "")
}

// GetCapacity get capacity
func (d *controllerService) GetCapacity(ctx context.Context, request *csi.GetCapacityRequest) (*csi.GetCapacityResponse, error) {
	klog.V(4).Infof("GetCapacity: called with args %+v", protosanitizer.StripSecrets(*request))

	topology := request.GetAccessibleTopology()
	if topology != nil {
		region := topology.Segments[corev1.LabelTopologyRegion]
		zone := topology.Segments[corev1.LabelTopologyZone]
		storageName := request.GetParameters()[StorageIDKey]

		if region == "" || zone == "" || storageName == "" {
			return nil, status.Error(codes.InvalidArgument, "region, zone and storageName must be provided")
		}

		klog.V(4).Infof("GetCapacity: region=%s, zone=%s, storageName=%s", region, zone, storageName)

		cl, err := d.cluster.GetProxmoxCluster(region)
		if err != nil {
			klog.Errorf("failed to get proxmox cluster: %v", err)

			return nil, status.Error(codes.Internal, err.Error())
		}

		vmr := pxapi.NewVmRef(vmID)
		vmr.SetNode(zone)
		vmr.SetVmType("qemu")

		availableCapacity := int64(0)

		storage, err := cl.GetStorageStatus(vmr, storageName)
		if err != nil {
			klog.Errorf("GetCapacity: failed to get storage status: %v", err)

			if !strings.Contains(err.Error(), "Parameter verification failed") {
				return nil, status.Error(codes.InvalidArgument, err.Error())
			}
		} else {
			availableCapacity = int64(storage["avail"].(float64))
		}

		return &csi.GetCapacityResponse{
			// MinimumVolumeSize: MinVolumeSize * 1024 * 1024 * 1024,
			AvailableCapacity: availableCapacity,
		}, nil
	}

	return nil, status.Error(codes.InvalidArgument, "no topology specified")
}

// CreateSnapshot create a snapshot
func (d *controllerService) CreateSnapshot(ctx context.Context, request *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	klog.V(4).Infof("CreateSnapshot: called with args %+v", protosanitizer.StripSecrets(*request))

	return nil, status.Error(codes.Unimplemented, "")
}

// DeleteSnapshot delete a snapshot
func (d *controllerService) DeleteSnapshot(ctx context.Context, request *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	klog.V(4).Infof("DeleteSnapshot: called with args %+v", protosanitizer.StripSecrets(*request))

	return nil, status.Error(codes.Unimplemented, "")
}

// ListSnapshots list snapshots
func (d *controllerService) ListSnapshots(ctx context.Context, request *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
	klog.V(4).Infof("ListSnapshots: called with args %+v", protosanitizer.StripSecrets(*request))

	return nil, status.Error(codes.Unimplemented, "")
}

// ControllerExpandVolume expand a volume
func (d *controllerService) ControllerExpandVolume(ctx context.Context, request *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	klog.V(4).Infof("ControllerExpandVolume: called with args %+v", protosanitizer.StripSecrets(*request))

	volumeID := request.GetVolumeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "VolumeID must be provided")
	}

	capacityRange := request.GetCapacityRange()
	if capacityRange == nil {
		return nil, status.Error(codes.InvalidArgument, "CapacityRange must be provided")
	}

	volSizeBytes := request.GetCapacityRange().GetRequiredBytes()
	volSizeGB := int(util.RoundUpSize(volSizeBytes, 1024*1024*1024))
	maxVolSize := capacityRange.GetLimitBytes()

	if maxVolSize > 0 && maxVolSize < volSizeBytes {
		return nil, status.Error(codes.OutOfRange, "After round-up, volume size exceeds the limit specified")
	}

	klog.V(4).Infof("ControllerExpandVolume resized volume %v to size %vG", volumeID, volSizeGB)

	region, zone, _, pvc, err := parseVolumeID(volumeID)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	cl, err := d.cluster.GetProxmoxCluster(region)
	if err != nil {
		klog.Errorf("failed to get proxmox cluster: %v", err)

		return nil, status.Error(codes.Internal, err.Error())
	}

	exist, err := isPvcExists(cl, volumeID)
	if err != nil {
		klog.Errorf("failed to check if pvc exists: %v", err)

		return nil, status.Error(codes.Internal, err.Error())
	}

	if !exist {
		klog.Errorf("volume %s not found", volumeID)

		return &csi.ControllerExpandVolumeResponse{}, nil
	}

	vmlist, err := cl.GetVmList()
	if err != nil {
		klog.Errorf("failed to get vm list: %v", err)

		return nil, status.Error(codes.Internal, err.Error())
	}

	vms, ok := vmlist["data"].([]interface{})
	if !ok {
		err = fmt.Errorf("failed to cast response to list, vmlist: %v", vmlist)

		return nil, status.Error(codes.Internal, err.Error())
	}

	for vmii := range vms {
		vm, ok := vms[vmii].(map[string]interface{})
		if !ok {
			return nil, status.Errorf(codes.Internal, "failed to cast response to map, vm: %v", vm)
		}

		if vm["node"].(string) == zone {
			vmID := int(vm["vmid"].(float64))

			vmr := pxapi.NewVmRef(vmID)
			vmr.SetNode(zone)
			vmr.SetVmType("qemu")

			config, err := cl.GetVmConfig(vmr)
			if err != nil {
				klog.Errorf("failed to get vm config: %v", err)

				return nil, status.Error(codes.Internal, err.Error())
			}

			lun, exist := isVolumeAttached(config, pvc)
			if !exist {
				continue
			}

			device := "scsi" + strconv.Itoa(lun)

			_, err = cl.ResizeQemuDiskRaw(vmr, device, fmt.Sprintf("%dG", volSizeGB))
			if err != nil {
				klog.Errorf("failed to resize vm disk: %s, %v", pvc, err)

				return nil, status.Error(codes.Internal, err.Error())
			}

			return &csi.ControllerExpandVolumeResponse{
				CapacityBytes:         volSizeBytes,
				NodeExpansionRequired: true,
			}, nil
		}
	}

	klog.Errorf("cannot resize unpublished volume %s", pvc)

	return &csi.ControllerExpandVolumeResponse{}, nil
}

// ControllerGetVolume get a volume
func (d *controllerService) ControllerGetVolume(ctx context.Context, request *csi.ControllerGetVolumeRequest) (*csi.ControllerGetVolumeResponse, error) {
	klog.V(4).Infof("ControllerGetVolume: called with args %+v", protosanitizer.StripSecrets(*request))

	return nil, status.Error(codes.Unimplemented, "")
}
