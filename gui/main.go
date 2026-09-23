// Command unreal-agent-gui is a desktop GUI for the Unreal Agent harness.
//
// It runs harness coordinators in-process, one per chat, behind a loopback
// HTTP API that its native window (or a browser, with -browser) renders. The
// same binary is the search/fetch/read helper its built-in skills call.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"syscall"
	"time"
)

const defaultBrowserAddr = "127.0.0.1:7777"

type options struct {
	addr    string
	dataDir string
	browser bool
	noOpen  bool
	debug   bool
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "search", "fetch", "read":
			if err := runTool(os.Args[1], os.Args[2:], os.Stdout); err != nil {
				if !errors.Is(err, flag.ErrHelp) {
					fmt.Fprintf(os.Stderr, "%s: %v\n", os.Args[1], err)
				}
				os.Exit(1)
			}
			return
		}
	}
	var opts options
	flags := flag.NewFlagSet("unreal-agent-gui", flag.ExitOnError)
	flags.StringVar(&opts.addr, "addr", "", "loopback address for the local API (default: a free port, or "+defaultBrowserAddr+" with -browser)")
	flags.StringVar(&opts.dataDir, "data", defaultDataDir(), "directory for settings, sessions, and skills")
	flags.BoolVar(&opts.browser, "browser", !nativeWindowAvailable, "open the GUI in your web browser instead of a native window")
	flags.BoolVar(&opts.noOpen, "no-open", false, "serve the local API without opening a window")
	flags.BoolVar(&opts.debug, "debug", false, "log API requests and enable the window's web inspector")
	flags.Usage = func() {
		fmt.Fprintf(flags.Output(), `Usage:
  unreal-agent-gui [options]          start the desktop app
  unreal-agent-gui search <query>     web search (used by the web-research skill)
  unreal-agent-gui fetch <url>        page, PDF, or text as readable text
  unreal-agent-gui read <file>        text from PDF, Office, OpenDocument, and HTML files

Options:
`)
		flags.PrintDefaults()
	}
	flags.Parse(os.Args[1:])
	if os.Getenv("UAG_LOGIN_ENV") == "1" {
		importLoginEnvironment()
	}
	if err := run(opts); err != nil {
		if os.Getenv("UAG_LOGIN_ENV") == "1" {
			reportFatal(err)
		}
		log.Fatal(err)
	}
}

func defaultDataDir() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "unreal-agent-gui")
	}
	return ".unreal-agent-gui"
}

func run(opts options) error {
	native := !opts.browser && !opts.noOpen
	addr := opts.addr
	if addr == "" {
		addr = "127.0.0.1:0"
		if !native {
			addr = defaultBrowserAddr
		}
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("parse -addr: %w", err)
	}
	if !loopbackHost(host) {
		// The agent runs shell commands, so the API must not be reachable from the network.
		return fmt.Errorf("-addr must be a loopback address such as %s, got %q", defaultBrowserAddr, addr)
	}

	signals, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	ctx, cancel := context.WithCancel(signals)
	defer cancel()
	if err := os.MkdirAll(opts.dataDir, 0o700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	unlock, err := lockDataDir(opts.dataDir)
	if err != nil {
		return err
	}
	defer unlock()
	app, err := newApp(ctx, opts.dataDir)
	if err != nil {
		return err
	}

	listener, err := net.Listen("tcp", addr)
	if err != nil && addr == defaultBrowserAddr {
		listener, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		return err
	}
	handler := app.handler()
	if opts.debug {
		handler = logRequests(handler)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go server.Serve(listener)
	link := fmt.Sprintf("http://%s/?token=%s", listener.Addr(), app.token)
	shutdown := func() { app.shutdown(cancel, 5*time.Second) }

	if native {
		runWindow(ctx, link, opts.debug, shutdown)
	} else {
		fmt.Printf("Unreal Agent GUI is running at\n  %s\nData: %s\nPress Ctrl+C to quit.\n", link, opts.dataDir)
		if !opts.noOpen {
			openBrowser(link)
		}
		<-ctx.Done()
	}
	shutdown()
	closing, done := context.WithTimeout(context.Background(), 2*time.Second)
	defer done()
	return server.Shutdown(closing)
}

// lockDataDir keeps a second instance from sharing the session store.
func lockDataDir(dataDir string) (func(), error) {
	file, err := os.OpenFile(filepath.Join(dataDir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("Unreal Agent is already running with data directory %s", dataDir)
	}
	return func() { file.Close() }, nil
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}

func openBrowser(link string) {
	command := exec.Command("xdg-open", link)
	if goruntime.GOOS == "darwin" {
		command = exec.Command("open", link)
	}
	if err := command.Start(); err != nil {
		fmt.Printf("Open the link above in a browser (%v).\n", err)
	}
}

// importLoginEnvironment loads the user's login-shell environment. Apps
// started from Finder get a minimal PATH, which hides gh, git, python, and
// other tools the agent needs.
func importLoginEnvironment() {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/zsh"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, shell, "-l", "-i", "-c", "env -0").Output()
	if err != nil {
		return
	}
	for _, entry := range bytes.Split(output, []byte{0}) {
		name, value, ok := strings.Cut(string(entry), "=")
		if !ok || name == "" || name == "_" || name == "SHLVL" || name == "PWD" || name == "OLDPWD" {
			continue
		}
		os.Setenv(name, value)
	}
}
