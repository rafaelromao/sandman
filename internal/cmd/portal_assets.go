package cmd

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"sort"
	"strings"
)

//go:embed portal.html portal_themes.json portal_state.js portal_scroll.js portal_diff.js
var portalAssets embed.FS

type portalThemeDef struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

var (
	portalThemeDefs           []portalThemeDef
	portalThemeOptionsHTML    template.HTML
	portalSupportedThemesJSON template.JS
	portalStateJS             template.JS
	portalScrollJS            template.JS
	portalDiffJS              template.JS
	portalPageTemplate        = template.Must(template.New("portal.html").ParseFS(portalAssets, "portal.html"))
)

func init() {
	data, err := portalAssets.ReadFile("portal_themes.json")
	if err != nil {
		panic(fmt.Sprintf("read portal themes: %v", err))
	}
	if err := json.Unmarshal(data, &portalThemeDefs); err != nil {
		panic(fmt.Sprintf("parse portal themes: %v", err))
	}

	sort.Slice(portalThemeDefs, func(i, j int) bool {
		return strings.Compare(portalThemeDefs[i].Label, portalThemeDefs[j].Label) < 0
	})

	values := make([]string, 0, len(portalThemeDefs))
	var options bytes.Buffer
	for _, theme := range portalThemeDefs {
		values = append(values, theme.ID)
		_, _ = fmt.Fprintf(&options, "<option value=\"%s\">%s</option>\n", template.HTMLEscapeString(theme.ID), template.HTMLEscapeString(theme.Label))
	}
	portalThemeOptionsHTML = template.HTML(options.String())
	b, err := json.Marshal(values)
	if err != nil {
		panic(fmt.Sprintf("marshal portal themes: %v", err))
	}
	portalSupportedThemesJSON = template.JS(b)

	stateJS, err := portalAssets.ReadFile("portal_state.js")
	if err != nil {
		panic(fmt.Sprintf("read portal state helper: %v", err))
	}
	portalStateJS = template.JS(stateJS)

	scrollJS, err := portalAssets.ReadFile("portal_scroll.js")
	if err != nil {
		panic(fmt.Sprintf("read portal scroll helper: %v", err))
	}
	portalScrollJS = template.JS(scrollJS)

	diffJS, err := portalAssets.ReadFile("portal_diff.js")
	if err != nil {
		panic(fmt.Sprintf("read portal diff helper: %v", err))
	}
	portalDiffJS = template.JS(diffJS)
}
