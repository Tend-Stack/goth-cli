package doctor

import (
	"os"
	"os/exec"
)

// Version probes have fixed arguments; CommandContext bounds the direct process.
// Native Windows process-tree acceptance remains required before release support.
func boundProcess(cmd *exec.Cmd) {}

func propertyFlags() int { return os.O_RDONLY }
