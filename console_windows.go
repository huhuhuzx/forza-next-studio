//go:build windows

package main

import (
	"os/exec"
	"syscall"
	"unsafe"
)

var (
	modKernel32Win          = syscall.NewLazyDLL("kernel32.dll")
	procSetConsoleTitleW    = modKernel32Win.NewProc("SetConsoleTitleW")
	procSetConsoleCtrlHdlr  = modKernel32Win.NewProc("SetConsoleCtrlHandler")
)

func setConsoleTitle(title string) {
	p, err := syscall.UTF16PtrFromString(title)
	if err != nil {
		return
	}
	_, _, _ = procSetConsoleTitleW.Call(uintptr(unsafe.Pointer(p)))
}

// installConsoleCloseHandler catches clicking the red X button on the console window
// (CTRL_CLOSE_EVENT / CTRL_LOGOFF_EVENT / CTRL_SHUTDOWN_EVENT) and runs onClose synchronously
// so APEX 6 LED lighting is restored and V4 child processes are terminated before exit!
func installConsoleCloseHandler(onClose func()) {
	cb := syscall.NewCallback(func(ctrlType uint32) uintptr {
		// 0=CTRL_C, 1=CTRL_BREAK are handled by Go os/signal;
		// 2=CTRL_CLOSE, 5=CTRL_LOGOFF, 6=CTRL_SHUTDOWN must run cleanup synchronously before Windows terminates us.
		if ctrlType < 2 {
			return 0
		}
		onClose()
		return 1
	})
	_, _, _ = procSetConsoleCtrlHdlr.Call(cb, 1)
}

func openBrowser(url string) {
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = cmd.Start()
}
