package main

import (
	_ "embed"
	"fmt"
	"strings"
)

//go:embed VERSION
var sourceVersion string

// Set by the release build with -ldflags.
var commit = "unknown"
var buildDate = "unknown"

func versionString() string {
	return fmt.Sprintf("ecs-swift-deploy %s (commit=%s built=%s)", strings.TrimSpace(sourceVersion), commit, buildDate)
}
