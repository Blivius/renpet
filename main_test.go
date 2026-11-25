package main

import (
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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
	rules := []Rule{{Remote: "acme/", Whom: "office"}, {Path: "/w/office", Whom: "office"}, {Path: "/w", Whom: "self"}}
	cases := []struct{ cwd, remote, want string }{
		{"/w/x", "git@github.com:Acme/app.git", "office"}, // remote beats path
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
		{Running: []Span{{at(10, 5), at(10, 12)}}, Thinking: []Span{{at(10, 30), at(10, 40)}}, Whom: "side"},
	}
	agents, you, whom := laneRows(ats, d0)
	cell := 40 // 10:00 is cell 10*4
	if agents[cell] != '2' || agents[cell+1] != '.' || agents[cell+2] != '.' {
		t.Errorf("agents lane around 10:00 = %q", agents[cell:cell+3])
	}
	if r := []rune(you); string(r[cell+2]) != "█" || string(r[cell]) != "." {
		t.Errorf("you lane around 10:00 = %q", string(r[cell:cell+3]))
	}
	if whom[cell] != 'o' || whom[cell+2] != 's' {
		t.Errorf("whom lane around 10:00 = %q", whom[cell:cell+3])
	}
}

func TestScaleHex(t *testing.T) {
	cases := map[float64]string{1: "#ff8040", 0.5: "#804020", 0: "#000000", 2: "#ff8040"}
	for f, want := range cases {
		if got := scaleHex("#ff8040", f); got != want {
			t.Errorf("scaleHex(%v) = %s, want %s", f, got, want)
		}
	}
	if got := scaleHex("#zz0000", 0.5); got != "#zz0000" {
		t.Errorf("bad hex should pass through, got %s", got)
	}
}

// The week view must fit an 80-column terminal, draw 24 three-column hours per weekday once the
// reveal has passed, and say something true.
func TestWeekView(t *testing.T) {
	gitInfo = func(dir string) (string, string) { return dir, "" }
	ansi := regexp.MustCompile(`\x1b\[[0-9;:]*m`)
	start := weekOf(time.Date(2026, 11, 18, 12, 0, 0, 0, time.Local)) // Mon 16 Nov
	if start.Weekday() != time.Monday {
		t.Fatalf("weekOf gave %s", start.Weekday())
	}
	ev := func(h, m int, human bool) Event {
		return Event{T: start.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute), Human: human}
	}
	var sessions []Session
	for k := range 3 { // three parallel office agents on Monday 10:00–10:40: a burn
		sessions = append(sessions, Session{Agent: "claude", Cwd: "/w/office", Events: []Event{ev(10, 0, true), ev(10, 10+k, false), ev(10, 20, false), ev(10, 30, false), ev(10, 40, false)}})
	}
	rules := []Rule{{Path: "/w/office", Whom: "office"}}
	end := start.AddDate(0, 0, 7)
	ats := attribute(sessions, rules, 15*time.Minute, start, end)
	w := weekData{start: start, week: build(sessions, rules, 15*time.Minute, start, end)}
	for i := range 7 {
		d := start.AddDate(0, 0, i)
		w.cells[i] = laneCells(ats, d, time.Hour)
		w.days[i] = build(sessions, rules, 15*time.Minute, d, d.AddDate(0, 0, 1))
	}
	if c := w.cells[0][10]; c.Peak != 3 || c.Active != 40*time.Minute || level(c.Active, time.Hour) != 3 {
		t.Fatalf("Mon 10:00 cell = %+v", c)
	}
	if got := voice(w); !strings.Contains(got, "Office took 100%") {
		t.Errorf("voice = %q", got)
	}
	view := ansi.ReplaceAllString(model{start: start, w: &w, shown: start, now: start.Add(time.Hour), user: "u"}.View().Content, "")
	rows := 0
	row := regexp.MustCompile(`^(Mon|Tue|Wed|Thu|Fri|Sat|Sun) [› ]  [·█ ]`)
	for _, line := range strings.Split(view, "\n") {
		if n := utf8.RuneCountInString(line); n > 80 {
			t.Errorf("line is %d wide: %q", n, line)
		}
		if row.MatchString(line) {
			rows++
			if n := utf8.RuneCountInString(line) - gutter; n != 72 { // 24 hours × "██ "
				t.Errorf("row has %d cell chars, want 72: %q", n, line)
			}
		}
	}
	if rows != 7 {
		t.Errorf("found %d weekday rows, want 7", rows)
	}
	if testing.Verbose() {
		t.Log("\n" + view)
	}
}

// Settings come from the config file, fall back to defaults, and reject nonsense loudly.
func TestConfig(t *testing.T) {
	dir := t.TempDir()
	c, err := loadConfig(filepath.Join(dir, "missing.json"))
	if err != nil || c.WeekStart != "monday" || c.Idle != "15m" || len(c.Rules) == 0 {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	p := write(t, dir, "c.json", `{"week_start":"sunday","idle":"20m","rules":[{"path":"/w","whom":"self"}]}`)
	if c, err = loadConfig(p); err != nil || c.WeekStart != "sunday" || c.Idle != "20m" || len(c.Rules) != 1 {
		t.Fatalf("file: %+v %v", c, err)
	}
	if _, err := loadConfig(write(t, dir, "bad.json", `{"week_start":"someday"}`)); err == nil {
		t.Fatal("bad week_start accepted")
	}

	defer func(ws time.Weekday) { weekStart = ws }(weekStart)
	wed := time.Date(2026, 11, 25, 15, 0, 0, 0, time.Local)
	for ws, want := range map[time.Weekday]string{time.Monday: "Mon 23", time.Sunday: "Sun 22", time.Saturday: "Sat 21"} {
		weekStart = ws
		if got := weekOf(wed).Format("Mon 2"); got != want {
			t.Errorf("week starting %s: weekOf(Wed 25) = %s, want %s", ws, got, want)
		}
	}
}

// Reading a growing transcript in two steps gives the same events as reading it once, and a
// line still being written is left for the next read.
func TestIncrementalParse(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	l1 := cl("user", "10:00:00", `"go"`) + "\n"
	l2 := cl("assistant", "10:01:00", `[{"type":"text","text":"ok"}]`) + "\n"
	l3 := cl("user", "10:05:00", `"next"`) + "\n"
	half := l3[:20]
	if err := os.WriteFile(p, []byte(l1+l2+half), 0o644); err != nil {
		t.Fatal(err)
	}
	s := Session{Agent: "claude", File: p}
	off, err := parseClaude(p, 0, &s, false)
	if err != nil || off != int64(len(l1+l2)) || len(s.Events) != 2 {
		t.Fatalf("first read: off=%d events=%d err=%v", off, len(s.Events), err)
	}
	if err := os.WriteFile(p, []byte(l1+l2+l3), 0o644); err != nil { // the half line got finished
		t.Fatal(err)
	}
	if off, err = parseClaude(p, off, &s, false); err != nil || off != int64(len(l1+l2+l3)) {
		t.Fatalf("second read: off=%d err=%v", off, err)
	}
	full, _ := loadClaude(p, false)
	if len(s.Events) != 3 || len(full.Events) != 3 || !s.Events[2].Human || !s.Events[2].T.Equal(full.Events[2].T) {
		t.Fatalf("incremental %+v vs full %+v", s.Events, full.Events)
	}
}

func TestReadGit(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "repo", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "repo", ".git"), "config",
		`[core]`, `	bare = false`, `[remote "upstream"]`, `	url = git@x:other/repo.git`, `[remote "origin"]`, `	url = git@github.com:Acme/app.git`)
	sub := filepath.Join(dir, "repo", "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if top, remote := readGit(sub); top != filepath.Join(dir, "repo") || remote != "git@github.com:Acme/app.git" {
		t.Errorf("readGit = %q, %q", top, remote)
	}
	if top, remote := readGit(filepath.Join(dir, "gone")); top != filepath.Join(dir, "gone") || remote != "" {
		t.Errorf("no repo: %q, %q", top, remote)
	}
}

// A session is named by your title, else Claude's, else your first prompt; slash commands read
// as typed. Each prompt row knows the think gap before it and the agent time after it.
func TestTitlesAndPrompts(t *testing.T) {
	dir := t.TempDir()
	s, _ := loadClaude(write(t, dir, "t.jsonl",
		cl("user", "10:00:00", `"<command-message>moeen</command-message>\n<command-name>/moeen</command-name>\n<command-args>look   around</command-args>"`),
		cl("assistant", "10:05:00", `[{"type":"text","text":"done"}]`),
		cl("user", "10:07:00", `[{"type":"text","text":"ship it"}]`),
		cl("assistant", "10:09:00", `[{"type":"text","text":"shipped"}]`),
		cl("assistant", "11:30:00", `[{"type":"text","text":"late"}]`), // past the idle gap: nobody's time
		`{"type":"ai-title","aiTitle":"Looking around"}`,
	), false)
	if s.Prompt != "/moeen look around" || s.Name() != "Looking around" {
		t.Fatalf("prompt %q name %q", s.Prompt, s.Name())
	}
	if s.Title = "Mine"; s.Name() != "Mine" {
		t.Fatalf("your title should win, got %q", s.Name())
	}
	rows := promptRows(s, 15*time.Minute)
	if len(rows) != 2 || rows[0].Run != 5*time.Minute || rows[1].Think != 2*time.Minute || rows[1].Run != 2*time.Minute || rows[1].Text != "ship it" {
		t.Fatalf("prompt rows: %+v", rows)
	}
}

func TestSaveConfig(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.json")
	link := filepath.Join(dir, "config.json")
	if err := os.WriteFile(real, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	want := Config{WeekStart: "sunday", Idle: "20m", Rules: []Rule{{Path: "~/x", Whom: "side"}}}
	if err := saveConfig(link, want); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Lstat(link); st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symlink was replaced instead of written through")
	}
	got, err := loadConfig(link)
	if err != nil || got.WeekStart != "sunday" || got.Idle != "20m" || len(got.Rules) != 1 || got.Rules[0].Whom != "side" {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	if fmtDur(15*time.Minute) != "15m" || fmtDur(2*time.Hour) != "2h" || snapIdle(12*time.Minute) != 15*time.Minute {
		t.Fatal("fmtDur/snapIdle")
	}
}

// Every screen renders at 80×24 without spilling past 80 columns or 24 lines.
func TestScreensFit(t *testing.T) {
	gitInfo = func(dir string) (string, string) { return dir, "" }
	start := weekOf(time.Date(2026, 11, 18, 12, 0, 0, 0, time.Local))
	ev := func(d, h, m int, human bool, text string) Event {
		return Event{T: start.AddDate(0, 0, d).Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute), Human: human, text: text}
	}
	var sessions []Session
	for k := range 30 { // enough rows to need scrolling
		sessions = append(sessions, Session{Agent: "claude", File: "/f", Cwd: "/w/a-very-long-project-path/that/keeps/going/on-and-on" + strings.Repeat("x", k%3),
			Prompt: "a prompt long enough to be cut " + strings.Repeat("words ", 20),
			Events: []Event{ev(0, 10, k, true, "hi"), ev(0, 10, k+3, false, ""), ev(0, 10, k+5, true, strings.Repeat("long text ", 50)), ev(0, 10, k+9, false, "")}})
	}
	sessions = append(sessions, Session{Agent: "codex", Cwd: "/w/side", Events: []Event{ev(0, 9, 0, true, "x"), ev(0, 9, 4, false, "")}})
	rules := []Rule{{Path: "/w/side", Whom: "side"}}
	end := start.AddDate(0, 0, 7)
	ats := attribute(sessions, rules, 15*time.Minute, start, end)
	w := weekData{start: start, week: build(sessions, rules, 15*time.Minute, start, end), ats: ats, gaps: promptGaps(sessions, start, end)}
	for i := range 7 {
		d := start.AddDate(0, 0, i)
		w.cells[i] = laneCells(ats, d, time.Hour)
		w.days[i] = build(sessions, rules, 15*time.Minute, d, d.AddDate(0, 0, 1))
	}
	m := model{start: start, w: &w, rules: rules, idle: 15 * time.Minute, width: 80, height: 24, cfg: Config{WeekStart: "monday", Idle: "15m", Rules: rules},
		cfgPath: "/c.json", gapIdle: 15 * time.Minute}
	rows, _ := m.projects()
	m.proj = rows[0]
	ss, _ := m.sessions()
	m.at = ss[0]
	m.detail = &m.at.Session
	ansiRe := regexp.MustCompile(`\x1b\[[0-9;:]*m`)
	for _, scr := range []screen{projectsScr, sessionsScr, detailScr, gapsScr, settingsScr} {
		m.scr = scr
		m.cur[scr] = 20
		view := ansiRe.ReplaceAllString(m.View().Content, "")
		lines := strings.Split(strings.TrimSuffix(view, "\n"), "\n")
		if len(lines) > 24 {
			t.Errorf("screen %d: %d lines", scr, len(lines))
		}
		for _, l := range lines {
			if n := utf8.RuneCountInString(l); n > 80 {
				t.Errorf("screen %d: %d wide: %q", scr, n, l)
			}
		}
		if scr == settingsScr && !strings.Contains(view, "Unassigned") && !strings.Contains(view, "a-very-long") {
			t.Errorf("settings should list the unassigned folder:\n%s", view)
		}
	}
}
