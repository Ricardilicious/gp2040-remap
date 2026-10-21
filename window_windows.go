//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/windows/registry"
)

// openWindow shows the app in its own window using Microsoft Edge's app mode
// (Edge comes with Windows 10 and 11), or Chrome if Edge isn't there. It uses
// a separate profile folder so the window is its own process; the returned
// channel closes when that window is closed. Returns nil if neither browser
// is found.
func openWindow(addr string) <-chan struct{} {
	exe := findBrowser()
	if exe == "" {
		return nil
	}
	profile := filepath.Join(os.TempDir(), "GP2040 Remap window")
	if d, err := os.UserCacheDir(); err == nil {
		profile = filepath.Join(d, "GP2040 Remap", "window")
	}
	cmd := exec.Command(exe,
		"--app="+addr,
		"--user-data-dir="+profile,
		"--window-size=1200,820",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-sync",
	)
	if err := cmd.Start(); err != nil {
		return nil
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	return done
}

func findBrowser() string {
	var candidates []string
	for _, name := range []string{"msedge.exe", "chrome.exe"} {
		for _, root := range []registry.Key{registry.CURRENT_USER, registry.LOCAL_MACHINE} {
			if k, err := registry.OpenKey(root, `SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\`+name, registry.QUERY_VALUE); err == nil {
				if v, _, err := k.GetStringValue(""); err == nil {
					candidates = append(candidates, v)
				}
				k.Close()
			}
		}
		sub := map[string]string{"msedge.exe": `Microsoft\Edge\Application`, "chrome.exe": `Google\Chrome\Application`}[name]
		for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles", "LOCALAPPDATA"} {
			if dir := os.Getenv(env); dir != "" {
				candidates = append(candidates, filepath.Join(dir, sub, name))
			}
		}
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return ""
}

func openBrowser(addr string) {
	exec.Command("rundll32", "url.dll,FileProtocolHandler", addr).Start()
}

// showInFolder opens Explorer with the file selected. Explorer wants the
// exact form /select,"C:\path\file", so the command line is built by hand.
func showInFolder(path string) {
	cmd := exec.Command("explorer.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `explorer.exe /select,"` + path + `"`}
	cmd.Start()
}
