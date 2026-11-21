//go:build !darwin && !linux

package adapter

import "os/exec"

func configureDocumentProcess(cmd *exec.Cmd) {}
