//go:build !darwin || !cgo

package main

import "context"

// The native window is macOS-only for now; elsewhere the GUI opens in a browser.
const nativeWindowAvailable = false

func runWindow(context.Context, string, bool, func()) {}

func reportFatal(error) {}
