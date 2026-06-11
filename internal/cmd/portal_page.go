package cmd

import (
	"encoding/json"
	"fmt"
	"html/template"
	"time"
)

type portalPageData struct {
	RepoRoot              string
	PollInterval          int
	CommandsPath          string
	RunsPath              string
	InstancesPath         string
	RefreshPath           string
	PortalTitle           string
	PortalSubtitle        string
	PortalStateStorageKey string
	LaunchData            portalLaunchFormData
	LaunchDataJSON        template.JS
	ThemeOptionsHTML      template.HTML
	SupportedThemesJSON   template.JS
	PortalStateJS         template.JS
	PortalScrollJS        template.JS
	PortalDiffJS          template.JS
	PortalAbortSupported  bool
}

func buildPortalPageData(repoRoot string, launchData portalLaunchFormData) (*portalPageData, error) {
	launchDataJSON, err := json.Marshal(struct {
		Agent             string `json:"agent"`
		Model             string `json:"model"`
		BaseBranch        string `json:"baseBranch"`
		Sandbox           string `json:"sandbox"`
		Parallel          int    `json:"parallel"`
		StartDelay        int    `json:"startDelay"`
		ContainerCapacity int    `json:"containerCapacity"`
		MaxContainers     int    `json:"maxContainers"`
		Ralph             int    `json:"ralph"`
	}{
		Agent:             launchData.Agent,
		Model:             launchData.Model,
		BaseBranch:        launchData.BaseBranch,
		Sandbox:           launchData.Sandbox,
		Parallel:          launchData.Parallel,
		StartDelay:        launchData.StartDelay,
		ContainerCapacity: launchData.ContainerCapacity,
		MaxContainers:     launchData.MaxContainers,
		Ralph:             launchData.Ralph,
	})
	if err != nil {
		return nil, err
	}
	return &portalPageData{
		RepoRoot:              repoRoot,
		PollInterval:          int(portalPollInterval / time.Millisecond),
		CommandsPath:          "/api/commands",
		RunsPath:              "/api/runs",
		InstancesPath:         "/api/instances",
		RefreshPath:           "/api/runs",
		PortalTitle:           "Sleep while your agents code",
		PortalSubtitle:        "AFK coding agents orchestration in isolated sandboxes.",
		PortalStateStorageKey: fmt.Sprintf("sandman.portal.view-state.v1:%s", repoRoot),
		LaunchData:            launchData,
		LaunchDataJSON:        template.JS(launchDataJSON),
		ThemeOptionsHTML:      portalThemeOptionsHTML,
		SupportedThemesJSON:   portalSupportedThemesJSON,
		PortalStateJS:         portalStateJS,
		PortalScrollJS:        portalScrollJS,
		PortalDiffJS:          portalDiffJS,
		PortalAbortSupported:  portalAbortSupported(),
	}, nil
}
