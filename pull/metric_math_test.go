package main

import (
	"math"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// This file evaluates the CloudWatch metric math of an alarm in alarms.sh at one timestamp, over a world of series values, so
// a test can say what an alarm reads when identities are parked or a host is down, not only which metrics it names. It knows
// what alarms.sh's identity alarms use: numbers, + - * /, < > <= >=, parentheses, FILL(x, n) and IF(c, a, b). A series that
// has no value at the timestamp is NaN: it propagates through arithmetic and comparison, and FILL(x, n) turns it into n, as
// CloudWatch does (a FILL of a series that was never published reads the number at every timestamp).
type mathWorld struct {
	t      *testing.T
	series map[string]float64 // "MetricName Set AZ" ("" AZ for a series without one) to its value
	metric map[string]alarmMetric
	cache  map[string]float64
}

func seriesKey(name, set, az string) string { return name + " " + set + " " + az }

// alarmValue is what the alarm's returned series reads in the world, and whether the alarm's comparison breaches on it
// (a missing value breaches nothing, as treat-missing-data notBreaching says).
func alarmValue(t *testing.T, a putAlarm, series map[string]float64) (value float64, breaching bool) {
	t.Helper()
	w := &mathWorld{t: t, series: series, metric: map[string]alarmMetric{}, cache: map[string]float64{}}
	var returned string
	for _, m := range alarmMetrics(t, a["--alarm-name"], a) {
		w.metric[m.ID] = m
		if m.ReturnData {
			returned = m.ID
		}
	}
	if returned == "" {
		t.Fatalf("%s returns no series", a["--alarm-name"])
	}
	value = w.id(returned)
	threshold, err := strconv.ParseFloat(a["--threshold"], 64)
	if err != nil {
		t.Fatal(err)
	}
	switch a["--comparison-operator"] {
	case "GreaterThanThreshold":
		breaching = value > threshold
	case "LessThanThreshold":
		breaching = value < threshold
	default:
		t.Fatalf("unknown comparison %s", a["--comparison-operator"])
	}
	return value, breaching
}

func (w *mathWorld) id(id string) float64 {
	if v, ok := w.cache[id]; ok {
		return v
	}
	m, ok := w.metric[id]
	if !ok {
		w.t.Fatalf("expression reads %q, which the alarm does not define", id)
	}
	v := math.NaN()
	if m.Expression != "" {
		v = (&mathParser{w: w, toks: tokens(w.t, m.Expression)}).parse()
	} else {
		var set, az string
		for _, d := range m.MetricStat.Metric.Dimensions {
			switch d.Name {
			case "Set":
				set = d.Value
			case "AZ":
				az = d.Value
			}
		}
		if got, ok := w.series[seriesKey(m.MetricStat.Metric.MetricName, set, az)]; ok {
			v = got
		}
	}
	w.cache[id] = v
	return v
}

func tokens(t *testing.T, s string) []string {
	var out []string
	for i := 0; i < len(s); {
		c := rune(s[i])
		switch {
		case unicode.IsSpace(c):
			i++
		case unicode.IsLetter(c) || c == '_':
			j := i
			for j < len(s) && (unicode.IsLetter(rune(s[j])) || unicode.IsDigit(rune(s[j])) || s[j] == '_') {
				j++
			}
			out = append(out, s[i:j])
			i = j
		case unicode.IsDigit(c):
			j := i
			for j < len(s) && (unicode.IsDigit(rune(s[j])) || s[j] == '.') {
				j++
			}
			out = append(out, s[i:j])
			i = j
		case (c == '<' || c == '>') && i+1 < len(s) && s[i+1] == '=':
			out = append(out, s[i:i+2])
			i += 2
		case strings.ContainsRune("+-*/<>(),", c):
			out = append(out, string(c))
			i++
		default:
			t.Fatalf("metric math %q: cannot read %q", s, s[i:])
		}
	}
	return out
}

type mathParser struct {
	w    *mathWorld
	toks []string
	pos  int
}

func (p *mathParser) peek() string {
	if p.pos < len(p.toks) {
		return p.toks[p.pos]
	}
	return ""
}

func (p *mathParser) next() string { tok := p.peek(); p.pos++; return tok }

func (p *mathParser) expect(tok string) {
	if got := p.next(); got != tok {
		p.w.t.Fatalf("metric math %v: want %q at %d, got %q", p.toks, tok, p.pos-1, got)
	}
}

func (p *mathParser) parse() float64 {
	v := p.cmp()
	if p.pos != len(p.toks) {
		p.w.t.Fatalf("metric math %v: left over from %d", p.toks, p.pos)
	}
	return v
}

func b2n(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func (p *mathParser) cmp() float64 {
	l := p.add()
	switch op := p.peek(); op {
	case "<", ">", "<=", ">=":
		p.next()
		r := p.add()
		if math.IsNaN(l) || math.IsNaN(r) {
			return math.NaN()
		}
		return b2n(map[string]bool{"<": l < r, ">": l > r, "<=": l <= r, ">=": l >= r}[op])
	}
	return l
}

func (p *mathParser) add() float64 {
	v := p.mul()
	for p.peek() == "+" || p.peek() == "-" {
		if p.next() == "+" {
			v += p.mul()
		} else {
			v -= p.mul()
		}
	}
	return v
}

func (p *mathParser) mul() float64 {
	v := p.primary()
	for p.peek() == "*" || p.peek() == "/" {
		if p.next() == "*" {
			v *= p.primary()
		} else {
			v /= p.primary()
		}
	}
	return v
}

func (p *mathParser) primary() float64 {
	tok := p.next()
	switch {
	case tok == "(":
		v := p.cmp()
		p.expect(")")
		return v
	case tok == "":
		p.w.t.Fatalf("metric math %v ends early", p.toks)
	case unicode.IsDigit(rune(tok[0])):
		v, err := strconv.ParseFloat(tok, 64)
		if err != nil {
			p.w.t.Fatal(err)
		}
		return v
	case p.peek() == "(":
		p.next()
		var args []float64
		for {
			args = append(args, p.cmp())
			if p.peek() != "," {
				break
			}
			p.next()
		}
		p.expect(")")
		switch {
		case tok == "FILL" && len(args) == 2:
			if math.IsNaN(args[0]) {
				return args[1]
			}
			return args[0]
		case tok == "IF" && len(args) == 3:
			if math.IsNaN(args[0]) {
				return math.NaN()
			}
			if args[0] != 0 {
				return args[1]
			}
			return args[2]
		}
		p.w.t.Fatalf("metric math: %s with %d arguments is not one the alarms use", tok, len(args))
	}
	return p.w.id(tok)
}
