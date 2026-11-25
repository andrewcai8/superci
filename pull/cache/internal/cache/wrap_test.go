package cache

import (
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestResultsURLIsChangedOnlyForAGitHubJob(t *testing.T) {
	here := "http://10.0.0.5:8257"
	cases := []struct{ in, want []string }{
		{[]string{"A=1", "ACTIONS_RESULTS_URL=https://results-receiver.actions.githubusercontent.com/"}, []string{"A=1", "ACTIONS_RESULTS_URL=http://10.0.0.5:8257/"}},
		{[]string{"ACTIONS_RESULTS_URL=http://10.0.0.5:8257/"}, []string{"ACTIONS_RESULTS_URL=http://10.0.0.5:8257/"}},
		{[]string{"ACTIONS_RESULTS_URL="}, []string{"ACTIONS_RESULTS_URL="}},
		{[]string{"A=1"}, []string{"A=1"}},
	}
	for _, c := range cases {
		if got := withResultsURL(append([]string(nil), c.in...), here); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%v: got %v, want %v", c.in, got, c.want)
		}
	}
}

func TestTheAddressIsUsedOnlyWhileSomethingListensThere(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, AddressFile)
	if serverAddress(file) != "" {
		t.Fatal("an address without a file")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(file, []byte("http://"+l.Addr().String()+"\n"), 0o644)
	if got := serverAddress(file); got != "http://"+l.Addr().String() {
		t.Fatalf("address = %q", got)
	}
	l.Close()
	if serverAddress(file) != "" {
		t.Fatal("an address nothing listens at was used")
	}
}

func TestWrapPutsTheRealNodeBesideItAndIsRepeatable(t *testing.T) {
	ext := t.TempDir()
	for _, n := range []string{"node20", "node24"} {
		os.MkdirAll(filepath.Join(ext, n, "bin"), 0o755)
		os.WriteFile(filepath.Join(ext, n, "bin", "node"), []byte("real "+n), 0o755)
	}
	os.MkdirAll(filepath.Join(ext, "other"), 0o755)
	for range 2 {
		if err := Wrap(ext); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range []string{"node20", "node24"} {
		if b, _ := os.ReadFile(filepath.Join(ext, n, "bin", "node.real")); string(b) != "real "+n {
			t.Errorf("%s: the real Node is %q", n, b)
		}
		if b, _ := os.ReadFile(filepath.Join(ext, n, "bin", "node")); string(b) == "real "+n || len(b) == 0 {
			t.Errorf("%s: node was not replaced", n)
		}
	}
	if err := Wrap(t.TempDir()); err == nil {
		t.Fatal("wrapping a folder with no Node said it worked")
	}
}
