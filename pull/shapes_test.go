package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

// githubStub answers the scale set client's HTTP calls: registration succeeds, and generatejitconfig answers the
// status, body and headers of the case, so the real client builds the error the listener reads.
type githubStub struct {
	status  int
	body    string
	ctype   string
	headers map[string]string
	jitHits atomic.Int32
}

func (s *githubStub) Do(req *http.Request) (*http.Response, error) {
	reply := func(code int, body string, h map[string]string, ct string) *http.Response {
		r := &http.Response{StatusCode: code, Status: fmt.Sprintf("%d %s", code, http.StatusText(code)), Header: http.Header{},
			Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)), Request: req}
		if ct != "" {
			r.Header.Set("Content-Type", ct)
		}
		for k, v := range h {
			r.Header.Set(k, v)
		}
		return r
	}
	switch p := req.URL.Path; {
	case strings.HasSuffix(p, "/actions/runners/registration-token"):
		return reply(201, `{"token":"t","expires_at":"2099-01-01T00:00:00Z"}`, nil, "application/json"), nil
	case strings.HasSuffix(p, "/actions/runner-registration"):
		enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
		jwt := enc(`{"alg":"HS256","typ":"JWT"}`) + "." + enc(fmt.Sprintf(`{"exp":%d}`, time.Now().Add(time.Hour).Unix())) + "." + enc("sig")
		return reply(200, fmt.Sprintf(`{"url":"https://actions.invalid/","token":%q}`, jwt), nil, "application/json"), nil
	case strings.HasSuffix(p, "/generatejitconfig"):
		s.jitHits.Add(1)
		return reply(s.status, s.body, s.headers, s.ctype), nil
	}
	return reply(404, "", nil, ""), nil
}

func exception(typeName, msg string) string {
	return fmt.Sprintf(`{"$id":"1","innerException":null,"message":%q,"typeName":%q,"typeKey":%q,"errorCode":0,"eventId":3000}`, msg, typeName, typeName)
}

type shape struct {
	name    string
	status  int
	body    string
	ctype   string
	headers map[string]string
}

func stubClient(t *testing.T, st *githubStub) *scaleset.Client {
	t.Helper()
	cl, err := scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
		GitHubConfigURL: "https://github.com/org", PersonalAccessToken: "pat", SystemInfo: scaleset.SystemInfo{System: "t", Version: "1", Subsystem: "t"},
	}, scaleset.WithHTTPClient(st))
	if err != nil {
		t.Fatal(err)
	}
	return cl
}

// An answer GitHub gives to a registration is a refusal unless it says the runner exists, the credential is bad or the
// scale set is gone, or we cancelled the ask. The listener cannot tell the rate-limit shapes GitHub uses from one
// another (the cap is documented, its refusal is not), so every other answer, a 5xx included, backs off: counted as a
// failure it left the listener asking GitHub about 300 times a minute.
func TestEveryAnswerButOursIsARefusalThatBacksOff(t *testing.T) {
	refusals := []shape{
		{"429 empty", 429, "", "", nil},
		{"429 json", 429, exception("RateLimitException", "slow down"), "application/json", nil},
		{"403 retry-after", 403, "{}", "application/json", map[string]string{"Retry-After": "30"}},
		{"403 primary remaining 0", 403, "{}", "application/json", map[string]string{"X-RateLimit-Remaining": "0"}},
		{"403 secondary rate limit", 403, exception("X", "You have exceeded a secondary rate limit"), "application/json", nil},
		{"403 text only", 403, exception("X", "API rate limit exceeded"), "application/json", nil},
		{"400 too many runners by typeName", 400, exception("GitHub.DistributedTask.WebApi.ConfigureTooManyRunnersException, GitHub.DistributedTask.WebApi", "x"), "application/json", nil},
		{"422 too many runners by typeName", 422, exception("ConfigureTooManyRunnersException", "x"), "application/json", nil},
		{"422 too many runners by message", 422, exception("TaskAgentPoolException", "You have exceeded the maximum number of runners that can be configured in 5 minutes"), "application/json", nil},
		{"422 other", 422, exception("SomeOtherException", "limit reached"), "application/json", nil},
		{"409 conflict", 409, exception("Conflict", "limit"), "application/json", nil},
		{"500", 500, exception("Boom", "internal"), "application/json", nil},
		{"502 html", 502, "<html>bad gateway</html>", "text/html", nil},
		{"503 retry-after", 503, "", "", map[string]string{"Retry-After": "20"}},
		{"504", 504, "", "", nil},
	}
	ours := []shape{
		{"runner exists", 409, exception("AgentExistsException", "exists"), "application/json", nil},
		{"unauthorized", 401, exception("Unauthorized", "bad token"), "application/json", nil},
		{"not found", 404, exception("ScaleSetNotFound", "gone"), "application/json", nil},
	}
	for _, c := range append(refusals, ours...) {
		wantRefused := !strings.Contains(c.name, "exists") && c.name != "unauthorized" && c.name != "not found"
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			st := &githubStub{status: c.status, body: c.body, ctype: c.ctype, headers: c.headers}
			b := NewBudget(builtIn(t), time.Now().Add(-time.Hour))
			h := burstProcess(stubClient(t, st), b, 1)
			code, _ := jitAsk(h, "i-0000")
			s := b.Snapshot(time.Now())
			if st.jitHits.Load() == 0 {
				t.Fatal("GitHub was never asked")
			}
			if wantRefused && (s.Refused != 1 || !s.Throttled || code != http.StatusTooManyRequests) {
				t.Fatalf("%d answered %d: %+v, want a counted refusal, every ask held and 429 to the machine", c.status, code, s)
			}
			if !wantRefused && (s.Refused != 0 || s.Throttled) {
				t.Fatalf("%d is no refusal, yet %+v", c.status, s)
			}
		})
	}
}

func TestOurOwnCancelIsNotARefusal(t *testing.T) {
	for _, err := range []error{context.Canceled, fmt.Errorf("request failed: %w", context.Canceled)} {
		if refusedRegistration(err) {
			t.Fatalf("%v counted as a refusal: a shutdown would hold every ask", err)
		}
	}
	if !refusedRegistration(errors.New("dial tcp: i/o timeout")) {
		t.Fatal("a transport error is not a refusal of ours")
	}
}
