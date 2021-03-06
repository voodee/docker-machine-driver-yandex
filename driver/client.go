package driver

import (
	"context"
	"errors"
	"fmt"

	"github.com/c2h5oh/datasize"
	"github.com/docker/machine/libmachine/log"
	"github.com/yandex-cloud/go-genproto/yandex/cloud/compute/v1"
	ycsdk "github.com/yandex-cloud/go-sdk"
	"github.com/yandex-cloud/go-sdk/iamkey"
)

const StandardImagesFolderID = "standard-images"

type YCClient struct {
	sdk *ycsdk.SDK
}

func (c *YCClient) createInstance(d *Driver) error {
	ctx := context.Background()

	imageID := d.ImageID
	if imageID == "" {
		var err error
		imageID, err = c.getImageIDFromFolder(d.ImageFamily, d.ImageFolderID)
		if err != nil {
			return err
		}
	}

	log.Infof("Use image with ID %q from folder ID %q", imageID, d.ImageFolderID)

	request := prepareInstanceCreateRequest(d, imageID)

	op, err := c.sdk.WrapOperation(c.sdk.Compute().Instance().Create(ctx, request))
	if err != nil {
		return fmt.Errorf("Error while requesting API to create instance: %s", err)
	}

	protoMetadata, err := op.Metadata()
	if err != nil {
		return fmt.Errorf("Error while get instance create operation metadata: %s", err)
	}

	md, ok := protoMetadata.(*compute.CreateInstanceMetadata)
	if !ok {
		return fmt.Errorf("could not get Instance ID from create operation metadata")
	}

	d.InstanceID = md.InstanceId

	log.Infof("Waiting for Instance with ID %q", d.InstanceID)
	if err = op.Wait(ctx); err != nil {
		return fmt.Errorf("Error while waiting operation to create instance: %s", err)
	}

	resp, err := op.Response()
	if err != nil {
		return fmt.Errorf("Instance creation failed: %s", err)
	}

	instance, ok := resp.(*compute.Instance)
	if !ok {
		return fmt.Errorf("Create response doesn't contain Instance")
	}

	d.IPAddress, err = c.getInstanceIPAddress(d, instance)

	return err
}

func prepareInstanceCreateRequest(d *Driver, imageID string) *compute.CreateInstanceRequest {
	// TODO support static address assignment
	// TODO additional disks

	request := &compute.CreateInstanceRequest{
		FolderId:   d.FolderID,
		Name:       d.MachineName,
		ZoneId:     d.Zone,
		PlatformId: d.PlatformID,
		ResourcesSpec: &compute.ResourcesSpec{
			Cores:        int64(d.Cores),
			CoreFraction: int64(d.CoreFraction),
			Memory:       toBytes(d.Memory),
		},
		BootDiskSpec: &compute.AttachedDiskSpec{
			AutoDelete: true,
			Disk: &compute.AttachedDiskSpec_DiskSpec_{
				DiskSpec: &compute.AttachedDiskSpec_DiskSpec{
					TypeId: d.DiskType,
					Size:   toBytes(d.DiskSize),
					Source: &compute.AttachedDiskSpec_DiskSpec_ImageId{
						ImageId: imageID,
					},
				},
			},
		},
		Labels: d.ParsedLabels(),
		NetworkInterfaceSpecs: []*compute.NetworkInterfaceSpec{
			{
				SubnetId:             d.SubnetID,
				PrimaryV4AddressSpec: &compute.PrimaryAddressSpec{},
			},
		},
		SchedulingPolicy: &compute.SchedulingPolicy{
			Preemptible: d.Preemptible,
		},
		Metadata: d.Metadata,
	}

	if d.Nat {
		request.NetworkInterfaceSpecs[0].PrimaryV4AddressSpec.OneToOneNatSpec = &compute.OneToOneNatSpec{
			IpVersion: compute.IpVersion_IPV4,
		}
	}

	return request
}

func NewYCClient(d *Driver) (*YCClient, error) {
	if d.Token != "" && d.ServiceAccountKeyFile != "" {
		return nil, errors.New("one of token or service account key file must be specified, not both")
	}

	var credentials ycsdk.Credentials
	switch {
	case d.Token != "":
		credentials = ycsdk.OAuthToken(d.Token)
	case d.ServiceAccountKeyFile != "":
		key, err := iamkey.ReadFromJSONFile(d.ServiceAccountKeyFile)
		if err != nil {
			return nil, err
		}

		credentials, err = ycsdk.ServiceAccountKey(key)
		if err != nil {
			return nil, err
		}
	}

	config := ycsdk.Config{
		Credentials: credentials,
	}

	if d.Endpoint != "" {
		config.Endpoint = d.Endpoint
	}

	sdk, err := ycsdk.Build(context.Background(), config)
	if err != nil {
		return nil, err
	}

	return &YCClient{
		sdk: sdk,
	}, nil
}

func (c *YCClient) getImageIDFromFolder(familyName, lookupFolderID string) (string, error) {
	image, err := c.sdk.Compute().Image().GetLatestByFamily(context.Background(), &compute.GetImageLatestByFamilyRequest{
		FolderId: lookupFolderID,
		Family:   familyName,
	})
	if err != nil {
		return "", err
	}
	return image.Id, nil
}

func (c *YCClient) getInstanceIPAddress(d *Driver, instance *compute.Instance) (address string, err error) {
	// Instance could have several network interfaces with different configuration each
	// Get all possible addresses for instance
	addrIPV4Internal, addrIPV4External, addrIPV6Addr, err := c.instanceAddresses(instance)
	if err != nil {
		return "", err
	}

	if d.UseIPv6 {
		if addrIPV6Addr != "" {
			return "[" + addrIPV6Addr + "]", nil
		}
		return "", errors.New("instance has no one IPv6 address")
	}

	if d.UseInternalIP {
		if addrIPV4Internal != "" {
			return addrIPV4Internal, nil
		}
		return "", errors.New("instance has no one IPv4 internal address")
	}
	if addrIPV4External != "" {
		return addrIPV4External, nil
	}
	return "", errors.New("instance has no one IPv4 external address")
}

func (c *YCClient) instanceAddresses(instance *compute.Instance) (ipV4Int, ipV4Ext, ipV6 string, err error) {
	if len(instance.NetworkInterfaces) == 0 {
		return "", "", "", errors.New("No one network interface found for an instance")
	}

	var ipV4IntFound, ipV4ExtFound, ipV6Found bool
	for _, iface := range instance.NetworkInterfaces {
		if !ipV6Found && iface.PrimaryV6Address != nil {
			ipV6 = iface.PrimaryV6Address.Address
			ipV6Found = true
		}

		if !ipV4IntFound && iface.PrimaryV4Address != nil {
			ipV4Int = iface.PrimaryV4Address.Address
			ipV4IntFound = true

			if !ipV4ExtFound && iface.PrimaryV4Address.OneToOneNat != nil {
				ipV4Ext = iface.PrimaryV4Address.OneToOneNat.Address
				ipV4ExtFound = true
			}
		}

		if ipV6Found && ipV4IntFound && ipV4ExtFound {
			break
		}
	}

	if !ipV4IntFound {
		// internal ipV4 address always should present
		return "", "", "", errors.New("No IPv4 internal address found. Bug?")
	}

	return
}

func toBytes(gigabytesCount int) int64 {
	return int64((datasize.ByteSize(gigabytesCount) * datasize.GB).Bytes())
}
