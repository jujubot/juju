// Copyright 2018 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package common

import (
	"context"

	ociCore "github.com/oracle/oci-go-sdk/core"
	ociIdentity "github.com/oracle/oci-go-sdk/identity"
)

type OCIIdentityClient interface {
	ListAvailabilityDomains(ctx context.Context, request ociIdentity.ListAvailabilityDomainsRequest) (response ociIdentity.ListAvailabilityDomainsResponse, err error)
}

type OCIComputeClient interface {
	ListVnicAttachments(ctx context.Context, request ociCore.ListVnicAttachmentsRequest) (response ociCore.ListVnicAttachmentsResponse, err error)
	TerminateInstance(ctx context.Context, request ociCore.TerminateInstanceRequest) (response ociCore.TerminateInstanceResponse, err error)
	GetInstance(ctx context.Context, request ociCore.GetInstanceRequest) (response ociCore.GetInstanceResponse, err error)
	LaunchInstance(ctx context.Context, request ociCore.LaunchInstanceRequest) (response ociCore.LaunchInstanceResponse, err error)
	ListInstances(ctx context.Context, request ociCore.ListInstancesRequest) (response ociCore.ListInstancesResponse, err error)
	ListShapes(ctx context.Context, request ociCore.ListShapesRequest) (response ociCore.ListShapesResponse, err error)
	ListImages(ctx context.Context, request ociCore.ListImagesRequest) (response ociCore.ListImagesResponse, err error)

	ListVolumeAttachments(ctx context.Context, request ociCore.ListVolumeAttachmentsRequest) (response ociCore.ListVolumeAttachmentsResponse, err error)
	GetVolumeAttachment(ctx context.Context, request ociCore.GetVolumeAttachmentRequest) (response ociCore.GetVolumeAttachmentResponse, err error)
	DetachVolume(ctx context.Context, request ociCore.DetachVolumeRequest) (response ociCore.DetachVolumeResponse, err error)
	AttachVolume(ctx context.Context, request ociCore.AttachVolumeRequest) (response ociCore.AttachVolumeResponse, err error)
}

type OCINetworkingClient interface {
	CreateVcn(ctx context.Context, request ociCore.CreateVcnRequest) (response ociCore.CreateVcnResponse, err error)
	DeleteVcn(ctx context.Context, request ociCore.DeleteVcnRequest) (response ociCore.DeleteVcnResponse, err error)
	ListVcns(ctx context.Context, request ociCore.ListVcnsRequest) (response ociCore.ListVcnsResponse, err error)
	GetVcn(ctx context.Context, request ociCore.GetVcnRequest) (response ociCore.GetVcnResponse, err error)

	CreateSubnet(ctx context.Context, request ociCore.CreateSubnetRequest) (response ociCore.CreateSubnetResponse, err error)
	ListSubnets(ctx context.Context, request ociCore.ListSubnetsRequest) (response ociCore.ListSubnetsResponse, err error)
	DeleteSubnet(ctx context.Context, request ociCore.DeleteSubnetRequest) (response ociCore.DeleteSubnetResponse, err error)
	GetSubnet(ctx context.Context, request ociCore.GetSubnetRequest) (response ociCore.GetSubnetResponse, err error)

	CreateInternetGateway(ctx context.Context, request ociCore.CreateInternetGatewayRequest) (response ociCore.CreateInternetGatewayResponse, err error)
	GetInternetGateway(ctx context.Context, request ociCore.GetInternetGatewayRequest) (response ociCore.GetInternetGatewayResponse, err error)
	ListInternetGateways(ctx context.Context, request ociCore.ListInternetGatewaysRequest) (response ociCore.ListInternetGatewaysResponse, err error)
	DeleteInternetGateway(ctx context.Context, request ociCore.DeleteInternetGatewayRequest) (response ociCore.DeleteInternetGatewayResponse, err error)

	CreateRouteTable(ctx context.Context, request ociCore.CreateRouteTableRequest) (response ociCore.CreateRouteTableResponse, err error)
	GetRouteTable(ctx context.Context, request ociCore.GetRouteTableRequest) (response ociCore.GetRouteTableResponse, err error)
	DeleteRouteTable(ctx context.Context, request ociCore.DeleteRouteTableRequest) (response ociCore.DeleteRouteTableResponse, err error)
	ListRouteTables(ctx context.Context, request ociCore.ListRouteTablesRequest) (response ociCore.ListRouteTablesResponse, err error)

	GetVnic(ctx context.Context, request ociCore.GetVnicRequest) (response ociCore.GetVnicResponse, err error)
}

type OCIFirewallClient interface {
	CreateSecurityList(ctx context.Context, request ociCore.CreateSecurityListRequest) (response ociCore.CreateSecurityListResponse, err error)
	ListSecurityLists(ctx context.Context, request ociCore.ListSecurityListsRequest) (response ociCore.ListSecurityListsResponse, err error)
	DeleteSecurityList(ctx context.Context, request ociCore.DeleteSecurityListRequest) (response ociCore.DeleteSecurityListResponse, err error)
	GetSecurityList(ctx context.Context, request ociCore.GetSecurityListRequest) (response ociCore.GetSecurityListResponse, err error)
}

type OCIStorageClient interface {
	CreateVolume(ctx context.Context, request ociCore.CreateVolumeRequest) (response ociCore.CreateVolumeResponse, err error)
	ListVolumes(ctx context.Context, request ociCore.ListVolumesRequest) (response ociCore.ListVolumesResponse, err error)
	GetVolume(ctx context.Context, request ociCore.GetVolumeRequest) (response ociCore.GetVolumeResponse, err error)
	DeleteVolume(ctx context.Context, request ociCore.DeleteVolumeRequest) (response ociCore.DeleteVolumeResponse, err error)
	UpdateVolume(ctx context.Context, request ociCore.UpdateVolumeRequest) (response ociCore.UpdateVolumeResponse, err error)
}
