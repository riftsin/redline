package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/riftsin/redline/internal/netinfo"
	"github.com/riftsin/redline/internal/paths"
	"github.com/riftsin/redline/internal/server"
)

const (
	startTimeout    = 20 * time.Second
	portFreeTimeout = 10 * time.Second
	probeTimeout    = 1500 * time.Millisecond
)

func baseURL() string {
	return "http://127.0.0.1:" + strconv.Itoa(netinfo.Port())
}

// stateID identifies this installation's state directory, so a CLI never
// adopts, replaces or stops a server that keeps its state elsewhere.
func stateID() (string, string, error) {
	dir, err := paths.StateDir()
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(dir))
	return hex.EncodeToString(sum[:])[:12], dir, nil
}

// BuildID distinguishes builds: a released version is its own identity; a
// local build ("(devel)", or a "+dirty" checkout) is identified by its
// executable, so a rebuild replaces the running server.
func BuildID() string {
	v := Version()
	if v != "(devel)" && !strings.Contains(v, "+dirty") {
		return v
	}
	exe, err := os.Executable()
	if err != nil {
		return v
	}
	info, err := os.Stat(exe)
	if err != nil {
		return v
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%d", exe, info.Size(), info.ModTime().UnixNano())))
	return v + "-build-" + hex.EncodeToString(sum[:])[:12]
}

var httpClient = &http.Client{}

// call sends a JSON request to the local server and decodes the JSON answer.
func call(ctx context.Context, method, path string, body, out any) (int, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL()+path, r)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, err
	}
	if out != nil {
		if err := json.Unmarshal(bytes.TrimSpace(data), out); err != nil {
			return resp.StatusCode, fmt.Errorf("unexpected response from the redline server (%d): %s", resp.StatusCode, strings.TrimSpace(string(data)))
		}
	}
	return resp.StatusCode, nil
}

func callWithTimeout(timeout time.Duration, method, path string, body, out any) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return call(ctx, method, path, body, out)
}

type probeResult int

const (
	notRunning probeResult = iota
	running                // this installation's server
	foreign                // something else owns the port
)

func probe() (probeResult, server.HealthResponse) {
	var h server.HealthResponse
	status, err := callWithTimeout(probeTimeout, "GET", "/health", nil, &h)
	if err != nil && status == 0 {
		return notRunning, h
	}
	if err != nil || status != http.StatusOK || h.App != server.AppName {
		return foreign, server.HealthResponse{}
	}
	id, _, err := stateID()
	if err != nil || h.StateID != id {
		return foreign, h
	}
	return running, h
}

func foreignError(h server.HealthResponse) *Error {
	owner := "another program"
	if h.App == server.AppName && h.StateDir != "" {
		owner = "another redline installation (state directory " + h.StateDir + ")"
	}
	return failure(fmt.Sprintf("port %d is used by %s", netinfo.Port(), owner),
		"set REDLINE_PORT to a free port for this installation")
}

// shouldReplace decides whether this CLI replaces a running server: only a
// newer CLI does, so two installed versions never keep restarting each
// other. A local (devel) build replaces any server built differently.
func shouldReplace(serverVersion, serverBuild, cliVersion, cliBuild string) bool {
	if serverBuild == cliBuild {
		return false
	}
	s, sok := parseSemver(serverVersion)
	c, cok := parseSemver(cliVersion)
	if !sok || !cok || strings.Contains(cliVersion, "+dirty") {
		return true
	}
	return compareSemver(c, s) > 0
}

// ensureServer makes sure this installation's server runs at a version at
// least as new as this CLI, starting or replacing it as needed.
func (e *env) ensureServer() error {
	result, h := probe()
	switch result {
	case foreign:
		return foreignError(h)
	case running:
		if !shouldReplace(h.Version, h.BuildID, Version(), BuildID()) {
			return nil
		}
		fmt.Fprintf(e.stderr, "redline: replacing the running server (%s) with this version (%s)\n", h.Version, Version())
		_, _ = callWithTimeout(3*time.Second, "POST", "/shutdown", map[string]string{"reason": "upgrade"}, nil)
		if err := awaitPortFree(); err != nil {
			return err
		}
	}
	if err := e.startServer(netinfo.Port()); err != nil {
		return failure("could not start the redline server: "+err.Error(), "check "+logHint()+" for details")
	}
	return awaitRunning()
}

func awaitRunning() error {
	deadline := time.Now().Add(startTimeout)
	for {
		switch result, h := probe(); result {
		case running:
			return nil
		case foreign:
			return foreignError(h)
		}
		if time.Now().After(deadline) {
			return failure(fmt.Sprintf("the redline server did not start on port %d", netinfo.Port()),
				"check "+logHint()+" for details", "set REDLINE_PORT to use another port")
		}
		time.Sleep(150 * time.Millisecond)
	}
}

func awaitPortFree() error {
	deadline := time.Now().Add(portFreeTimeout)
	for {
		if result, _ := probe(); result == notRunning {
			return nil
		}
		if time.Now().After(deadline) {
			return failure("the running redline server did not stop", "run `redline stop`, then retry")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func logHint() string {
	dir, err := paths.StateDir()
	if err != nil {
		return "the redline server log"
	}
	return logPath(dir)
}

func logPath(stateDir string) string {
	return stateDir + string(os.PathSeparator) + "server.log"
}

// serverError turns an unexpected answer into an error.
func serverError(status int, err error) error {
	if err != nil {
		return failure(err.Error(), "run `redline stop`, then retry")
	}
	return failure(fmt.Sprintf("unexpected response from the redline server (HTTP %d)", status), "run `redline stop`, then retry")
}

// apiError reads the error a JSON endpoint returned.
type apiError struct {
	Status   string `json:"status"`
	Code     string `json:"code"`
	Error    string `json:"error"`
	EndedBy  string `json:"ended_by"`
	Listener string `json:"listener"`
}

// -- Versions ------------------------------------------------------------------

type semver struct {
	major, minor, patch int
	pre                 string
}

func parseSemver(v string) (semver, bool) {
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexByte(v, '+'); i >= 0 {
		v = v[:i]
	}
	core, pre, _ := strings.Cut(v, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return semver{}, false
	}
	var n [3]int
	for i, p := range parts {
		x, err := strconv.Atoi(p)
		if err != nil || x < 0 {
			return semver{}, false
		}
		n[i] = x
	}
	return semver{n[0], n[1], n[2], pre}, true
}

func compareSemver(a, b semver) int {
	for _, d := range [][2]int{{a.major, b.major}, {a.minor, b.minor}, {a.patch, b.patch}} {
		if d[0] != d[1] {
			if d[0] > d[1] {
				return 1
			}
			return -1
		}
	}
	switch {
	case a.pre == b.pre:
		return 0
	case a.pre == "":
		return 1 // a release outranks its prereleases
	case b.pre == "":
		return -1
	case a.pre > b.pre:
		return 1
	}
	return -1
}
