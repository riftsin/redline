package cli

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"

	"github.com/riftsin/redline/internal/attachments"
	"github.com/riftsin/redline/internal/netinfo"
	"github.com/riftsin/redline/internal/review"
	"github.com/riftsin/redline/internal/server"
	"github.com/riftsin/redline/internal/store"
)

// spawnServer launches `redline server` in the background, appending its
// output to the state directory's server.log.
func spawnServer(port int) error {
	_, dir, err := stateID()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logFile, err := os.OpenFile(logPath(dir), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()

	cmd := exec.Command(exe, "server")
	cmd.Env = append(os.Environ(), "REDLINE_PORT="+strconv.Itoa(port))
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Dir = dir
	cmd.SysProcAttr = detached()
	if err := cmd.Start(); err != nil {
		return err
	}
	// The server outlives us; do not wait for it.
	return cmd.Process.Release()
}

// serverCommand runs the server in the foreground. The CLI starts it in the
// background; running it by hand is useful for debugging.
func serverCommand(e *env, args []string) (string, error) {
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	if _, err := parse(fs, args); err != nil {
		return "", err
	}
	id, dir, err := stateID()
	if err != nil {
		return "", failure(err.Error())
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", failure(err.Error())
	}
	log.SetFlags(log.Ldate | log.Ltime | log.LUTC)
	log.SetPrefix(fmt.Sprintf("redline[%d] ", os.Getpid()))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, filepath.Join(dir, "redline.db"))
	if err != nil {
		return "", failure(err.Error())
	}
	defer st.Close()
	m := review.NewManager(st, review.DisconnectGraceFromEnv())
	defer m.Close()
	att, err := attachments.Open(filepath.Join(dir, "attachments"), st, attachments.FromEnv(os.Getenv))
	if err != nil {
		return "", failure("cannot prepare the image store: " + err.Error())
	}
	m.Attachments = att
	go att.RunSweeper(ctx)

	plan := netinfo.Resolve(os.Getenv, netinfo.TailscaleStatus)
	if plan.Warning != "" {
		log.Print(plan.Warning)
	}
	port := netinfo.Port()
	srv := server.New(m, att, plan, port, server.Info{Version: Version(), BuildID: BuildID(), StateID: id, StateDir: dir})
	log.Printf("starting %s (%s, %s/%s) on port %d, links at %s", Version(), BuildID(), runtime.GOOS, runtime.GOARCH, port, plan.BaseURL(port))
	if err := srv.Serve(ctx); err != nil {
		log.Printf("server stopped: %v", err)
		return "", failure("the server could not start: " + err.Error())
	}
	log.Print("stopped")
	return "", nil
}
