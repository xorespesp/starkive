//go:build !windows

package main

// Does nothing, because a terminal outside Windows stays open after the setup tool exits.
func waitForEnterIfConsoleCloses() {}
