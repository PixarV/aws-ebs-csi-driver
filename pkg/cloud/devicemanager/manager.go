/*
Copyright 2019 The Kubernetes Authors.

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

package devicemanager

import (
	"fmt"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"k8s.io/klog"
)

const devPreffix = "/dev/disk/by-id/virtio-"

type Device struct {
	Instance          *ec2.Instance
	Path              string
	VolumeID          string
	IsAlreadyAssigned bool

	isTainted   bool
	releaseFunc func() error
}

func (d *Device) Release(force bool) {
	if !d.isTainted || force {
		if err := d.releaseFunc(); err != nil {
			klog.Errorf("Error releasing device: %v", err)
		}
	}
}

// Taint marks the device as no longer reusable
func (d *Device) Taint() {
	d.isTainted = true
}

type DeviceManager interface {
	// NewDevice retrieves the device if the device is already assigned.
	// Otherwise it creates a new device with next available device name
	// and mark it as unassigned device.
	NewDevice(instance *ec2.Instance, volumeID string) (device *Device, err error)

	// GetDevice returns the device already assigned to the volume.
	GetDevice(instance *ec2.Instance, volumeID string) (device *Device, err error)
}

type deviceManager struct {
	// We keep an active list of devices we have assigned but not yet
	// attached, to avoid a race condition where we assign a device mapping
	// and then get a second request before we attach the volume.
	mux      sync.Mutex
	inFlight inFlightAttaching
}

var _ DeviceManager = &deviceManager{}

// inFlightAttaching represents the device names being currently attached to nodes.
// A valid pseudo-representation of it would be {"nodeID": {"deviceName: "volumeID"}}.
type inFlightAttaching map[string]map[string]string

func (i inFlightAttaching) Add(nodeID, volumeID, name string) {
	attaching := i[nodeID]
	if attaching == nil {
		attaching = make(map[string]string)
		i[nodeID] = attaching
	}
	attaching[name] = volumeID
}

func (i inFlightAttaching) Del(nodeID, name string) {
	delete(i[nodeID], name)
}

func (i inFlightAttaching) GetNames(nodeID string) map[string]string {
	return i[nodeID]
}

func (i inFlightAttaching) GetVolume(nodeID, name string) string {
	return i[nodeID][name]
}

func NewDeviceManager() DeviceManager {
	return &deviceManager{
		inFlight: make(inFlightAttaching),
	}
}

func (d *deviceManager) NewDevice(instance *ec2.Instance, volumeID string) (*Device, error) {
	d.mux.Lock()
	defer d.mux.Unlock()

	if instance == nil {
		return nil, fmt.Errorf("instance is nil")
	}

	// Get device names being attached and already attached to this instance
	inUse := d.getVolumeIdsInUse(instance)

	// Check if this volume is already assigned a device on this machine
	if path := d.getPath(inUse, volumeID); path != "" {
		return d.newBlockDevice(instance, volumeID, path, true), nil
	}

	nodeID, err := getInstanceID(instance)
	if err != nil {
		return nil, err
	}

	// Add the chosen device and volume to the "attachments in progress" map
	d.inFlight.Add(nodeID, volumeID, volumeID)

	return d.newBlockDevice(instance, volumeID, devPreffix+volumeID, false), nil
}

func (d *deviceManager) GetDevice(instance *ec2.Instance, volumeID string) (*Device, error) {
	d.mux.Lock()
	defer d.mux.Unlock()

	inUse := d.getVolumeIdsInUse(instance)

	if path := d.getPath(inUse, volumeID); path != "" {
		return d.newBlockDevice(instance, volumeID, path, true), nil
	}

	return d.newBlockDevice(instance, volumeID, "", false), nil
}

func (d *deviceManager) newBlockDevice(instance *ec2.Instance, volumeID string, path string, isAlreadyAssigned bool) *Device {
	device := &Device{
		Instance:          instance,
		Path:              path,
		VolumeID:          volumeID,
		IsAlreadyAssigned: isAlreadyAssigned,

		isTainted: false,
	}
	device.releaseFunc = func() error {
		return d.release(device)
	}
	return device
}

func (d *deviceManager) release(device *Device) error {
	nodeID, err := getInstanceID(device.Instance)
	if err != nil {
		return err
	}

	d.mux.Lock()
	defer d.mux.Unlock()

	existingVolumeID := d.inFlight.GetVolume(nodeID, device.VolumeID)
	if len(existingVolumeID) == 0 {
		// Attaching is not in progress, so there's nothing to release
		return nil
	}

	if device.VolumeID != existingVolumeID {
		// This actually can happen, because GetNext combines the inFlightAttaching map with the volumes
		// attached to the instance (as reported by the EC2 API).  So if release comes after
		// a 10 second poll delay, we might as well have had a concurrent request to allocate a mountpoint,
		// which because we allocate sequentially is very likely to get the immediately freed volume.
		return fmt.Errorf("release on device %q assigned to different volume: %q vs %q", device.Path, device.VolumeID, existingVolumeID)
	}

	klog.V(5).Infof("[Debug] Releasing in-process attachment entry: %v -> volume %s", device.Path, device.VolumeID)
	d.inFlight.Del(nodeID, device.VolumeID)

	return nil
}

// getVolumeIdsInUse returns the device to volume ID mapping
// the mapping includes both already attached and being attached volumes
func (d *deviceManager) getVolumeIdsInUse(instance *ec2.Instance) []string {
	nodeID := aws.StringValue(instance.InstanceId)
	var inUse []string
	for _, blockDevice := range instance.BlockDeviceMappings {
		if blockDevice.Ebs == nil {
			continue
		}

		name := aws.StringValue(blockDevice.DeviceName)
		// trim device prefix from name
		name = strings.TrimPrefix(name, devPreffix)

		inUse = append(inUse, *blockDevice.Ebs.VolumeId)
	}

	for _, volumeID := range d.inFlight.GetNames(nodeID) {
		inUse = append(inUse, volumeID)
	}

	return inUse
}

func (d *deviceManager) getPath(inUse []string, volumeID string) string {
	for _, volID := range inUse {
		if volumeID == volID {
			return devPreffix + volumeID
		}
	}
	return ""
}

func getInstanceID(instance *ec2.Instance) (string, error) {
	if instance == nil {
		return "", fmt.Errorf("can't get ID from a nil instance")
	}
	return aws.StringValue(instance.InstanceId), nil
}
