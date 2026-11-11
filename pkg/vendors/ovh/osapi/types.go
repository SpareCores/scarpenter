/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package osapi is the narrow, SDK-free view of the OpenStack APIs the OVH
// driver uses: the types of Nova, Neutron, Glance and Cinder resources it reads
// and writes, and the interfaces that the real client (package sdk, on
// gophercloud) and the in-memory fake (package fake) both implement. The driver
// and its tests never touch the SDK directly.
package osapi

import (
	"errors"
	"fmt"
	"time"
)

// APIError is the error of an OpenStack call that reached the service and got a
// non-success answer. OpenStack has no stable error codes for most failures, so
// the driver classifies by Status plus Message text (see the classification
// table in package ovh).
type APIError struct {
	// Status is the HTTP status.
	Status int
	// Message is the service's message (Nova: the "message" of the error body,
	// Neutron: "message" of NeutronError, with the "type" prefixed).
	Message string
	// Service is "nova", "neutron", "glance", "cinder" or "keystone".
	Service string
}

// Error implements error.
func (e *APIError) Error() string {
	return fmt.Sprintf("%s: HTTP %d: %s", e.Service, e.Status, e.Message)
}

// ErrNoEndpoint is returned by Provider.Clients for a region that has no
// endpoint of a needed service in the Keystone catalog.
var ErrNoEndpoint = errors.New("no endpoint for the region in the service catalog")

// Server status and task state values the driver relies on (Nova's).
const (
	StatusBuild     = "BUILD"
	StatusActive    = "ACTIVE"
	StatusError     = "ERROR"
	StatusShutoff   = "SHUTOFF"
	StatusDeleted   = "DELETED"
	TaskScheduling  = "scheduling"
	TaskSpawning    = "spawning"
	TaskDeleting    = "deleting"
	ImageActive     = "active"
	VisibilityPriv  = "private"
	VisibilityComm  = "community"
	ExternalNetwork = "Ext-Net"
)

// Flavor is a Nova flavor. IDs differ per region; the name is Keeper's API
// reference.
type Flavor struct {
	ID     string
	Name   string
	VCPUs  int64
	RAMMiB int64
	DiskGB int64
}

// Address is one address of a server.
type Address struct {
	Network string
	Addr    string
	Version int
}

// Fault is the failure information of a server in ERROR.
type Fault struct {
	Code    int
	Message string
}

// Server is a Nova server as the driver reads it.
type Server struct {
	ID        string
	Name      string
	Status    string
	TaskState string
	Created   time.Time
	// FlavorID is empty from Nova microversion 2.47, which embeds the flavor
	// without an ID; FlavorName is its name (original_name) and always set.
	FlavorID         string
	FlavorName       string
	ImageID          string
	AvailabilityZone string
	Metadata         map[string]string
	Tags             []string
	Addresses        []Address
	Fault            *Fault
	// Volumes are the IDs of the attached volumes.
	Volumes []string
	// SecurityGroups are the names of the security groups of the server (Nova
	// reports names).
	SecurityGroups []string
}

// ServerFilter narrows a server list server side.
type ServerFilter struct {
	// Name is Nova's name filter, a regular expression ("^name$" anchors).
	Name string
	// Tags requires all of the tags (Nova microversion 2.26).
	Tags []string
	// Status filters by Nova status.
	Status string
}

// BlockDevice is one block device mapping v2 of a server create.
type BlockDevice struct {
	// SourceType is image, blank, volume or snapshot; DestinationType is volume
	// or local.
	SourceType      string
	DestinationType string
	UUID            string
	BootIndex       int
	SizeGiB         int
	VolumeType      string
	// DeleteOnTermination makes the volume go with the server.
	DeleteOnTermination bool
}

// CreateServerRequest is the body of POST /servers (the fields the driver uses).
type CreateServerRequest struct {
	Name     string
	FlavorID string
	// ImageID is empty when the root is a block device mapping with boot index 0.
	ImageID string
	// NetworkIDs are Neutron network UUIDs.
	NetworkIDs []string
	// SecurityGroups are group names (Nova takes names).
	SecurityGroups []string
	// UserData is the raw data; the client base64-encodes it.
	UserData         []byte
	ConfigDrive      *bool
	AvailabilityZone string
	Metadata         map[string]string
	// Tags are Nova server tags (microversion 2.26).
	Tags         []string
	BlockDevices []BlockDevice
	// SchedulerGroup is the server group ID of the os:scheduler_hints group.
	SchedulerGroup string
}

// AvailabilityZone is a Nova availability zone.
type AvailabilityZone struct {
	Name      string
	Available bool
}

// Limits are Nova's absolute limits of the project in a region (RAM in MiB).
type Limits struct {
	MaxInstances, UsedInstances       int64
	MaxCores, UsedCores               int64
	MaxRAMMiB, UsedRAMMiB             int64
	MaxServerGroups, UsedServerGroups int64
	MaxSecurityGroups                 int64
	UsedSecurityGroups                int64
}

// ServerGroup is a Nova server group.
type ServerGroup struct {
	ID      string
	Name    string
	Policy  string
	Members []string
}

// Network is a Neutron network.
type Network struct {
	ID       string
	Name     string
	External bool
}

// SecurityRule is a Neutron security group rule.
type SecurityRule struct {
	ID            string
	Direction     string // ingress, egress
	EtherType     string // IPv4, IPv6
	Protocol      string
	PortMin       int
	PortMax       int
	RemoteCIDR    string
	RemoteGroupID string
}

// SecurityGroup is a Neutron security group.
type SecurityGroup struct {
	ID          string
	Name        string
	Description string
	Rules       []SecurityRule
	Tags        []string
}

// Image is a Glance image.
type Image struct {
	ID         string
	Name       string
	Status     string
	Visibility string
	Owner      string
	Tags       []string
	Properties map[string]string
	Created    time.Time
	SizeBytes  int64
	// VirtualSizeBytes is the image's virtual size, 0 when unknown.
	VirtualSizeBytes int64
}

// ImageFilter narrows an image list server side; empty fields do not filter.
type ImageFilter struct {
	Name       string
	Status     string
	Visibility string
	Owner      string
	// Tags requires all of the tags.
	Tags []string
}

// VolumeType is a Cinder volume type.
type VolumeType struct {
	ID   string
	Name string
}
