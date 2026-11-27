package main

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
)

const (
	// What a machine's script is rendered with in production: the identity's pin is "sha256//" and the 44 characters of
	// a SHA-256's base64 (identity.go), the listener the address of an elastic IP, here the longest a dotted quad is.
	realPin      = "sha256//AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	realListener = "https://255.255.255.255:8443"

	ec2UserDataLimit = 16 * 1024
	// A boot script may use this much of the limit once compressed: the next change then has room for the rest.
	userDataBudget = ec2UserDataLimit * 6 / 10
)

// launchUserData is the raw user data (before EC2's base64) a launch template made for this script holds.
func launchUserData(t *testing.T, script string) []byte {
	t.Helper()
	cfg := shippedConfig(t)
	r := &Region{Name: "us-east-2", image: "ami-1", root: "/dev/sda1"}
	data := templateData(r, script, "sg-1", 30, cfg.Sets()[0], nil)
	raw, err := base64.StdEncoding.DecodeString(aws.ToString(data.UserData))
	if err != nil {
		t.Fatalf("the template's user data is not base64: %v", err)
	}
	return raw
}

func gzipped(t *testing.T, script string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(script)); err != nil || zw.Close() != nil {
		t.Fatal("gzip failed")
	}
	return buf.Bytes()
}

func gunzipped(t *testing.T, raw []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("the user data is not gzip (starts % x): %v", raw[:min(8, len(raw))], err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestEveryModesLaunchTemplateGetsGzipUserDataThatUnzipsToTheScript(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"scaleset", "dispatch"} {
		script := bootDataMode(cfg, realListener, realPin, "superci-4cpu", mode)
		raw := launchUserData(t, script)
		t.Logf("%s: %d bytes of script, %d bytes of user data", mode, len(script), len(raw))
		if len(raw) < 2 || raw[0] != 0x1f || raw[1] != 0x8b {
			t.Fatalf("%s: user data does not start with gzip's magic bytes (starts % x), so cloud-init would run it as text", mode, raw[:min(4, len(raw))])
		}
		if len(raw) > userDataBudget {
			t.Errorf("%s: user data is %d bytes; at most %d of EC2's %d", mode, len(raw), userDataBudget, ec2UserDataLimit)
		}
		if got := gunzipped(t, raw); got != script {
			t.Errorf("%s: the user data unzips to something other than the rendered script", mode)
		}
		first, _, _ := strings.Cut(script, "\n")
		if !strings.HasPrefix(first, "#!") || !strings.HasPrefix(gunzipped(t, raw), first+"\n") {
			t.Errorf("%s: the script's first line %q is not what cloud-init reads after unzipping", mode, first)
		}
	}
}

func TestTheSameScriptIsTheSameUserDataAndAChangedOneIsNew(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	script := bootDataMode(cfg, realListener, realPin, "superci-4cpu", "dispatch")
	a, b := launchUserData(t, script), launchUserData(t, script)
	if !bytes.Equal(a, b) {
		t.Fatal("one script gave two different user data")
	}
	have, want := templatePair()
	have.UserData = aws.String(base64.StdEncoding.EncodeToString(a))
	want.UserData = aws.String(base64.StdEncoding.EncodeToString(b))
	if !sameData(have, want) {
		t.Fatal("an unchanged script makes a new template version")
	}
	changed := launchUserData(t, strings.Replace(script, "poweroff", "poweroff ", 1))
	want.UserData = aws.String(base64.StdEncoding.EncodeToString(changed))
	if sameData(have, want) {
		t.Fatal("a script changed by one byte kept the old template version")
	}
	// The latest version of a template made before the data was zipped holds the script as text: the same script is
	// the same version, so the start that ships the gzip makes none for it, and another script is a new one.
	have.UserData = aws.String(base64.StdEncoding.EncodeToString([]byte(script)))
	want.UserData = aws.String(base64.StdEncoding.EncodeToString(gzipped(t, script)))
	if !sameData(have, want) {
		t.Fatal("a version holding the script as text differs from the same script zipped")
	}
	have.UserData = aws.String(base64.StdEncoding.EncodeToString([]byte(script + "\n")))
	if sameData(have, want) {
		t.Fatal("a version holding another script as text was kept")
	}
}

func TestAnotherGzipOfTheSameScriptIsTheSameVersion(t *testing.T) {
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	zw.Name = "boot.sh"
	zw.Write([]byte("#!/bin/bash\necho hi\n"))
	zw.Close()
	have, want := templatePair()
	have.UserData = aws.String(base64.StdEncoding.EncodeToString(buf.Bytes()))
	want.UserData = aws.String(encodeUserData("#!/bin/bash\necho hi\n"))
	if !sameData(have, want) {
		t.Fatal("a Go release that zips differently would make a version for every set")
	}
}

// What a machine reads from its user data through IMDS arrives zipped: a reader that greps it must unzip first. On main
// none reads it (boot.sh and agent.py use IMDS for the token, region, instance id, identity document and spot notice);
// this fails a machine-side script that starts to, with the unzip it needs named.
var userDataRead = regexp.MustCompile(`user-data`)
var userDataUnzip = regexp.MustCompile(`gunzip|zcat|gzip -d|gzip\.|zlib`)

func TestNoMachineSideScriptReadsItsUserDataWithoutUnzippingIt(t *testing.T) {
	var scanned int
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "deploy", "tools", "testdata", "bench", "certs", ".git":
				return filepath.SkipDir
			}
			return nil
		}
		switch filepath.Ext(path) {
		case ".sh", ".py", ".go":
		default:
			return nil
		}
		if strings.HasSuffix(path, "_test.go") || filepath.Base(path) == "test_agent.py" {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		if userDataRead.Match(raw) && !userDataUnzip.Match(raw) {
			t.Errorf("%s reads the machine's user data, which is gzip: unzip it first (gunzip or zcat)", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 4 {
		t.Fatalf("scanned %d scripts: the walk found none of the machine's", scanned)
	}
	// The guard bites: the job-start block of the parked branch read it as text.
	if !userDataRead.MatchString(`ud=$(md user-data)`) || userDataUnzip.MatchString(`ud=$(md user-data)`) {
		t.Fatal("the guard does not tell a reader that never unzips")
	}
}
