package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// runDisk runs boot.sh's disk function in bash with findmnt and lsblk answering for a root on part of disk (its size
// 30083776512 bytes before it grows), and the commands that change the machine recording what they were asked. It returns
// those calls and the ROOT_BYTES the function exported for the pre-read.
func runDisk(t *testing.T, part, disk string) ([]string, string) {
	t.Helper()
	src, err := os.ReadFile("boot.sh")
	if err != nil {
		t.Fatal(err)
	}
	fn := regexp.MustCompile(`(?s)\ndisk\(\) \{\n.*?\n\}\ndisk\n`).Find(src)
	if fn == nil {
		t.Fatal("boot.sh has no disk function that it calls")
	}
	dir := t.TempDir()
	script := `log=$1/calls
findmnt() { echo "$PART"; }
lsblk() { case "$*" in *SIZE*) echo 30083776512 ;; *) echo "$DISK" ;; esac; }
mount() { echo "mount $*" >> $log; }
growpart() { echo "growpart $*" >> $log; }
resize2fs() { echo "resize2fs $*" >> $log; }
` + string(fn) + `wait
bash -c 'printf %s "$ROOT_BYTES"' > $1/root
`
	cmd := exec.Command("bash", "-c", script, "_", dir)
	cmd.Env = append(os.Environ(), "PART="+part, "DISK="+disk)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "calls"))
	root, _ := os.ReadFile(filepath.Join(dir, "root"))
	return strings.Split(strings.TrimSpace(string(raw)), "\n"), string(root)
}

func TestTheRootGrowsToTheVolumeAndARenameNoLongerWritesAFileOut(t *testing.T) {
	for _, c := range []struct{ part, disk, num string }{{"/dev/nvme0n1p1", "nvme0n1", "1"}, {"/dev/xvda1", "xvda", "1"}, {"/dev/nvme1n1p16", "nvme1n1", "16"}} {
		calls, root := runDisk(t, c.part, c.disk)
		if root != "30083776512" {
			t.Errorf("root %s: the size exported for the pre-read is %q, want the root's size before it grows", c.part, root)
		}
		want := []string{"mount -o remount,noauto_da_alloc /", "growpart /dev/" + c.disk + " " + c.num, "resize2fs " + c.part}
		if strings.Join(calls, "|") != strings.Join(want, "|") {
			t.Errorf("root %s: ran %q, want %q", c.part, calls, want)
		}
	}
}

func TestAptReadsTheRegionsMirror(t *testing.T) {
	src, err := os.ReadFile("boot.sh")
	if err != nil {
		t.Fatal(err)
	}
	fn := regexp.MustCompile(`(?s)\nmirror\(\) \{\n.*?\n\}\nmirror "\$CR"\n`).Find(src)
	if fn == nil {
		t.Fatal(`boot.sh has no mirror function that it calls with the region`)
	}
	sources := filepath.Join(t.TempDir(), "ubuntu.sources")
	image := "Types: deb\nURIs: https://archive.ubuntu.com/ubuntu/\nSuites: noble noble-updates noble-backports\n\nTypes: deb\nURIs: https://security.ubuntu.com/ubuntu/\nSuites: noble-security\n"
	if err := os.WriteFile(sources, []byte(image), 0o644); err != nil {
		t.Fatal(err)
	}
	script := strings.TrimSuffix(string(fn), "mirror \"$CR\"\n") + `mirror us-east-2 "$1"` + "\n"
	if out, err := exec.Command("bash", "-c", script, "_", sources).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	got, _ := os.ReadFile(sources)
	want := strings.NewReplacer("https://archive.ubuntu.com/ubuntu/", "http://us-east-2.ec2.archive.ubuntu.com/ubuntu/",
		"https://security.ubuntu.com/ubuntu/", "http://us-east-2.ec2.archive.ubuntu.com/ubuntu/").Replace(image)
	if string(got) != want {
		t.Errorf("sources after boot:\n%s\nwant:\n%s", got, want)
	}
}
