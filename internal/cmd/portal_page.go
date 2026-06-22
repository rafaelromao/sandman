package cmd

import (
	"fmt"
	"html/template"
	"time"
)

type portalPageData struct {
	RepoRoot              string
	PollInterval          int
	RunsPath              string
	InstancesPath         string
	RefreshPath           string
	PortalTitle           string
	PortalSubtitle        string
	PortalStateStorageKey string
	ThemeOptionsHTML      template.HTML
	SupportedThemesJSON   template.JS
	PortalStateJS         template.JS
	PortalScrollJS        template.JS
	PortalDiffJS          template.JS
	PortalAbortSupported  bool
}

func buildPortalPageData(repoRoot string) (*portalPageData, error) {
	return &portalPageData{
		RepoRoot:              repoRoot,
		PollInterval:          int(portalPollInterval / time.Millisecond),
		RunsPath:              "/api/runs",
		InstancesPath:         "/api/instances",
		RefreshPath:           "/api/runs",
		PortalTitle:           "Sleep while your agents code",
		PortalSubtitle:        "AFK coding agents orchestration in isolated sandboxes.",
		PortalStateStorageKey: fmt.Sprintf("sandman.portal.view-state.v1:%s", repoRoot),
		ThemeOptionsHTML:      portalThemeOptionsHTML,
		SupportedThemesJSON:   portalSupportedThemesJSON,
		PortalStateJS:         portalStateJS,
		PortalScrollJS:        portalScrollJS,
		PortalDiffJS:          portalDiffJS,
		PortalAbortSupported:  portalAbortSupported(),
	}, nil
}
