// Copyright (c) 2026, RetailNext, Inc.
// This material contains trade secrets and confidential information of
// RetailNext, Inc.  Any use, reproduction, disclosure or dissemination
// is strictly prohibited without the explicit written permission
// of RetailNext, Inc.
// All rights reserved.

package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"

	"github.com/cedws/iapc/iap"
	"github.com/retailnext/ig-iap-tunnel/gcp"
	"github.com/retailnext/ig-iap-tunnel/proxy"
)

var instanceGroupIDRe = regexp.MustCompile(`^projects/([^/]+)/regions/([^/]+)/instanceGroups/([^/]+)$`)

func parseInstanceGroupID(id string) (project, region, group string, err error) {
	m := instanceGroupIDRe.FindStringSubmatch(id)
	if m == nil {
		return "", "", "", fmt.Errorf("invalid instance-group-id %q: must be projects/{project}/regions/{region}/instanceGroups/{name}", id)
	}
	return m[1], m[2], m[3], nil
}

func run() error {
	instanceGroupID := flag.String("instance-group-id", "", "Managed instance group resource ID (projects/{project}/regions/{region}/instanceGroups/{name})")
	remotePort := flag.String("remote-port", "", "Port on the remote instance")
	localPort := flag.String("local-port", "", "Local port to listen on")
	proxyDomains := flag.String("proxy-domains", "", "Comma-separated list of domains to route through the IAP tunnel. When set, connections are parsed as HTTP proxy requests (CONNECT or absolute-URI) and only matching domains (including subdomains) go through the tunnel; everything else is dialed directly. When unset, all traffic goes through the tunnel.")
	flag.Parse()

	if *instanceGroupID == "" || *remotePort == "" || *localPort == "" {
		flag.Usage()
		return fmt.Errorf("required flags not provided")
	}

	project, region, instanceGroup, err := parseInstanceGroupID(*instanceGroupID)
	if err != nil {
		return fmt.Errorf("invalid input: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	gcpConfig, err := gcp.NewConfig(ctx, project, region)
	if err != nil {
		return fmt.Errorf("failed to create GCP config: %w", err)
	}

	instanceName, zone, err := gcpConfig.FindHealthyInstanceInGroup(instanceGroup)
	if err != nil {
		return fmt.Errorf("failed to find healthy instance: %w", err)
	}
	slog.Info("selected instance", "instance", instanceName, "zone", zone)

	iface, err := gcpConfig.GetFirstNetworkInterface(instanceName, zone)
	if err != nil {
		return fmt.Errorf("failed to get network interface: %w", err)
	}

	opts := []iap.DialOption{
		iap.WithProject(project),
		iap.WithInstance(instanceName, zone, iface),
		iap.WithPort(*remotePort),
		iap.WithTokenSource(&gcpConfig.Cred.TokenSource),
	}
	var domains []string
	for d := range strings.SplitSeq(*proxyDomains, ",") {
		if d = strings.TrimSpace(d); d != "" {
			if strings.ContainsAny(d, "*?") {
				return fmt.Errorf("invalid domain %q: wildcards are not supported", d)
			}
			if strings.HasPrefix(d, ".") {
				return fmt.Errorf("invalid domain %q: leading dot is not allowed", d)
			}
			if strings.HasSuffix(d, ".") {
				return fmt.Errorf("invalid domain %q: trailing dot is not allowed", d)
			}
			domains = append(domains, d)
		}
	}
	if len(domains) > 0 {
		slog.Info("selective proxying enabled", "domains", domains)
	}

	return proxy.Listen(ctx, "127.0.0.1:"+*localPort, opts, domains)
}

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}
