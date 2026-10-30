package sandbox

import (
	"strings"
	"testing"
)

func TestImageBuildCredentialsStayOutOfArguments(t *testing.T) {
	for _, selected := range []string{"", "GH_TOKEN", "GITHUB_TOKEN", "MISE_GITHUB_TOKEN"} {
		t.Run(selected, func(t *testing.T) {
			for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN", "MISE_GITHUB_TOKEN"} {
				t.Setenv(name, "")
			}
			if selected != "" {
				t.Setenv(selected, "fixture-credential-do-not-persist")
			}
			args := ImageBuildArgs("image", "Dockerfile", "context")
			joined := strings.Join(args, " ")
			if strings.Contains(joined, "fixture-credential") || strings.Contains(joined, "--build-arg") {
				t.Fatal("credential must use a transient secret, never argv or build args")
			}
			if selected == "" && strings.Contains(joined, "--secret") {
				t.Fatal("unauthenticated local builds must remain supported")
			}
			if selected != "" && !strings.Contains(joined, "--secret=id=github_token,src="+selected+",type=env") {
				t.Fatalf("missing environment-backed secret: %v", args)
			}
			if args[len(args)-1] != "context" {
				t.Fatal("build context must follow secret options")
			}
		})
	}
}

func TestImageBuildMiseCredentialSurvivesGitHubTestShims(t *testing.T) {
	t.Setenv("MISE_GITHUB_TOKEN", "real-build-credential")
	t.Setenv("GITHUB_TOKEN", "fake")
	t.Setenv("GH_TOKEN", "fake")
	args := strings.Join(ImageBuildArgs("image", "Dockerfile", "context"), " ")
	if !strings.Contains(args, "src=MISE_GITHUB_TOKEN,") || strings.Contains(args, "real-build-credential") {
		t.Fatal("build authentication must use the dedicated mise token without disclosing it")
	}
}
