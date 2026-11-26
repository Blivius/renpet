package main

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"time"
)

// --demo: a made-up developer's last six weeks instead of your logs. A day job at Acme, a side
// project in the evenings with agents in parallel, open source at weekends, a nightly script.
// Every day is generated from its date, so it reads the same each time, and the last minutes
// are always live. Nothing is read from or written to your files.

var demo bool

const demoDays = 45

type demoProj struct {
	dir, agent string
	titles     []string
	prompts    []string
}

var demoProjs = map[string]demoProj{
	"api": {"work/acme-api", "claude", []string{"Rate limiter for the public API", "Fix flaky billing webhook test", "Migrate invoices to Postgres 17", "Add audit log endpoint"},
		[]string{"the billing webhook test fails one run in five, find out why", "add a token bucket per API key, 100 req/min default", "write the migration and a rollback for invoices", "review the diff and tighten the error messages", "now add tests for the edge cases you listed"}},
	"web": {"work/acme-web", "codex", []string{"Dashboard empty states", "Upgrade to React 20", "Fix date picker in Safari"},
		[]string{"empty states for the dashboard cards, match the design doc", "upgrade react and fix whatever breaks", "the date picker shows the wrong month in Safari", "run the e2e suite and fix the failures"}},
	"pocket": {"code/pocketbook", "claude", []string{"Offline sync for notes", "Import from Markdown folders", "Search ranking", "Share links with expiry", "iOS widget"},
		[]string{"design offline sync: last write wins is not enough, propose something", "build the markdown folder import, keep front matter", "search ranks old notes too high, fix the scoring", "share links that expire after 7 days", "/review the sync branch before I merge it", "split this into three subagents: schema, api, ui"}},
	"queue": {"oss/tinyqueue", "antigravity", []string{"Triage issues #212–#230", "Retry with jitter", "Docs for v0.9"},
		[]string{"go through the open issues and label them", "add exponential backoff with jitter to retries", "write the v0.9 migration guide"}},
	"nightly": {"jobs/nightly-triage", "claude", []string{"Nightly triage"}, []string{"summarise yesterday's errors"}},
}

func demoConfig() Config {
	return Config{WeekStart: "monday", Idle: "15m", Rules: []Rule{{Path: "~/work", Whom: "acme"},
		{Path: "~/code", Whom: "side-project"}, {Path: "~/oss", Whom: "open source"}, {Path: "~/jobs", Whom: "automation"}}}
}

// startDemo switches the app to the made-up developer: settings and guesses go to a temp folder.
func startDemo() (Config, string) {
	demo = true
	gitInfo = func(dir string) (string, string) { return dir, "" }
	return demoConfig(), filepath.Join(os.TempDir(), "renpet-demo", "config.json")
}

// demoSession is one session from start: prompts, each followed by an agent run with events a
// minute or two apart, then you thinking before the next.
func demoSession(r *rand.Rand, key string, n int, start time.Time, prompts int, runMax int) Session {
	p := demoProjs[key]
	home, _ := os.UserHomeDir()
	s := Session{Agent: p.agent, Cwd: filepath.Join(home, p.dir), AITitle: p.titles[r.IntN(len(p.titles))],
		File: filepath.Join(home, ".renpet-demo", key, fmt.Sprintf("%s-%d.jsonl", start.Format("20060102-1504"), n))}
	t := start
	for i := range prompts {
		s.add(t, true, p.prompts[(n+i)%len(p.prompts)])
		run := time.Duration(2+r.IntN(runMax)) * time.Minute
		for x := 20 * time.Second; x < run; x += time.Duration(30+r.IntN(110)) * time.Second {
			s.add(t.Add(x), false, "")
		}
		t = t.Add(run)
		s.add(t, false, "")
		t = t.Add(time.Duration(40+r.IntN(400)) * time.Second)
	}
	s.Waiting = true
	return s
}

// demoSub is a subagent working inside its parent's first run.
func demoSub(parent Session, k int) Session {
	s := Session{Agent: "claude-sub", Cwd: parent.Cwd, File: parent.File[:len(parent.File)-len(".jsonl")] + fmt.Sprintf("/subagents/agent-%d.jsonl", k)}
	for t := parent.Events[0].T.Add(time.Minute); t.Before(parent.Events[0].T.Add(9 * time.Minute)); t = t.Add(70 * time.Second) {
		s.add(t, false, "")
	}
	s.Waiting = true
	return s
}

// demoDay is everything the made-up developer did on day.
func demoDay(day, now time.Time) (out []Session) {
	r := rand.New(rand.NewPCG(uint64(day.Unix()), 7))
	ago := nDays(day, startOfDay(now))
	at := func(h, m int) time.Time { return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute) }
	if ago == 17 { // one day off: the streak starts after it
		return nil
	}
	out = append(out, demoSession(r, "nightly", 0, at(3, r.IntN(20)), 1, 3))
	wd := day.Weekday()
	if wd != time.Saturday && wd != time.Sunday {
		for k := range 2 + r.IntN(2) { // the day job: two or three sessions, sometimes side by side
			key := []string{"api", "web"}[r.IntN(2)]
			out = append(out, demoSession(r, key, k, at(10+k*2+r.IntN(2), r.IntN(50)), 4+r.IntN(5), 12))
		}
	} else {
		out = append(out, demoSession(r, "queue", 0, at(14, r.IntN(40)), 5+r.IntN(4), 10))
	}
	if r.IntN(5) > 0 { // most evenings: the side project, agents in parallel
		par := 2 + r.IntN(3)
		if ago == 2 { // the burst: seven at once, and one long run nobody watched
			par = 7
		}
		for k := range par {
			s := demoSession(r, "pocket", k, at(20, 30+k*4+r.IntN(10)), 3+r.IntN(4), 14)
			if k == 0 && ago == 2 {
				long := Session{Agent: s.Agent, Cwd: s.Cwd, File: s.File + ".long", AITitle: "Rewrite the sync engine"}
				long.add(at(21, 5), true, "rewrite the sync engine with CRDTs, run the whole suite, don't stop until it's green")
				for t := at(21, 6); t.Before(at(22, 23)); t = t.Add(100 * time.Second) {
					long.add(t, false, "")
				}
				out = append(out, long)
			}
			out = append(out, s)
			if r.IntN(3) == 0 {
				out = append(out, demoSub(s, 1), demoSub(s, 2))
			}
		}
	}
	if ago == 4 { // the late night
		out = append(out, demoSession(r, "pocket", 9, at(23, 50), 14, 12))
	}
	return out
}

// demoSessions stands in for load: the days that touch [from, to), and right now, two agents
// working and one waiting on you.
func demoSessions(from, to time.Time) (out []Session) {
	now := time.Now()
	first := maxT(startOfDay(from).AddDate(0, 0, -1), startOfDay(now).AddDate(0, 0, -demoDays))
	for day := first; day.Before(to) && !day.After(now); day = day.AddDate(0, 0, 1) {
		for _, s := range demoDay(day, now) {
			var ev []Event
			for _, e := range s.Events {
				if e.T.Before(now.Add(-20 * time.Minute)) { // only the live sessions below reach now
					ev = append(ev, e)
				}
			}
			if s.Events = ev; len(ev) > 0 {
				out = append(out, s)
			}
		}
	}
	r := rand.New(rand.NewPCG(uint64(startOfDay(now).Unix()), 9))
	live := func(key string, n int, ago time.Duration, waiting bool) Session {
		s := demoSession(r, key, 100+n, now.Add(-ago), 4, 6)
		shift := now.Add(-15 * time.Second).Sub(s.Events[len(s.Events)-1].T)
		if waiting {
			shift -= 3 * time.Minute
		}
		for i := range s.Events {
			s.Events[i].T = s.Events[i].T.Add(shift)
		}
		s.Waiting = waiting
		s.File = filepath.Join(filepath.Dir(s.File), fmt.Sprintf("live-%d.jsonl", n)) // stable while the clock moves
		return s
	}
	work := live("pocket", 0, 40*time.Minute, false)
	sub := demoSub(work, 1)
	for i := range sub.Events { // a subagent at work right now
		sub.Events[i].T = sub.Events[i].T.Add(now.Add(-30 * time.Second).Sub(sub.Events[len(sub.Events)-1].T))
	}
	sub.Waiting = false
	out = append(out, work, sub, live("api", 1, 35*time.Minute, false), live("web", 2, 30*time.Minute, true))
	return out
}

// demoRead is readSession for the demo: the session with its prompts, regenerated.
func demoRead(file string) (Session, error) {
	now := time.Now()
	for _, s := range demoSessions(now.AddDate(0, 0, -demoDays), now) {
		if s.File == file {
			return s, nil
		}
	}
	return Session{}, fmt.Errorf("not in the demo")
}
