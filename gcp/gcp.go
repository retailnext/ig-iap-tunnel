// Copyright (c) 2026, RetailNext, Inc.
// This material contains trade secrets and confidential information of
// RetailNext, Inc.  Any use, reproduction, disclosure or dissemination
// is strictly prohibited without the explicit written permission
// of RetailNext, Inc.
// All rights reserved.

package gcp

import (
	"context"
	"fmt"
	"math/rand/v2"
	"regexp"

	"golang.org/x/oauth2/google"
	compute "google.golang.org/api/compute/v1"
	"google.golang.org/api/option"
)

type Config struct {
	Project    string
	Region     string
	Cred       *google.Credentials
	ctx        context.Context
	computeSvc *compute.Service
}

func NewConfig(ctx context.Context, project, region string) (*Config, error) {
	creds, err := google.FindDefaultCredentials(
		ctx, compute.ComputeReadonlyScope,
		"https://www.googleapis.com/auth/cloud-platform",
	)
	if err != nil {
		return nil, err
	}
	svc, err := compute.NewService(ctx, option.WithCredentials(creds))
	if err != nil {
		return nil, err
	}
	return &Config{
		Project:    project,
		Region:     region,
		Cred:       creds,
		ctx:        ctx,
		computeSvc: svc,
	}, nil
}

func (c *Config) FindHealthyInstanceInGroup(instanceGroupManager string) (name, zone string, err error) {
	var candidates []*compute.ManagedInstance
	pageToken := ""
	for {
		call := c.computeSvc.RegionInstanceGroupManagers.
			ListManagedInstances(c.Project, c.Region, instanceGroupManager).
			Context(c.ctx)
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}
		resp, err := call.Do()
		if err != nil {
			return "", "", fmt.Errorf("listing managed instances: %w", err)
		}
		for _, managedInstance := range resp.ManagedInstances {
			if isHealthy(managedInstance) {
				candidates = append(candidates, managedInstance)
			}
		}
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}

	if len(candidates) == 0 {
		return "", "", fmt.Errorf("no healthy instances found in %s/%s", c.Region, instanceGroupManager)
	}

	name, zone, err = parseInstanceURL(candidates[rand.IntN(len(candidates))].Instance) //nolint:gosec // non-cryptographic use: selecting a random healthy instance for load balancing
	if err != nil {
		return "", "", err
	}
	return name, zone, nil
}

func isHealthy(managedInstance *compute.ManagedInstance) bool {
	if managedInstance.CurrentAction != "NONE" || managedInstance.InstanceStatus != "RUNNING" {
		return false
	}
	// If no health checks are configured, RUNNING is sufficient.
	if len(managedInstance.InstanceHealth) == 0 {
		return true
	}
	for _, h := range managedInstance.InstanceHealth {
		if h.DetailedHealthState == "HEALTHY" {
			return true
		}
	}
	return false
}

func (c *Config) GetFirstNetworkInterface(name, zone string) (string, error) {
	instance, err := c.computeSvc.Instances.Get(c.Project, zone, name).Context(c.ctx).Do()
	if err != nil {
		return "", fmt.Errorf("getting instance %s: %w", name, err)
	}
	if len(instance.NetworkInterfaces) == 0 {
		return "", fmt.Errorf("instance %s has no network interfaces", name)
	}
	return instance.NetworkInterfaces[0].Name, nil
}

var instanceURLRe = regexp.MustCompile(`/zones/([^/]+)/instances/([^/]+)`)

// parseInstanceURL extracts the instance name and zone from a Compute Engine self-link.
// Format: https://www.googleapis.com/compute/v1/projects/{project}/zones/{zone}/instances/{name}
func parseInstanceURL(url string) (string, string, error) {
	m := instanceURLRe.FindStringSubmatch(url)
	if m == nil {
		return "", "", fmt.Errorf("cannot parse instance URL: %s", url)
	}
	zone, name := m[1], m[2]
	return name, zone, nil
}
