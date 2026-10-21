//go:build !windows

package main

import (
	"os/exec"
	"path/filepath"
)

func openWindow(addr string) <-chan struct{} { return nil }

func openBrowser(addr string) {
	_ = exec.Command("xdg-open", addr).Start()
}

func showInFolder(path string) { _ = exec.Command("xdg-open", filepath.Dir(path)).Start() }
