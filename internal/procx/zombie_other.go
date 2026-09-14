//go:build !darwin

package procx

import (
	"os/exec"
	"strconv"
	"strings"
)

// zombie asks ps for the process state; Z marks a zombie.
func zombie(pid int) bool {
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}
