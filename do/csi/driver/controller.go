package driver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	csi "github.com/container-storage-interface/spec/lib/go/csi/v0"
	"github.com/digitalocean/godo"
)

const (
	_  = iota
	KB = 1 << (10 * iota)
	MB
	GB
	TB
)

const (
	defaultVolumeSizeInGB = 16 * GB

	defaultMinVolumeSizeInGB = 1 * GB
	defaultMaxVolumeSizeInGB = 16 * TB

	createdByDO = "Created by DigitalOcean CSI driver"
)

// CreateVolume creates a new volume from the given request. The function is
// idempotent.
func (d *Driver) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	volumeName := req.Name

	// get volume first, if it's created do no thing
	volumes, _, err := d.doClient.Storage.ListVolumes(ctx, &godo.ListVolumeParams{
		Region: d.region,
		Name:   volumeName,
	})
	if err != nil {
		return nil, err
	}

	// volume already exist, do not thing
	if len(volumes) != 0 {
		if len(volumes) > 1 {
			return nil, fmt.Errorf("fatal issue: duplicate volume %q exists", volumeName)
		}
		vol := volumes[0]

		// check if it was created by the CSI driver
		if vol.Description != createdByDO {
			return nil, fmt.Errorf("fatal issue: volume %q (%s) was not created by CSI",
				vol.Name, vol.Description)
		}

		return &csi.CreateVolumeResponse{
			Volume: &csi.Volume{
				Id:            vol.ID,
				CapacityBytes: vol.SizeGigaBytes * GB,
			},
		}, nil
	}

	size, err := extractStorage(req.CapacityRange)
	if err != nil {
		return nil, err
	}

	volumeReq := &godo.VolumeCreateRequest{
		Region:        d.nodeId,
		Name:          volumeName,
		Description:   createdByDO,
		SizeGigaBytes: size,
	}

	// TODO(arslan): Currently DO only supports SINGLE_NODE_WRITER mode. In the
	// future, if we support more modes, we need to make sure to create a
	// volume that aligns with the incoming capability. We need to make sure to
	// test req.VolumeCapabilities
	vol, _, err := d.doClient.Storage.CreateVolume(ctx, volumeReq)
	if err != nil {
		return nil, err
	}

	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			Id:            vol.ID,
			CapacityBytes: size * GB,
		},
	}, nil
}

// DeleteVolume deletes the given volume. The function is idempotent.
func (d *Driver) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	_, err := d.doClient.Storage.DeleteVolume(ctx, req.VolumeId)
	if err != nil {
		return nil, err
	}

	return &csi.DeleteVolumeResponse{}, nil
}

// ControllerPublishVolume attaches the given volume to the node
func (d *Driver) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	dropletID, err := strconv.Atoi(req.NodeId)
	if err != nil {
		return nil, fmt.Errorf("malformed nodeId %q detected: %s", req.NodeId, err)
	}

	// TODO(arslan): wait volume to attach
	_, resp, err := d.doClient.StorageActions.Attach(ctx, req.VolumeId, dropletID)
	if err != nil {
		// don't do anything if attached
		if resp.StatusCode == http.StatusUnprocessableEntity || strings.Contains(err.Error(), "This volume is already attached") {
			return &csi.ControllerPublishVolumeResponse{}, nil
		}

		return nil, err
	}

	return &csi.ControllerPublishVolumeResponse{}, nil
}

// ControllerUnpublishVolume deattaches the given volume from the node
func (d *Driver) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	dropletID, err := strconv.Atoi(req.NodeId)
	if err != nil {
		return nil, fmt.Errorf("malformed nodeId %q detected: %s", req.NodeId, err)
	}

	// TODO(arslan): wait volume to deattach
	_, resp, err := d.doClient.StorageActions.DetachByDropletID(ctx, req.NodeId, dropletID)
	if err != nil {
		if resp.StatusCode == http.StatusUnprocessableEntity || strings.Contains(err.Error(), "Attachment not found") {
			return &csi.ControllerUnpublishVolumeResponse{}, nil
		}
		return nil, err
	}
	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

// ValidateVolumeCapabilities checks whether the volume capabilities requested
// are supported.
func (d *Driver) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	var vcaps []*csi.VolumeCapability_AccessMode
	for _, mode := range []csi.VolumeCapability_AccessMode_Mode{
		// DO currently only support a single node to be attached to a single
		// node in read/write mode
		csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
	} {
		vcaps = append(vcaps, &csi.VolumeCapability_AccessMode{Mode: mode})
	}

	hasSupport := func(mode csi.VolumeCapability_AccessMode_Mode) bool {
		for _, m := range vcaps {
			if mode == m.Mode {
				return true
			}
		}
		return false
	}

	resp := &csi.ValidateVolumeCapabilitiesResponse{
		Supported: false,
	}

	for _, cap := range req.VolumeCapabilities {
		// cap.AccessMode.Mode
		if hasSupport(cap.AccessMode.Mode) {
			resp.Supported = true
		} else {
			// we need to make sure all capabilities are supported. Revert back
			// in case we have a cap that is supported, but is invalidated now
			resp.Supported = false
		}
	}

	return resp, nil
}

// ListVolumes returns a list of all requested volumes
func (d *Driver) ListVolumes(ctx context.Context, req *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {

	page, err := strconv.Atoi(req.StartingToken)
	if err != nil {
		return nil, err
	}

	listOpts := &godo.ListVolumeParams{
		ListOptions: &godo.ListOptions{
			PerPage: int(req.MaxEntries),
			Page:    page,
		},
		Region: d.region,
	}

	var volumes []godo.Volume
	var lastPage int
	for {
		vols, resp, err := d.doClient.Storage.ListVolumes(ctx, listOpts)
		if err != nil {
			return nil, err
		}

		for _, vol := range vols {
			volumes = append(volumes, vol)
		}

		if resp.Links == nil || resp.Links.IsLastPage() {
			page, err := resp.Links.CurrentPage()
			if err != nil {
				return nil, err
			}
			// save this for the response
			lastPage = page
			break
		}

		page, err := resp.Links.CurrentPage()
		if err != nil {
			return nil, err
		}

		listOpts.ListOptions.Page = page + 1
	}

	var entries []*csi.ListVolumesResponse_Entry
	for _, vol := range volumes {
		entries = append(entries, &csi.ListVolumesResponse_Entry{
			Volume: &csi.Volume{
				Id:            vol.ID,
				CapacityBytes: vol.SizeGigaBytes * GB,
			},
		})
	}

	// TODO(arslan): check that the NextToken logic works fine, might be racy
	return &csi.ListVolumesResponse{
		Entries:   entries,
		NextToken: strconv.Itoa(lastPage),
	}, nil
}

// GetCapacity returns the capacity of the storage pool
func (d *Driver) GetCapacity(ctx context.Context, req *csi.GetCapacityRequest) (*csi.GetCapacityResponse, error) {
	return nil, errors.New("not implemented")
}

// ControllerGetCapabilities returns the capabilities of the controller service.
func (d *Driver) ControllerGetCapabilities(ctx context.Context, req *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	newCap := func(cap csi.ControllerServiceCapability_RPC_Type) *csi.ControllerServiceCapability {
		return &csi.ControllerServiceCapability{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{
					Type: cap,
				},
			},
		}
	}

	// TODO(arslan): checkout if the capabilities are worth supporting
	var caps []*csi.ControllerServiceCapability
	for _, cap := range []csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
		csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
		csi.ControllerServiceCapability_RPC_LIST_VOLUMES,
		csi.ControllerServiceCapability_RPC_GET_CAPACITY,
	} {
		caps = append(caps, newCap(cap))
	}

	return &csi.ControllerGetCapabilitiesResponse{
		Capabilities: caps,
	}, nil
}

// extractStorage extracts the storage size in GB from the given capacity
// range. If the capacity range is not satisfied it returns the default volume
// size.
func extractStorage(capRange *csi.CapacityRange) (int64, error) {
	if capRange == nil {
		return defaultVolumeSizeInGB, nil
	}

	minSize := (capRange.RequiredBytes) / GB
	maxSize := (capRange.LimitBytes) / GB

	if minSize == maxSize {
		return minSize, nil
	}

	return 0, errors.New("requiredBytes and LimitBytes are not the same")
}
