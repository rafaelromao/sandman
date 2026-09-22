package scaffold

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionAtLeast(t *testing.T) {
	cases := []struct {
		name     string
		resolved string
		min      string
		want     bool
	}{
		{"equal", "1.24.0", "1.24.0", true},
		{"patch greater", "1.24.13", "1.24.0", true},
		{"minor greater", "1.26.3", "1.24.0", true},
		{"minor less", "1.20.14", "1.24.0", false},
		{"min without patch", "1.24.13", "1.24", true},
		{"min without patch below", "1.23.5", "1.24", false},
		{"node floors", "20.19.0", "20.19", true},
		{"node below floor", "18.20.8", "20.19", false},
		{"dotnet floor", "10.0.100", "8.0", true},
		{"dotnet below floor", "7.0.410", "8.0", false},
		{"v prefix", "v1.2.1", "1.0.0", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := versionAtLeast(tc.resolved, tc.min); got != tc.want {
				t.Errorf("versionAtLeast(%q, %q) = %v, want %v", tc.resolved, tc.min, got, tc.want)
			}
		})
	}
}

func TestAnalyzerMetadataLine(t *testing.T) {
	cases := []struct {
		name      string
		preset    string
		goVer     string
		nodeVer   string
		dotnetVer string
		rustVer   string
		want      string
	}{
		{
			name:   "generic",
			preset: defaultBuildToolsPreset,
			want:   "# sandman analyzers: manual",
		},
		{
			name:   "go modern",
			preset: goBuildToolsPreset,
			goVer:  "1.26.3",
			want:   "# sandman analyzers: gocognit@v1.2.1,gocyclo@v0.6.0",
		},
		{
			name:   "go old skips gocognit",
			preset: goBuildToolsPreset,
			goVer:  "1.20.14",
			want:   "# sandman analyzers: gocyclo@v0.6.0",
		},
		{
			name:      "dotnet modern",
			preset:    dotnetBuildToolsPreset,
			dotnetVer: "10.0.100",
			want:      "# sandman analyzers: dotnet-crap@0.1.1",
		},
		{
			name:      "dotnet old skipped",
			preset:    dotnetBuildToolsPreset,
			dotnetVer: "7.0.410",
			want:      "# sandman analyzers: manual",
		},
		{
			name:    "node modern",
			preset:  nodeBuildToolsPreset,
			nodeVer: "24.2.0",
			want:    "# sandman analyzers: eslint@10.8.0",
		},
		{
			name:    "node old skipped",
			preset:  nodeBuildToolsPreset,
			nodeVer: "18.20.8",
			want:    "# sandman analyzers: manual",
		},
		{
			name:   "python",
			preset: pythonBuildToolsPreset,
			want:   "# sandman analyzers: radon@6.0.1",
		},
		{
			name:   "elixir",
			preset: elixirBuildToolsPreset,
			want:   "# sandman analyzers: credo@1.7.19",
		},
		{
			name:   "ruby",
			preset: rubyBuildToolsPreset,
			want:   "# sandman analyzers: flog@4.9.4",
		},
		{
			name:    "rust",
			preset:  rustBuildToolsPreset,
			rustVer: "1.96.1",
			want:    "# sandman analyzers: clippy@1.96.1",
		},
		{
			name:   "java",
			preset: javaBuildToolsPreset,
			want:   "# sandman analyzers: pmd@7.27.0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderAnalyzerMetadataLine(tc.preset, tc.goVer, tc.nodeVer, tc.dotnetVer, tc.rustVer)
			if !strings.Contains(got, tc.want) {
				t.Errorf("renderAnalyzerMetadataLine() = %q, want it to contain %q", got, tc.want)
			}
		})
	}
}

func TestScaffold_DockerfileProvisionAnalyzers(t *testing.T) {
	cases := []struct {
		preset      string
		toolVersion string
		metadata    string
		installBits []string
	}{
		{
			preset:      "generic",
			metadata:    "# sandman analyzers: manual",
			installBits: nil,
		},
		{
			preset:   "go",
			metadata: "# sandman analyzers: gocognit@v1.2.1,gocyclo@v0.6.0",
			installBits: []string{
				"ENV PATH=\"/.local/share/go/bin:$PATH\"",
				"RUN go install github.com/uudashr/gocognit/cmd/gocognit@v1.2.1",
				"RUN go install github.com/fzipp/gocyclo/cmd/gocyclo@v0.6.0",
			},
		},
		{
			preset:   "python",
			metadata: "# sandman analyzers: radon@6.0.1",
			installBits: []string{
				"RUN pip3 install radon==6.0.1",
				"RUN ln -sf \"$(dirname \"$(mise which python3)\")/radon\" /usr/local/bin/radon",
			},
		},
		{
			preset:   "elixir",
			metadata: "# sandman analyzers: credo@1.7.19",
			installBits: []string{
				"RUN mix escript.install hex credo 1.7.19 --force",
				"ENV PATH=\"/root/.mix/escripts:$PATH\"",
			},
		},
		{
			preset:   "ruby",
			metadata: "# sandman analyzers: flog@4.9.4",
			installBits: []string{
				"RUN gem install flog -v 4.9.4",
				"RUN ln -sf \"$(dirname \"$(mise which ruby)\")/flog\" /usr/local/bin/flog",
			},
		},
		{
			preset:      "rust",
			metadata:    "# sandman analyzers: clippy@",
			installBits: nil,
		},
		{
			preset:   "java",
			metadata: "# sandman analyzers: pmd@7.27.0",
			installBits: []string{
				"RUN curl -fsSL https://github.com/pmd/pmd/releases/download/pmd_releases%2F7.27.0/pmd-dist-7.27.0-bin.zip -o /tmp/pmd.zip",
				"unzip -q /tmp/pmd.zip -d /opt",
				"/opt/pmd-bin-7.27.0/bin/pmd /usr/local/bin/pmd",
			},
		},
		{
			preset:      "node",
			toolVersion: "24",
			metadata:    "# sandman analyzers: eslint@10.8.0",
			installBits: []string{"RUN npm install -g eslint@10.8.0"},
		},
		{
			preset:      "dotnet",
			toolVersion: "8.0",
			metadata:    "# sandman analyzers: dotnet-crap@0.1.1",
			installBits: []string{
				"RUN dotnet tool install -g Crap4DotNet --version 0.1.1",
				"ENV PATH=\"/root/.dotnet/tools:$PATH\"",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.preset, func(t *testing.T) {
			dir := t.TempDir()
			s := &Scaffolder{}
			if err := s.Scaffold(dir, Options{BuildTools: tc.preset, ToolVersion: tc.toolVersion}, &fakePrompter{confirm: true}); err != nil {
				t.Fatalf("scaffold: %v", err)
			}
			data, err := readDockerfileContent(filepath.Join(dir, ".sandman", "Dockerfile"))
			if err != nil {
				t.Fatalf("read Dockerfile: %v", err)
			}
			if !strings.Contains(data, tc.metadata) {
				t.Errorf("Dockerfile missing analyzer metadata %q, got:\n%s", tc.metadata, data)
			}
			for _, bit := range tc.installBits {
				if !strings.Contains(data, bit) {
					t.Errorf("Dockerfile missing analyzer install %q, got:\n%s", bit, data)
				}
			}
		})
	}
}

func TestScaffold_GatingOmitsAnalyzerBelowFloor(t *testing.T) {
	cases := []struct {
		preset      string
		toolVersion string
		omit        string
	}{
		{"go", "prefix:1.20", "gocognit"},
		{"node", "18", "eslint"},
		{"dotnet", "7", "Crap4DotNet"},
	}
	for _, tc := range cases {
		t.Run(tc.preset, func(t *testing.T) {
			dir := t.TempDir()
			s := &Scaffolder{}
			if err := s.Scaffold(dir, Options{BuildTools: tc.preset, ToolVersion: tc.toolVersion}, &fakePrompter{confirm: true}); err != nil {
				t.Fatalf("scaffold: %v", err)
			}
			data, err := readDockerfileContent(filepath.Join(dir, ".sandman", "Dockerfile"))
			if err != nil {
				t.Fatalf("read Dockerfile: %v", err)
			}
			if strings.Contains(data, tc.omit) {
				t.Errorf("Dockerfile should omit %q below its version floor, got:\n%s", tc.omit, data)
			}
		})
	}
}

func TestReadDockerfileMetadata_ParsesAnalyzers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Dockerfile")
	content := "# sandman build-tools: go\n# sandman analyzers: gocognit@v1.2.1,gocyclo@v0.6.0\nFROM debian:bookworm-slim\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}
	meta, found, err := readDockerfileMetadata(path)
	if err != nil {
		t.Fatalf("readDockerfileMetadata: %v", err)
	}
	if !found {
		t.Fatal("expected metadata found")
	}
	if meta.Analyzers != "gocognit@v1.2.1,gocyclo@v0.6.0" {
		t.Errorf("Analyzers = %q, want %q", meta.Analyzers, "gocognit@v1.2.1,gocyclo@v0.6.0")
	}
}
func readDockerfileContent(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
