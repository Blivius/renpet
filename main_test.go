package main

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, dir, name string, lines ...string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const day = "2026-11-20T"

func cl(typ, clock, content string) string {
	return `{"type":"` + typ + `","timestamp":"` + day + clock + `Z","cwd":"CWD","message":{"content":` + content + `}}`
}

// Golden fixture: two parallel Claude sessions, an idle split, a tool_result and an injected
// notification that must not count as prompts, a Codex rollout, and rule order.
func TestBuild(t *testing.T) {
	gitInfo = func(dir string) (string, string) { return dir, "" }
	dir := t.TempDir()
	office := func(s string) string { return strings.ReplaceAll(s, "CWD", "/w/office") }
	self := func(s string) string { return strings.ReplaceAll(s, "CWD", "/w/self") }

	a, _ := loadClaude(write(t, dir, "a.jsonl",
		`{"type":"mode"}`, // no timestamp: ignored
		office(cl("user", "10:00:00", `"start the refactor"`)),
		office(cl("assistant", "10:01:00", `[{"type":"text","text":"ok"}]`)),
		office(cl("user", "10:05:00", `[{"type":"tool_result","content":"done"}]`)), // agent, not you
		office(cl("assistant", "10:06:00", `[{"type":"text","text":"next?"}]`)),
		office(cl("user", "10:10:00", `[{"type":"text","text":"yes go"}]`)), // you: 4m think
		office(cl("assistant", "10:12:00", `[{"type":"text","text":"working"}]`)),
		office(cl("user", "10:13:00", `"<task-notification>bg done</task-notification>"`)), // injected
		office(cl("user", "11:00:00", `"back after lunch"`)),                               // 47m gap: idle
		office(cl("assistant", "11:02:00", `[{"type":"text","text":"hi"}]`)),
	), false)
	b, _ := loadClaude(write(t, dir, "b.jsonl",
		self(cl("user", "10:02:00", `"parallel task"`)),
		self(cl("assistant", "10:03:00", `[{"type":"text","text":"a"}]`)),
		self(cl("assistant", "10:08:00", `[{"type":"text","text":"b"}]`)),
	), false)
	c, _ := loadCodex(write(t, dir, "rollout-c.jsonl",
		`{"timestamp":"`+day+`12:00:00Z","type":"session_meta","payload":{"cwd":"/w/self"}}`,
		`{"timestamp":"`+day+`12:00:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>x</environment_context>"}]}}`,
		`{"timestamp":"`+day+`12:01:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"fix the bug"}]}}`,
		`{"timestamp":"`+day+`12:01:00Z","type":"event_msg","payload":{"type":"task_started"}}`,
		`{"timestamp":"`+day+`12:04:00Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"fixed"}]}}`,
		`{"timestamp":"`+day+`12:04:30Z","type":"event_msg","payload":{"type":"task_complete"}}`,
	))
	if c.Cwd != "/w/self" {
		t.Fatalf("codex cwd = %q", c.Cwd)
	}

	rules := []Rule{{Path: "/w/office", Whom: "office"}, {Path: "/w", Whom: "self"}}
	from, _ := time.Parse(time.RFC3339, day+"00:00:00Z")
	r := build([]Session{a, b, c}, rules, 15*time.Minute, from, from.Add(24*time.Hour))

	got := map[string]Row{}
	for _, row := range r.Rows {
		got[row.Project+"|"+row.Whom] = row
	}
	want := map[string][4]float64{ // with-agents, agent-run, think (minutes), peak
		"/w/office|office": {15, 11, 4, 1},
		"/w/self|self":     {10.5, 9.5, 1, 1},
		"*|office":         {15, 11, 4, 1},
		"*|self":           {10.5, 9.5, 1, 1},
		"*|TOTAL":          {19.5, 20.5, 5, 2}, // A and B overlap 10:02–10:08
	}
	for k, w := range want {
		row, ok := got[k]
		if !ok {
			t.Fatalf("missing row %s; have %v", k, got)
		}
		m := func(h float64) float64 { return math.Round(h*60*10) / 10 }
		if m(row.ActiveHours) != w[0] || m(row.AgentHours) != w[1] || m(row.ThinkHours) != w[2] || float64(row.Peak) != w[3] {
			t.Errorf("%s: got active=%v agent=%v think=%v peak=%d, want %v", k, m(row.ActiveHours), m(row.AgentHours), m(row.ThinkHours), row.Peak, w)
		}
	}
	if n := len(got["/w/self|self"].Sessions); n != 2 {
		t.Errorf("self sessions = %d, want 2", n)
	}
}

func TestWhomOrder(t *testing.T) {
	rules := []Rule{{Remote: "tulip-tech/", Whom: "office"}, {Path: "/w/office", Whom: "office"}, {Path: "/w", Whom: "self"}}
	cases := []struct{ cwd, remote, want string }{
		{"/w/x", "git@neeomdumham:Tulip-Tech/velaops.git", "office"}, // remote beats path
		{"/w/office/sub", "", "office"},
		{"/w/officer", "", "self"}, // prefix must end at a path boundary
		{"/elsewhere", "", "unknown"},
	}
	for _, c := range cases {
		if got := whom(rules, c.cwd, c.remote); got != c.want {
			t.Errorf("whom(%q,%q) = %q, want %q", c.cwd, c.remote, got, c.want)
		}
	}
}

// A subagent's "user" lines come from the parent agent; Antigravity marks you as USER_EXPLICIT.
func TestSubagentAndAntigravity(t *testing.T) {
	dir := t.TempDir()
	sub, _ := loadClaude(write(t, dir, "agent-x.jsonl",
		cl("user", "10:00:00", `"research the API"`), // written by the parent agent
		cl("assistant", "10:03:00", `[{"type":"text","text":"found it"}]`),
	), true)
	for _, e := range sub.Events {
		if e.Human {
			t.Fatalf("subagent line counted as human: %+v", e)
		}
	}
	ag, _ := loadAntigravity(write(t, dir, "transcript.jsonl",
		`{"source":"SYSTEM","type":"SYSTEM_MESSAGE","created_at":"`+day+`09:00:00Z"}`,
		`{"source":"USER_EXPLICIT","type":"USER_INPUT","created_at":"`+day+`09:01:00Z"}`,
		`{"source":"MODEL","type":"PLANNER_RESPONSE","created_at":"`+day+`09:04:00Z"}`,
	), "/w/self")
	if len(ag.Events) != 3 || ag.Events[0].Human || !ag.Events[1].Human || ag.Events[2].Human || ag.Cwd != "/w/self" {
		t.Fatalf("antigravity parse: %+v", ag)
	}
}

// Two agents overlapping in the 10:00 cell show as "2"; think time ≥5 min shows as █.
func TestLaneRows(t *testing.T) {
	d0, _ := time.Parse(time.RFC3339, day+"00:00:00Z")
	at := func(h, m int) time.Time { return d0.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute) }
	ats := []Attributed{
		{Running: []Span{{at(10, 0), at(10, 10)}}, Whom: "office"},
		{Running: []Span{{at(10, 5), at(10, 12)}}, Thinking: []Span{{at(10, 30), at(10, 40)}}, Whom: "blivius"},
	}
	agents, you, whom := laneRows(ats, d0)
	cell := 40 // 10:00 is cell 10*4
	if agents[cell] != '2' || agents[cell+1] != '.' || agents[cell+2] != '.' {
		t.Errorf("agents lane around 10:00 = %q", agents[cell:cell+3])
	}
	if r := []rune(you); string(r[cell+2]) != "█" || string(r[cell]) != "." {
		t.Errorf("you lane around 10:00 = %q", string(r[cell:cell+3]))
	}
	if whom[cell] != 'o' || whom[cell+2] != 'b' {
		t.Errorf("whom lane around 10:00 = %q", whom[cell:cell+3])
	}
}
