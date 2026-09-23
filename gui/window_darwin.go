//go:build darwin && cgo

package main

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework Cocoa
#import <Cocoa/Cocoa.h>

extern void uagWillTerminate(void);

// webview creates the NSApplication but no menu bar, and WKWebView needs the
// Edit menu's responder actions for copy, paste, and undo shortcuts.
static void uagInstallMenu(void) {
	NSString *name = @"Unreal Agent";
	NSMenu *bar = [[NSMenu alloc] init];

	NSMenu *app = [[NSMenu alloc] initWithTitle:name];
	[app addItemWithTitle:[@"Hide " stringByAppendingString:name] action:@selector(hide:) keyEquivalent:@"h"];
	[app addItem:[NSMenuItem separatorItem]];
	// Closing the window shuts down cleanly; see windowWillClose in webview.
	[app addItemWithTitle:[@"Quit " stringByAppendingString:name] action:@selector(performClose:) keyEquivalent:@"q"];
	NSMenuItem *appItem = [[NSMenuItem alloc] init];
	appItem.submenu = app;
	[bar addItem:appItem];

	NSMenu *edit = [[NSMenu alloc] initWithTitle:@"Edit"];
	[edit addItemWithTitle:@"Undo" action:@selector(undo:) keyEquivalent:@"z"];
	NSMenuItem *redo = [edit addItemWithTitle:@"Redo" action:@selector(redo:) keyEquivalent:@"z"];
	redo.keyEquivalentModifierMask = NSEventModifierFlagCommand | NSEventModifierFlagShift;
	[edit addItem:[NSMenuItem separatorItem]];
	[edit addItemWithTitle:@"Cut" action:@selector(cut:) keyEquivalent:@"x"];
	[edit addItemWithTitle:@"Copy" action:@selector(copy:) keyEquivalent:@"c"];
	[edit addItemWithTitle:@"Paste" action:@selector(paste:) keyEquivalent:@"v"];
	[edit addItemWithTitle:@"Select All" action:@selector(selectAll:) keyEquivalent:@"a"];
	NSMenuItem *editItem = [[NSMenuItem alloc] init];
	editItem.submenu = edit;
	[bar addItem:editItem];

	NSMenu *window = [[NSMenu alloc] initWithTitle:@"Window"];
	[window addItemWithTitle:@"Minimize" action:@selector(performMiniaturize:) keyEquivalent:@"m"];
	[window addItemWithTitle:@"Zoom" action:@selector(performZoom:) keyEquivalent:@""];
	NSMenuItem *windowItem = [[NSMenuItem alloc] init];
	windowItem.submenu = window;
	[bar addItem:windowItem];

	[NSApp setMainMenu:bar];
	[NSApp setWindowsMenu:window];

	// Quitting from the Dock or at logout bypasses the window; stop runs first.
	[[NSNotificationCenter defaultCenter] addObserverForName:NSApplicationWillTerminateNotification
		object:nil queue:nil usingBlock:^(NSNotification *note) { uagWillTerminate(); }];
}
*/
import "C"

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"

	webview "github.com/webview/webview_go"
)

const nativeWindowAvailable = true

// beforeTerminate runs when macOS terminates the app without closing the window.
var beforeTerminate = func() {}

//export uagWillTerminate
func uagWillTerminate() {
	beforeTerminate()
}

func init() {
	// Cocoa must run on the main thread, which the main goroutine starts on.
	runtime.LockOSThread()
}

// runWindow shows the GUI in a native window and blocks until it closes or
// ctx is canceled.
func runWindow(ctx context.Context, link string, debug bool, shutdown func()) {
	window := webview.New(debug)
	defer window.Destroy()
	beforeTerminate = shutdown
	window.SetTitle("Unreal Agent")
	window.SetSize(880, 560, webview.HintMin)
	window.SetSize(1360, 860, webview.HintNone)
	window.Dispatch(func() { C.uagInstallMenu() })
	window.Bind("uagOpenExternal", openExternal)
	window.Navigate(link)
	go func() {
		<-ctx.Done()
		window.Dispatch(window.Terminate)
	}()
	window.Run()
}

// openExternal opens web links in the default browser; the WebView has no tabs.
func openExternal(target string) error {
	lower := strings.ToLower(target)
	if !strings.HasPrefix(lower, "https://") && !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "mailto:") {
		return fmt.Errorf("refusing to open %q", target)
	}
	return exec.Command("open", target).Start()
}

// reportFatal shows startup errors when there is no terminal to print them to.
func reportFatal(err error) {
	script := fmt.Sprintf("display alert %q message %q as critical", "Unreal Agent could not start", err.Error())
	exec.Command("osascript", "-e", script).Run()
}
