package sandbox

import (
	"os"
	"strings"
)

// ImageBuildArgs shares the image-build authentication contract with prewarm
// builders. Only the environment variable name reaches argv; the runtime mounts
// its value transiently at /run/secrets/github_token for opted-in RUN steps.
func ImageBuildArgs(tag, dockerfile, contextDir string) []string {
	args := []string{"build", "-t", tag, "-f", dockerfile}
	for _, name := range []string{"MISE_GITHUB_TOKEN", "GITHUB_TOKEN", "GH_TOKEN"} {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			args = append(args, "--secret=id=github_token,src="+name+",type=env")
			break
		}
	}
	return append(args, contextDir)
}
