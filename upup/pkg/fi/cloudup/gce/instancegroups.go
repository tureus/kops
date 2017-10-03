/*
Copyright 2017 The Kubernetes Authors.

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

package gce

import (
	"fmt"
	"github.com/golang/glog"
	context "golang.org/x/net/context"
	compute "google.golang.org/api/compute/v0.beta"
	"k8s.io/api/core/v1"
	"k8s.io/kops/pkg/apis/kops"
	"k8s.io/kops/pkg/cloudinstances"
	"strconv"
)

// DeleteGroup deletes a cloud of instances controlled by an Instance Group Manager
func (c *gceCloudImplementation) DeleteGroup(g *cloudinstances.CloudInstanceGroup) error {
	return deleteCloudInstanceGroup(c, g)
}

// deleteCloudInstanceGroup deletes the InstanceGroupManager and current InstanceTemplate
func deleteCloudInstanceGroup(c GCECloud, g *cloudinstances.CloudInstanceGroup) error {
	mig := g.Raw.(*compute.InstanceGroupManager)
	err := DeleteInstanceGroupManager(c, mig)
	if err != nil {
		return err
	}

	return DeleteInstanceTemplate(c, mig.InstanceTemplate)
}

// DeleteGroup implements fi.Cloud::DeleteGroup
func (c *mockGCECloud) DeleteGroup(g *cloudinstances.CloudInstanceGroup) error {
	return deleteCloudInstanceGroup(c, g)
}

// DeleteInstance deletes a GCE instance
func (c *gceCloudImplementation) DeleteInstance(i *cloudinstances.CloudInstanceGroupMember) error {
	return DeleteInstance(c, i.ID)
}

// DeleteInstance deletes a GCE instance
func (c *mockGCECloud) DeleteInstance(i *cloudinstances.CloudInstanceGroupMember) error {
	return DeleteInstance(c, i.ID)
}

// GetCloudGroups returns a map of CloudGroup that backs a list of instance groups
func (c *gceCloudImplementation) GetCloudGroups(cluster *kops.Cluster, instancegroups []*kops.InstanceGroup, warnUnmatched bool, nodes []v1.Node) (map[string]*cloudinstances.CloudInstanceGroup, error) {
	return getCloudGroups(c, cluster, instancegroups, warnUnmatched, nodes)
}

func getCloudGroups(c GCECloud, cluster *kops.Cluster, instancegroups []*kops.InstanceGroup, warnUnmatched bool, nodes []v1.Node) (map[string]*cloudinstances.CloudInstanceGroup, error) {
	groups := make(map[string]*cloudinstances.CloudInstanceGroup)

	project := c.Project()
	ctx := context.Background()
	nodesByExternalID := cloudinstances.GetNodeMap(nodes)

	// There is some code duplication with resources/gce.go here, but more in the structure than a straight copy-paste

	// The strategy:
	// * Find the InstanceTemplates, matching on tags
	// * Find InstanceGroupManagers attached to those templates
	// * Find Instances attached to those InstanceGroupManagers

	instanceTemplates := make(map[string]*compute.InstanceTemplate)
	{
		templates, err := FindInstanceTemplates(c, cluster.Name)
		if err != nil {
			return nil, err
		}

		for _, t := range templates {
			instanceTemplates[t.SelfLink] = t
		}
	}

	zones, err := c.Zones()
	if err != nil {
		return nil, err
	}

	for _, zoneName := range zones {
		err := c.Compute().InstanceGroupManagers.List(project, zoneName).Pages(ctx, func(page *compute.InstanceGroupManagerList) error {
			for _, mig := range page.Items {
				name := mig.Name

				instanceTemplate := instanceTemplates[mig.InstanceTemplate]
				if instanceTemplate == nil {
					glog.V(2).Infof("ignoring MIG %s with unmanaged InstanceTemplate: %s", name, mig.InstanceTemplate)
					continue
				}

				ig, err := matchInstanceGroup(mig, cluster, instancegroups)
				if err != nil {
					return fmt.Errorf("error getting instance group for MIG %q", name)
				}
				if ig == nil {
					if warnUnmatched {
						glog.Warningf("Found MIG with no corresponding instance group %q", name)
					}
					continue
				}

				g := &cloudinstances.CloudInstanceGroup{
					HumanName:     mig.Name,
					InstanceGroup: ig,
					MinSize:       int(mig.TargetSize),
					MaxSize:       int(mig.TargetSize),
					Raw:           mig,
				}
				groups[mig.Name] = g

				latestInstanceTemplate := mig.InstanceTemplate

				instances, err := ListManagedInstances(c, mig)
				if err != nil {
					return err
				}

				for _, i := range instances {
					id := i.Instance
					cm := &cloudinstances.CloudInstanceGroupMember{
						ID: id,
					}

					node := nodesByExternalID[strconv.FormatUint(i.Id, 10)]
					if node != nil {
						cm.Node = node
					} else {
						glog.V(8).Infof("unable to find node for instance: %s", id)
					}

					if i.Version != nil && latestInstanceTemplate == i.Version.InstanceTemplate {
						g.Ready = append(g.Ready, cm)
					} else {
						g.NeedUpdate = append(g.NeedUpdate, cm)
					}
				}

			}
			return nil
		})

		if err != nil {
			return nil, fmt.Errorf("error listing InstanceGroupManagers: %v", err)
		}
	}

	return groups, nil
}

// NameForInstanceGroupManager builds a name for an InstanceGroupManager in the specified zone
func NameForInstanceGroupManager(c *kops.Cluster, ig *kops.InstanceGroup, zone string) string {
	name := SafeObjectName(zone+"."+ig.ObjectMeta.Name, c.ObjectMeta.Name)
	return name
}

// matchInstanceGroup filters a list of instancegroups for recognized cloud groups
func matchInstanceGroup(mig *compute.InstanceGroupManager, c *kops.Cluster, instancegroups []*kops.InstanceGroup) (*kops.InstanceGroup, error) {
	var matches []*kops.InstanceGroup
	for _, ig := range instancegroups {
		name := NameForInstanceGroupManager(c, ig, mig.Zone)
		if name == mig.Name {
			matches = append(matches, ig)
		}
	}

	if len(matches) == 0 {
		return nil, nil
	}
	if len(matches) != 1 {
		return nil, fmt.Errorf("found multiple instance groups matching MIG %q", mig.Name)
	}
	return matches[0], nil
}
