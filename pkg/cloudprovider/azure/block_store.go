/*
Copyright 2017 the Heptio Ark contributors.

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

package azure

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/arm/examples/helpers"
	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2017-03-30/compute"
	"github.com/Azure/azure-sdk-for-go/services/resources/mgmt/2016-06-01/subscriptions"
	"github.com/Azure/go-autorest/autorest"
	"github.com/Azure/go-autorest/autorest/azure"
	"github.com/pkg/errors"
	"github.com/satori/uuid"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/heptio/ark/pkg/cloudprovider"
	"github.com/heptio/ark/pkg/util/collections"
)

const (
	azureClientIDKey         string = "AZURE_CLIENT_ID"
	azureClientSecretKey     string = "AZURE_CLIENT_SECRET"
	azureSubscriptionIDKey   string = "AZURE_SUBSCRIPTION_ID"
	azureTenantIDKey         string = "AZURE_TENANT_ID"
	azureStorageAccountIDKey string = "AZURE_STORAGE_ACCOUNT_ID"
	azureStorageKeyKey       string = "AZURE_STORAGE_KEY"
	azureResourceGroupKey    string = "AZURE_RESOURCE_GROUP"

	locationKey   = "location"
	apiTimeoutKey = "apiTimeout"
)

type blockStore struct {
	disks         *compute.DisksClient
	snaps         *compute.SnapshotsClient
	subscription  string
	resourceGroup string
	location      string
	apiTimeout    time.Duration
}

func getConfig() map[string]string {
	cfg := map[string]string{
		azureClientIDKey:         "",
		azureClientSecretKey:     "",
		azureSubscriptionIDKey:   "",
		azureTenantIDKey:         "",
		azureStorageAccountIDKey: "",
		azureStorageKeyKey:       "",
		azureResourceGroupKey:    "",
	}

	for key := range cfg {
		cfg[key] = os.Getenv(key)
	}

	return cfg
}

func NewBlockStore() cloudprovider.BlockStore {
	return &blockStore{}
}

func (b *blockStore) Init(config map[string]string) error {
	var (
		location      = config[locationKey]
		apiTimeoutVal = config[apiTimeoutKey]
		apiTimeout    time.Duration
		err           error
	)

	if location == "" {
		return errors.Errorf("missing %s in azure configuration", locationKey)
	}

	if apiTimeout, err = time.ParseDuration(apiTimeoutVal); err != nil {
		return errors.Wrapf(err, "could not parse %s (expected time.Duration)", apiTimeoutKey)
	}

	if apiTimeout == 0 {
		apiTimeout = 2 * time.Minute
	}

	cfg := getConfig()

	spt, err := helpers.NewServicePrincipalTokenFromCredentials(cfg, azure.PublicCloud.ResourceManagerEndpoint)
	if err != nil {
		return errors.Wrap(err, "error creating new service principal token")
	}

	disksClient := compute.NewDisksClient(cfg[azureSubscriptionIDKey])
	snapsClient := compute.NewSnapshotsClient(cfg[azureSubscriptionIDKey])

	authorizer := autorest.NewBearerAuthorizer(spt)
	disksClient.Authorizer = authorizer
	snapsClient.Authorizer = authorizer

	// validate the location
	groupClient := subscriptions.NewClient()
	groupClient.Authorizer = authorizer

	ctx := context.Background()
	locs, err := groupClient.ListLocations(ctx, cfg[azureSubscriptionIDKey])
	if err != nil {
		return errors.WithStack(err)
	}

	if locs.Value == nil {
		return errors.New("no locations returned from Azure API")
	}

	locationExists := false
	for _, loc := range *locs.Value {
		if (loc.Name != nil && *loc.Name == location) || (loc.DisplayName != nil && *loc.DisplayName == location) {
			locationExists = true
			break
		}
	}

	if !locationExists {
		return errors.Errorf("location %q not found", location)
	}

	b.disks = &disksClient
	b.snaps = &snapsClient
	b.subscription = cfg[azureSubscriptionIDKey]
	b.resourceGroup = cfg[azureResourceGroupKey]
	b.location = location
	b.apiTimeout = apiTimeout

	return nil
}

func (b *blockStore) CreateVolumeFromSnapshot(snapshotID, volumeType, volumeAZ string, iops *int64) (string, error) {
	fullSnapshotName := getFullSnapshotName(b.subscription, b.resourceGroup, snapshotID)
	diskName := "restore-" + uuid.NewV4().String()

	disk := compute.Disk{
		Name:     &diskName,
		Location: &b.location,
		Sku: &compute.DiskSku{
			Name: compute.StorageAccountTypes(volumeType),
		},
		DiskProperties: &compute.DiskProperties{
			CreationData: &compute.CreationData{
				CreateOption:     compute.Copy,
				SourceResourceID: &fullSnapshotName,
			},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), b.apiTimeout)
	defer cancel()

	future, err := b.disks.CreateOrUpdate(ctx, b.resourceGroup, *disk.Name, disk)

	err = future.WaitForCompletion(ctx, b.disks.Client)
	if err != nil {
		return "", errors.WithStack(err)
	}
	return diskName, nil
}

func (b *blockStore) GetVolumeInfo(volumeID, volumeAZ string) (string, *int64, error) {
	ctx := context.Background()

	res, err := b.disks.Get(ctx, b.resourceGroup, volumeID)
	if err != nil {
		return "", nil, errors.WithStack(err)
	}

	return string(res.Sku.Name), nil, nil
}

func (b *blockStore) IsVolumeReady(volumeID, volumeAZ string) (ready bool, err error) {
	ctx := context.Background()
	res, err := b.disks.Get(ctx, b.resourceGroup, volumeID)
	if err != nil {
		return false, errors.WithStack(err)
	}

	if res.ProvisioningState == nil {
		return false, errors.New("nil ProvisioningState returned from Get call")
	}

	return *res.ProvisioningState == "Succeeded", nil
}

func (b *blockStore) CreateSnapshot(volumeID, volumeAZ string, tags map[string]string) (string, error) {
	fullDiskName := getFullDiskName(b.subscription, b.resourceGroup, volumeID)
	// snapshot names must be <= 80 characters long
	var snapshotName string
	suffix := "-" + uuid.NewV4().String()

	if len(volumeID) <= (80 - len(suffix)) {
		snapshotName = volumeID + suffix
	} else {
		snapshotName = volumeID[0:80-len(suffix)] + suffix
	}

	snap := compute.Snapshot{
		Name: &snapshotName,
		DiskProperties: &compute.DiskProperties{
			CreationData: &compute.CreationData{
				CreateOption:     compute.Copy,
				SourceResourceID: &fullDiskName,
			},
		},
		Tags:     &map[string]*string{},
		Location: &b.location,
	}

	for k, v := range tags {
		val := v
		(*snap.Tags)[k] = &val
	}

	ctx, cancel := context.WithTimeout(context.Background(), b.apiTimeout)
	defer cancel()

	future, err := b.snaps.CreateOrUpdate(ctx, b.resourceGroup, *snap.Name, snap)

	err = future.WaitForCompletion(ctx, b.snaps.Client)
	if err != nil {
		return "", errors.WithStack(err)
	}

	return snapshotName, nil
}

func (b *blockStore) DeleteSnapshot(snapshotID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), b.apiTimeout)
	defer cancel()

	future, err := b.snaps.Delete(ctx, b.resourceGroup, snapshotID)
	err = future.WaitForCompletion(ctx, b.snaps.Client)

	return errors.WithStack(err)
}

func getFullDiskName(subscription string, resourceGroup string, diskName string) string {
	return fmt.Sprintf("/subscriptions/%v/resourceGroups/%v/providers/Microsoft.Compute/disks/%v", subscription, resourceGroup, diskName)
}

func getFullSnapshotName(subscription string, resourceGroup string, snapshotName string) string {
	return fmt.Sprintf("/subscriptions/%v/resourceGroups/%v/providers/Microsoft.Compute/snapshots/%v", subscription, resourceGroup, snapshotName)
}

func (b *blockStore) GetVolumeID(pv runtime.Unstructured) (string, error) {
	if !collections.Exists(pv.UnstructuredContent(), "spec.azureDisk") {
		return "", nil
	}

	volumeID, err := collections.GetString(pv.UnstructuredContent(), "spec.azureDisk.diskName")
	if err != nil {
		return "", err
	}

	return volumeID, nil
}

func (b *blockStore) SetVolumeID(pv runtime.Unstructured, volumeID string) (runtime.Unstructured, error) {
	azure, err := collections.GetMap(pv.UnstructuredContent(), "spec.azureDisk")
	if err != nil {
		return nil, err
	}

	if uri, err := collections.GetString(azure, "diskURI"); err == nil {
		previousVolumeID, err := collections.GetString(azure, "diskName")
		if err != nil {
			return nil, err
		}

		azure["diskURI"] = strings.Replace(uri, previousVolumeID, volumeID, -1)
	}

	azure["diskName"] = volumeID

	return pv, nil
}
