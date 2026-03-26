package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
)

func installSudoers() {
	u, err := user.Current()
	if err != nil {
		fatalf("Failed to determine current user: %v", err)
	}

	inhibitPath, err := exec.LookPath("systemd-inhibit")
	if err != nil {
		inhibitPath = "/usr/bin/systemd-inhibit"
	}

	rule := fmt.Sprintf("%s ALL=(root) NOPASSWD: %s --what=sleep --why=* --mode=block *\n",
		u.Username, inhibitPath)

	tmp, err := os.CreateTemp("", "block-sleep-sudoers-")
	if err != nil {
		fatalf("Failed to create temp file: %v", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.WriteString(rule); err != nil {
		tmp.Close()
		fatalf("Failed to write temp file: %v", err)
	}
	tmp.Close()
	os.Chmod(tmpPath, 0440)

	out, err := exec.Command("sudo", "visudo", "-c", "-f", tmpPath).CombinedOutput()
	if err != nil {
		fatalf("Sudoers validation failed: %s\n%v", out, err)
	}

	sudoersPath := fmt.Sprintf("/etc/sudoers.d/block-sleep-%s", u.Username)

	if err := exec.Command("sudo", "cp", tmpPath, sudoersPath).Run(); err != nil {
		fatalf("Failed to install sudoers file: %v", err)
	}
	if err := exec.Command("sudo", "chmod", "0440", sudoersPath).Run(); err != nil {
		fatalf("Failed to set permissions: %v", err)
	}

	fmt.Printf("Installed %s\n", sudoersPath)
	fmt.Printf("Rule: %s", rule)
}
