package main

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type permissionWorkflow struct {
	Permissions map[string]string `yaml:"permissions"`
	Jobs        map[string]struct {
		Permissions map[string]string `yaml:"permissions"`
	} `yaml:"jobs"`
}

func readPermissionWorkflow(t *testing.T, path string) permissionWorkflow {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var workflow permissionWorkflow
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	return workflow
}

func missingReusablePermissions(granted map[string]string, called permissionWorkflow) []string {
	rank := map[string]int{"": 0, "none": 0, "read": 1, "write": 2}
	var missing []string
	check := func(name string, requested map[string]string) {
		for scope, level := range requested {
			if rank[level] > rank[granted[scope]] {
				missing = append(missing, name+":"+scope)
			}
		}
	}
	check("workflow", called.Permissions)
	// GitHub validates even conditional jobs before any caller job can start.
	for name, job := range called.Jobs {
		check(name, job.Permissions)
	}
	return missing
}

func TestReleaseCallerAllowsReusableCIPermissions(t *testing.T) {
	release := readPermissionWorkflow(t, "../.github/workflows/release.yml")
	ci := readPermissionWorkflow(t, "../.github/workflows/go.yml")
	granted := release.Jobs["quality"].Permissions
	if granted == nil {
		granted = release.Permissions
	}
	if missing := missingReusablePermissions(granted, ci); len(missing) != 0 {
		t.Fatalf("Release cannot start reusable CI: missing permissions %v", missing)
	}
	for _, scope := range []string{"actions", "pull-requests"} {
		t.Run("reject_missing_"+scope, func(t *testing.T) {
			reduced := make(map[string]string)
			for key, value := range granted {
				if key != scope {
					reduced[key] = value
				}
			}
			if !strings.Contains(strings.Join(missingReusablePermissions(reduced, ci), ","), ":"+scope) {
				t.Fatalf("must detect missing %s before Release is deployed", scope)
			}
		})
	}
}
