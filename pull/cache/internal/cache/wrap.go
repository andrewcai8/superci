package cache

import (
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// AddressFile is where the server writes the address it listens on, in the runner's externals folder (a job's
// containers see that folder at /__e, so a wrapped Node in one finds it too).
const AddressFile = "pullcache.address"

const realNode = "node.real"

// Wrap puts this program in front of each of the runner's Nodes (externals/node*/bin/node), which run every
// JavaScript action. The runner gives a step ACTIONS_RESULTS_URL itself, after anything set for the runner, so
// this is the one place it can be changed for every action at once. Run again, it only replaces this program.
func Wrap(externals string) error {
	me, err := os.Executable()
	if err != nil {
		return err
	}
	dirs, err := os.ReadDir(externals)
	if err != nil {
		return err
	}
	wrapped := 0
	for _, d := range dirs {
		if !strings.HasPrefix(d.Name(), "node") {
			continue
		}
		bin := filepath.Join(externals, d.Name(), "bin")
		node, real := filepath.Join(bin, "node"), filepath.Join(bin, realNode)
		if _, err := os.Stat(real); err != nil {
			if fi, err := os.Stat(node); err != nil || !fi.Mode().IsRegular() {
				continue
			}
			if err := os.Rename(node, real); err != nil {
				return err
			}
		}
		if err := copyExecutable(me, node); err != nil {
			return err
		}
		wrapped++
	}
	if wrapped == 0 {
		return fmt.Errorf("no Node in %s", externals)
	}
	return nil
}

func copyExecutable(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := to + ".pullcache"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, to)
}

// RunAsNode is what a wrapped Node does: become the real one, with the action's cache service here when the server
// is up (and not here already). Anything else is as it was. It returns only when the real Node cannot be started.
func RunAsNode(args []string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	dir := filepath.Dir(exe)
	real := filepath.Join(dir, realNode)
	env := os.Environ()
	if here := serverAddress(filepath.Join(dir, "..", "..", AddressFile)); here != "" {
		env = withResultsURL(env, here)
	}
	return syscall.Exec(real, args, env)
}

// serverAddress is the address in the file when something is listening there.
func serverAddress(file string) string {
	b, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	here := strings.TrimSpace(string(b))
	u, err := url.Parse(here)
	if err != nil || u.Host == "" {
		return ""
	}
	c, err := net.DialTimeout("tcp", u.Host, 300*time.Millisecond)
	if err != nil {
		return ""
	}
	c.Close()
	return here
}

func withResultsURL(env []string, here string) []string {
	const name = "ACTIONS_RESULTS_URL="
	for i, e := range env {
		if v, ok := strings.CutPrefix(e, name); ok {
			// Only a job of GitHub's (an http address), and not one already pointed here.
			if strings.HasPrefix(v, "http") && !strings.HasPrefix(v, here) {
				env[i] = name + here + "/"
			}
			return env
		}
	}
	return env
}
