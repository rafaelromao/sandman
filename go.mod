module github.com/rafaelromao/sandman

go 1.25.0

// Patched standard library for builds and reachable-vulnerability scanning.
toolchain go1.26.9

require (
	github.com/spf13/cobra v1.10.2
	github.com/spf13/pflag v1.0.9
	golang.org/x/sys v0.46.0
	gopkg.in/yaml.v3 v3.0.1
)

require github.com/inconshreveable/mousetrap v1.1.0 // indirect
