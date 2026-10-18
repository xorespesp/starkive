package main

import (
	"bufio"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// Waits for Enter if the console window closes when this process exits (for example, started by double-clicking it).
func waitForEnterIfConsoleCloses() {
	var ids [2]uint32
	count, _, _ := syscall.NewLazyDLL("kernel32.dll").NewProc("GetConsoleProcessList").Call(uintptr(unsafe.Pointer(&ids[0])), uintptr(len(ids)))
	// 1 means only this process is attached to the console, so the console closes with it (measured)
	if count != 1 {
		return
	}
	fmt.Print("\nPress Enter to close.")
	bufio.NewReader(os.Stdin).ReadString('\n')
}
