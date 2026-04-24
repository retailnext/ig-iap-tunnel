// Copyright (c) 2026, RetailNext, Inc.
// This material contains trade secrets and confidential information of
// RetailNext, Inc.  Any use, reproduction, disclosure or dissemination
// is strictly prohibited without the explicit written permission
// of RetailNext, Inc.
// All rights reserved.

package gcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	compute "google.golang.org/api/compute/v1"
	"google.golang.org/api/option"
)

// instanceURL returns a fake Compute Engine self-link for use in tests.
func instanceURL(project, zone, name string) string {
	return "https://www.googleapis.com/compute/v1/projects/" + project + "/zones/" + zone + "/instances/" + name
}

// newTestConfig creates a GCPConfig wired to a mock HTTP server.
// The server handler receives all Compute API calls made during the test.
func newTestConfig(ctx context.Context, t *testing.T, handler http.HandlerFunc) *Config {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	svc, err := compute.NewService(ctx,
		option.WithEndpoint(ts.URL+"/"),
		option.WithoutAuthentication(),
	)
	if err != nil {
		t.Fatalf("compute.NewService: %v", err)
	}
	return &Config{
		Project:    "test-project",
		Region:     "us-central1",
		ctx:        ctx,
		computeSvc: svc,
	}
}

// respondJSON writes v as a JSON response.
func respondJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v) //nolint:errcheck // encoding failure corrupts the response and causes the test to fail on the read side
}

// --- isHealthy ---

func TestIsHealthy(t *testing.T) {
	tests := []struct {
		name string
		mi   *compute.ManagedInstance
		want bool
	}{
		{
			name: "running with no health checks",
			mi:   &compute.ManagedInstance{CurrentAction: "NONE", InstanceStatus: "RUNNING"},
			want: true,
		},
		{
			name: "running with healthy check",
			mi: &compute.ManagedInstance{
				CurrentAction:  "NONE",
				InstanceStatus: "RUNNING",
				InstanceHealth: []*compute.ManagedInstanceInstanceHealth{
					{DetailedHealthState: "HEALTHY"},
				},
			},
			want: true,
		},
		{
			name: "running but all checks unhealthy",
			mi: &compute.ManagedInstance{
				CurrentAction:  "NONE",
				InstanceStatus: "RUNNING",
				InstanceHealth: []*compute.ManagedInstanceInstanceHealth{
					{DetailedHealthState: "UNHEALTHY"},
				},
			},
			want: false,
		},
		{
			name: "running with mixed health checks — at least one healthy",
			mi: &compute.ManagedInstance{
				CurrentAction:  "NONE",
				InstanceStatus: "RUNNING",
				InstanceHealth: []*compute.ManagedInstanceInstanceHealth{
					{DetailedHealthState: "UNHEALTHY"},
					{DetailedHealthState: "HEALTHY"},
				},
			},
			want: true,
		},
		{
			name: "not running",
			mi:   &compute.ManagedInstance{CurrentAction: "NONE", InstanceStatus: "STAGING"},
			want: false,
		},
		{
			name: "running but being recreated",
			mi:   &compute.ManagedInstance{CurrentAction: "RECREATING", InstanceStatus: "RUNNING"},
			want: false,
		},
		{
			name: "being deleted",
			mi:   &compute.ManagedInstance{CurrentAction: "DELETING", InstanceStatus: "RUNNING"},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isHealthy(tt.mi); got != tt.want {
				t.Errorf("isHealthy() = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- parseInstanceURL ---

func TestParseInstanceURL(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		wantName string
		wantZone string
		wantErr  bool
	}{
		{
			name:     "valid self-link",
			url:      "https://www.googleapis.com/compute/v1/projects/my-project/zones/us-central1-a/instances/my-instance",
			wantName: "my-instance",
			wantZone: "us-central1-a",
		},
		{
			name:    "missing instances segment",
			url:     "https://www.googleapis.com/compute/v1/projects/proj/zones/us-central1-a",
			wantErr: true,
		},
		{
			name:    "empty string",
			url:     "",
			wantErr: true,
		},
		{
			name:    "unrecognised format",
			url:     "not-valid",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name, zone, err := parseInstanceURL(tt.url)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseInstanceURL() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				assert.Equal(t, tt.wantName, name)
				assert.Equal(t, tt.wantZone, zone)
			}
		})
	}
}

// --- FindHealthyInstanceInGroup ---

func TestFindHealthyInstanceInGroup(t *testing.T) {
	ctx := context.Background()

	t.Run("returns first healthy instance", func(t *testing.T) {
		cfg := newTestConfig(ctx, t, func(w http.ResponseWriter, _ *http.Request) {
			respondJSON(w, &compute.RegionInstanceGroupManagersListInstancesResponse{
				ManagedInstances: []*compute.ManagedInstance{
					{
						Instance:       instanceURL("test-project", "us-central1-a", "instance-1"),
						InstanceStatus: "RUNNING",
						CurrentAction:  "NONE",
					},
				},
			})
		})

		name, zone, err := cfg.FindHealthyInstanceInGroup("my-mig")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assert.Equal(t, "instance-1", name)
		assert.Equal(t, "us-central1-a", zone)
	})

	t.Run("skips unhealthy instances and returns a healthy one", func(t *testing.T) {
		cfg := newTestConfig(ctx, t, func(w http.ResponseWriter, _ *http.Request) {
			respondJSON(w, &compute.RegionInstanceGroupManagersListInstancesResponse{
				ManagedInstances: []*compute.ManagedInstance{
					{
						Instance:       instanceURL("test-project", "us-central1-a", "bad-instance"),
						InstanceStatus: "RUNNING",
						CurrentAction:  "RECREATING",
					},
					{
						Instance:       instanceURL("test-project", "us-central1-b", "good-instance"),
						InstanceStatus: "RUNNING",
						CurrentAction:  "NONE",
						InstanceHealth: []*compute.ManagedInstanceInstanceHealth{
							{DetailedHealthState: "HEALTHY"},
						},
					},
				},
			})
		})

		name, zone, err := cfg.FindHealthyInstanceInGroup("my-mig")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assert.Equal(t, "good-instance", name)
		assert.Equal(t, "us-central1-b", zone)
	})

	t.Run("selects randomly among multiple healthy instances", func(t *testing.T) {
		cfg := newTestConfig(ctx, t, func(w http.ResponseWriter, _ *http.Request) {
			respondJSON(w, &compute.RegionInstanceGroupManagersListInstancesResponse{
				ManagedInstances: []*compute.ManagedInstance{
					{Instance: instanceURL("test-project", "us-central1-a", "instance-a"), InstanceStatus: "RUNNING", CurrentAction: "NONE"},
					{Instance: instanceURL("test-project", "us-central1-b", "instance-b"), InstanceStatus: "RUNNING", CurrentAction: "NONE"},
					{Instance: instanceURL("test-project", "us-central1-c", "instance-c"), InstanceStatus: "RUNNING", CurrentAction: "NONE"},
				},
			})
		})

		seen := map[string]bool{}
		// I hope 30 is enough to see some variation without making the test too slow.
		for range 30 {
			name, _, err := cfg.FindHealthyInstanceInGroup("my-mig")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			seen[name] = true
		}
		if len(seen) < 2 {
			t.Errorf("expected random selection across instances, but only saw %v", seen)
		}
	})

	t.Run("returns error when no healthy instances", func(t *testing.T) {
		cfg := newTestConfig(ctx, t, func(w http.ResponseWriter, _ *http.Request) {
			respondJSON(w, &compute.RegionInstanceGroupManagersListInstancesResponse{
				ManagedInstances: []*compute.ManagedInstance{
					{
						Instance:       instanceURL("test-project", "us-central1-a", "bad"),
						InstanceStatus: "STAGING",
						CurrentAction:  "CREATING",
					},
				},
			})
		})

		_, _, err := cfg.FindHealthyInstanceInGroup("my-mig")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})

	t.Run("returns error when instance group is empty", func(t *testing.T) {
		cfg := newTestConfig(ctx, t, func(w http.ResponseWriter, _ *http.Request) {
			respondJSON(w, &compute.RegionInstanceGroupManagersListInstancesResponse{})
		})

		_, _, err := cfg.FindHealthyInstanceInGroup("my-mig")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})

	t.Run("follows pagination to find healthy instance", func(t *testing.T) {
		page := 0
		cfg := newTestConfig(ctx, t, func(w http.ResponseWriter, _ *http.Request) {
			page++
			if page == 1 {
				respondJSON(w, &compute.RegionInstanceGroupManagersListInstancesResponse{
					ManagedInstances: []*compute.ManagedInstance{
						{
							Instance:       instanceURL("test-project", "us-central1-a", "unhealthy"),
							InstanceStatus: "RUNNING",
							CurrentAction:  "RECREATING",
						},
					},
					NextPageToken: "page2token",
				})
			} else {
				respondJSON(w, &compute.RegionInstanceGroupManagersListInstancesResponse{
					ManagedInstances: []*compute.ManagedInstance{
						{
							Instance:       instanceURL("test-project", "us-central1-b", "healthy"),
							InstanceStatus: "RUNNING",
							CurrentAction:  "NONE",
						},
					},
				})
			}
		})

		name, _, err := cfg.FindHealthyInstanceInGroup("my-mig")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assert.Equal(t, "healthy", name)
		assert.Equal(t, 2, page)
	})

	t.Run("returns error on API failure", func(t *testing.T) {
		cfg := newTestConfig(ctx, t, func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "internal server error", http.StatusInternalServerError)
		})

		_, _, err := cfg.FindHealthyInstanceInGroup("my-mig")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		assert.Contains(t, err.Error(), "listing managed instances")
	})
}
