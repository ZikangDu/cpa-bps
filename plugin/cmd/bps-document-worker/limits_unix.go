//go:build darwin || linux

package main

import "syscall"

func hardenWorker() {
	// CPU and output limits apply to the helper and inherited PDF/XLS processes.
	_ = syscall.Setrlimit(syscall.RLIMIT_CPU, &syscall.Rlimit{Cur: 35, Max: 40})
	_ = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: 96 << 20, Max: 96 << 20})
	_ = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &syscall.Rlimit{Cur: 128, Max: 128})
}
