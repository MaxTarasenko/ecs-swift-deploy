package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionDoesNotRequireAWSArguments(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"--version"}, &out, &errOut); code != 0 {
		t.Fatalf("code=%d err=%s", code, errOut.String())
	}
	if !strings.HasPrefix(out.String(), "ecs-swift-deploy "+strings.TrimSpace(sourceVersion)+" (commit=") {
		t.Fatalf("unexpected version: %s", out.String())
	}
}
