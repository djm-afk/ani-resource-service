package main

import (
	"os/exec"
	"strings"
	"testing"
)

func TestClusterReadProgramParsesWithoutExecution(t *testing.T) {
	command := exec.Command("python3", "-c", "import ast,sys;ast.parse(sys.stdin.read())")
	command.Stdin = strings.NewReader(clusterReadScript)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("read-only SSH verifier is invalid Python: %v %s", err, output)
	}
}
