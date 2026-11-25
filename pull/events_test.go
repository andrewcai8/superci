package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

// capture runs f with the default logger writing JSON lines to a buffer and returns each line as a map.
func capture(t *testing.T, f func()) []map[string]any {
	t.Helper()
	old := slog.Default()
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(old)
	f()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("not JSON: %s", l)
		}
		out = append(out, m)
	}
	return out
}

func schemaEvents(t *testing.T) map[string][]string {
	t.Helper()
	raw, err := os.ReadFile("timeline.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Version int `json:"version"`
		Events  map[string]struct {
			Fields []string `json:"fields"`
		} `json:"events"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	if s.Version != tlVersion {
		t.Fatalf("schema version %d, code writes %d", s.Version, tlVersion)
	}
	out := map[string][]string{}
	for k, v := range s.Events {
		out[k] = v.Fields
	}
	return out
}

// Every line the listener writes is an event of the schema, carries only fields the schema lists, and nothing the
// schema lists for the listener goes unwritten.
func TestListenerEventsMatchSchema(t *testing.T) {
	schema := schemaEvents(t)
	now := time.Date(2026, 11, 25, 17, 4, 40, 0, time.UTC)
	jobBase := scaleset.JobMessageBase{RunnerRequestID: 7, WorkflowRunID: 9, JobID: "g", JobDisplayName: "verify", OwnerName: "o", RepositoryName: "r",
		RequestLabels: []string{"superci-4cpu"}, QueueTime: now.Add(-time.Second), ScaleSetAssignTime: now, RunnerAssignTime: now, FinishTime: now}
	lines := capture(t, func() {
		tlMessage("superci-4cpu", &scaleset.RunnerScaleSetMessage{
			JobAvailableMessages: []*scaleset.JobAvailable{{JobMessageBase: jobBase}},
			JobAssignedMessages:  []*scaleset.JobAssigned{{JobMessageBase: jobBase}},
			JobStartedMessages:   []*scaleset.JobStarted{{RunnerID: 3, RunnerName: "superci-4cpu-i-0abc", JobMessageBase: jobBase}},
			JobCompletedMessages: []*scaleset.JobCompleted{{Result: "Succeeded", RunnerID: 3, RunnerName: "superci-4cpu-i-0abc", JobMessageBase: jobBase}},
		}, now)
		tlAcquired("superci-4cpu", []int64{7}, now)
		tlLaunched("superci-4cpu", "r-1", "spot", now.Add(-2*time.Second), []*Machine{{ID: "i-0abc", Region: "us-east-1", AZ: "us-east-1b", Type: "c7i.xlarge", Lifecycle: "spot"}})
		tlEnded("us-east-1", "idle", []string{"i-0abc"}, now)
		tlAsk("superci-4cpu", "i-0abc", Wait, now)
		tlAsk("superci-4cpu", "i-0abc", Wait, now.Add(3*time.Second))
		tlAsk("superci-4cpu", "i-0abc", Grant, now.Add(6*time.Second))
		tlAsk("superci-4cpu", "i-0def", End, now)
		tlClaim("superci-4cpu", "i-0abc", nil, now.Add(-30*time.Millisecond), now)
		tlClaim("superci-4cpu", "i-0abc", ErrClaimed, now, now)
		tlClaim("superci-4cpu", "i-0abc", errors.New("s3 down"), now, now)
		tlStart("superci-pull", now)
		snap := Snapshot{Set: "superci-4cpu", Pool: 2, Desired: 5}
		tlDecision(snap, "r-1", 3, 10, 0.5, now)
		tlPoolTarget(snap, now)
		tlRetired("superci-4cpu", &Machine{ID: "i-0abc", Launched: now.Add(-time.Minute)}, "overdue", now)
		tlIssued("superci-4cpu", "i-0abc", "superci-4cpu-i-0abc", 3, now.Add(-time.Second), now)
		tlRefused("superci-4cpu", "i-0abc", "exists", now.Add(-time.Second), now)
		tlNotice("superci-4cpu", &Machine{ID: "i-0abc", Region: "us-east-1", AZ: "us-east-1b", Type: "c7i.xlarge"}, now)
	})
	written := map[string]bool{}
	for _, l := range lines {
		ev, _ := l["ev"].(string)
		fields, ok := schema[ev]
		if !ok || l["msg"] != "tl" || l["v"] != float64(tlVersion) || l["t"] == nil {
			t.Fatalf("not a schema event: %v", l)
		}
		written[ev] = true
		allowed := map[string]bool{"msg": true, "v": true, "ev": true, "t": true, "t_ms": true, "boot": true, "eid": true, "time": true, "level": true}
		for _, f := range fields {
			allowed[f] = true
		}
		for k := range l {
			if !allowed[k] {
				t.Errorf("%s writes field %q, which the schema does not list", ev, k)
			}
		}
	}
	for ev := range schema {
		if !written[ev] && !markNames[ev] && ev != "ec2.state" {
			t.Errorf("schema event %s is never written by the listener", ev)
		}
	}
	if lines[0]["inst"] != nil || lines[2]["inst"] != "i-0abc" {
		t.Errorf("instance joins: available %v, started %v", lines[0]["inst"], lines[2]["inst"])
	}
	eids := map[any]bool{}
	for _, l := range lines {
		if eids[l["eid"]] || l["boot"] != tlBoot {
			t.Errorf("eid %v repeats or boot is not this process's: %v", l["eid"], l)
		}
		eids[l["eid"]] = true
	}
	var results []any
	for _, l := range lines {
		if l["ev"] == "jit.claim" {
			results = append(results, l["result"])
		}
	}
	if len(results) != 3 || results[0] != "taken" || results[1] != "refused" || results[2] != "error" {
		t.Errorf("claims: %v", results)
	}
	for _, l := range lines {
		if l["ev"] == "jit.granted" && (l["waited_ms"] != float64(6000) || l["polls"] != float64(3)) {
			t.Errorf("a machine that asked three times over 6 s: %v", l)
		}
	}
}

// A mark name the listener takes is an event of the schema from a machine, and the other way round.
func TestMarkNamesMatchSchema(t *testing.T) {
	raw, _ := os.ReadFile("timeline.schema.json")
	var s struct {
		Events map[string]struct {
			Src string `json:"src"`
		} `json:"events"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	for name := range markNames {
		if s.Events[name].Src != "machine" {
			t.Errorf("mark %s is not a machine event of the schema", name)
		}
	}
	for name, e := range s.Events {
		if e.Src == "machine" && !markNames[name] {
			t.Errorf("machine event %s is not a mark the listener takes", name)
		}
	}
}

func epoch(t time.Time) string { return strconv.FormatFloat(float64(t.UnixNano())/1e9, 'f', 3, 64) }

func TestMarkWritesEventsFromTheMachinesClock(t *testing.T) {
	now := time.Now()
	h := &Server{}
	lines := capture(t, func() {
		h.markEvents(&Set{spec: SetSpec{Name: "superci-4cpu"}}, &Machine{ID: "i-0abc"}, now,
			"boot.start=1700000000.5=0=12.3,cache.up="+epoch(now.Add(-2*time.Second))+"=1500,bogus="+epoch(now)+",doc.got=nope,runner.exit="+epoch(now)+"=5=0",
			epoch(now.Add(-100*time.Millisecond)))
	})
	if len(lines) != 2 {
		t.Fatalf("want cache.up and runner.exit only (boot.start is years off, bogus is no name, doc.got has no time): %v", lines)
	}
	if lines[0]["ev"] != "cache.up" || lines[0]["inst"] != "i-0abc" || lines[0]["dur_ms"] != float64(1500) || lines[1]["val"] != "0" || lines[1]["dur_ms"] != float64(5) {
		t.Errorf("%v", lines)
	}
	if sk := lines[0]["skew_ms"].(float64); sk < 90 || sk > 400 {
		t.Errorf("skew_ms %v, want about the 100 ms the request took", sk)
	}
}

func TestMarksOfOneMachineAreBounded(t *testing.T) {
	now := time.Now()
	h := &Server{}
	var marks []string
	for i := 0; i < markLimit+10; i++ {
		marks = append(marks, "runner.online="+epoch(now))
	}
	lines := capture(t, func() {
		h.markEvents(&Set{spec: SetSpec{Name: "s"}}, &Machine{ID: "i-bounded"}, now, strings.Join(marks, ","), "")
	})
	if len(lines) != markLimit {
		t.Fatalf("wrote %d marks, want the limit %d", len(lines), markLimit)
	}
}
