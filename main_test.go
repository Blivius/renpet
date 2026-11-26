package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
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

// testModel is the app on given sessions, fully revealed, without reading anything from disk.
func testModel(sessions []Session, rules []Rule, p period, from, to time.Time, w, h int) model {
	d := rangeFrom(sessions, p, from, to, rules, 15*time.Minute, to)
	return model{rules: rules, idle: 15 * time.Minute, per: p, from: from, to: to, d: &d, loaded: true, past: map[[2]time.Time]*rangeData{},
		width: w, height: h, cfg: Config{WeekStart: "monday", Idle: "15m", Rules: rules}, cfgPath: "/c.json", gapIdle: 15 * time.Minute,
		now: to.Add(time.Hour), shown: from}
}

// The week view must fit an 80-column terminal, draw 24 three-column hours per weekday once the
// reveal has passed, and say something true.
func TestWeekView(t *testing.T) {
	gitInfo = func(dir string) (string, string) { return dir, "" }
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
	m := testModel(sessions, rules, weekP, start, start.AddDate(0, 0, 7), 80, 24)
	if c := m.d.cells[0][10]; c.Peak != 3 || c.Active != 40*time.Minute || level(c.Active, time.Hour) != 3 {
		t.Fatalf("Mon 10:00 cell = %+v", c)
	}
	if got := voice(*m.d); !strings.Contains(got, "Office took 100%") {
		t.Errorf("voice = %q", got)
	}
	if tot := total(m.d.total); tot.Peak != 3 || !tot.PeakAt.Equal(start.Add(10*time.Hour)) {
		t.Errorf("peak %d at %s", tot.Peak, tot.PeakAt)
	}
	view := ansi.Strip(m.View().Content)
	rows := 0
	row := regexp.MustCompile(`^ (Mon|Tue|Wed|Thu|Fri|Sat|Sun) [› ]  [·█ ]`)
	for _, line := range strings.Split(view, "\n") {
		if n := utf8.RuneCountInString(line); n > 80 {
			t.Errorf("line is %d wide: %q", n, line)
		}
		if row.MatchString(line) {
			rows++
			if n := utf8.RuneCountInString(line) - 1 - gutter; n != 72 { // 24 hours × "██ "
				t.Errorf("row has %d cell chars, want 72: %q", n, line)
			}
		}
	}
	if rows != 7 {
		t.Errorf("found %d weekday rows, want 7:\n%s", rows, view)
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
	if err := writeJSON(link, want); err != nil {
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

// Every screen, the key sheet and the palette render inside the terminal: 80×24 with the tab
// bar, 120×30 with the sidebar, and a month as a calendar.
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
	month := time.Date(2026, 11, 1, 0, 0, 0, 0, time.Local)
	for _, c := range []struct {
		p        period
		from, to time.Time
		w, h     int
	}{{weekP, start, start.AddDate(0, 0, 7), 80, 24}, {weekP, start, start.AddDate(0, 0, 7), 120, 30}, {monthP, month, month.AddDate(0, 1, 0), 80, 24}, {dayP, start, start.AddDate(0, 0, 1), 100, 24}} {
		m := testModel(sessions, rules, c.p, c.from, c.to, c.w, c.h)
		m.whole = true // a month's first day is empty; the whole range has the rows
		rows, _ := m.projects()
		m.proj = rows[0]
		ss, _ := m.sessions()
		m.at = ss[0].Attributed
		m.detail = &m.at.Session
		recs := records(m.d.ats, c.to)
		m.rec = &recs
		lv := liveFrom(sessions, rules, 15*time.Minute, start.Add(10*time.Hour+40*time.Minute))
		m.live = &lv
		for i, scr := range []screen{overviewScr, projectsScr, sessionsScr, detailScr, gapsScr, settingsScr, settingsScr, projectsScr, timelineScr, recordsScr, overviewScr, liveScr, overviewScr} {
			m.scr, m.cur[scr] = scr, 20
			m.help = i == 6                                 // the key sheet over settings
			m.pal = paletteState{open: i == 7, query: "ra"} // the palette over projects
			m.gs = guessState{open: i == 10, input: "12"}   // the perception check over the overview
			m.card, m.cardHide = i == 12, c.w == 80         // the card over the overview
			if scr == liveScr && !strings.Contains(m.View().Content, "working") {
				t.Errorf("live should list the working sessions:\n%s", ansi.Strip(m.View().Content))
			}
			if box := ansi.Strip(m.cardBox()); i == 12 && c.w == 80 && (!strings.Contains(box, "▒▒▒▒▒") || strings.Contains(box, "Side")) {
				t.Errorf("the card should hide names:\n%s", box)
			}
			view := ansi.Strip(m.View().Content)
			if scr == timelineScr && c.p != monthP && (!strings.Contains(view, "┿") || !strings.Contains(view, "━")) {
				t.Errorf("timeline should draw prompts and agent runs:\n%s", view)
			}
			if i == 10 && (!strings.Contains(view, "Perception check") || strings.Contains(view, "agents waited on you")) {
				t.Errorf("the guess box should hide the numbers:\n%s", view)
			}
			lines := strings.Split(strings.TrimSuffix(view, "\n"), "\n")
			if len(lines) > c.h {
				t.Errorf("%dx%d screen %d: %d lines:\n%s", c.w, c.h, scr, len(lines), view)
			}
			for _, l := range lines {
				if n := utf8.RuneCountInString(l); n > c.w {
					t.Errorf("%dx%d screen %d: %d wide: %q", c.w, c.h, scr, n, l)
				}
			}
			if scr == settingsScr && !m.help && !strings.Contains(view, "Unassigned") && !strings.Contains(view, "on-and-on") {
				t.Errorf("settings should list the unassigned folder:\n%s", view)
			}
			if testing.Verbose() && c.w == 120 {
				t.Logf("%d:\n%s", scr, view)
			}
		}
	}
}

// Subagents fold into the session that started them, automation folders into one row, and the
// palette finds commands by their letters in order.
func TestFolding(t *testing.T) {
	gitInfo = func(dir string) (string, string) { return dir, "" }
	start := weekOf(time.Date(2026, 11, 18, 12, 0, 0, 0, time.Local))
	ev := func(m int, human bool) Event {
		return Event{T: start.Add(10*time.Hour + time.Duration(m)*time.Minute), Human: human}
	}
	parent := Session{Agent: "claude", File: "/p/s.jsonl", Cwd: "/w/app", Prompt: "build it", Events: []Event{ev(0, true), ev(10, false)}}
	sub := Session{Agent: "claude-sub", File: "/p/s/subagents/agent-1.jsonl", Cwd: "/w/app", Events: []Event{ev(2, false), ev(8, false)}}
	var jobs []Session
	for k := range 3 {
		jobs = append(jobs, Session{Agent: "claude", File: fmt.Sprintf("/j%d", k), Cwd: fmt.Sprintf("/jobs/run-%d", k), Events: []Event{ev(20, false), ev(30+k, false)}})
	}
	rules := []Rule{{Path: "/jobs", Whom: "automation"}, {Path: "/w", Whom: "side"}}
	m := testModel(append([]Session{parent, sub}, jobs...), rules, weekP, start, start.AddDate(0, 0, 7), 100, 30)
	m.whole = true
	rows, _ := m.projects() // the automation row has the most hours, so it comes first
	if len(rows) != 2 || rows[0].Project != "*" || rows[0].Whom != "automation" || len(rows[0].Sessions) != 3 {
		t.Fatalf("rows %+v", rows)
	}
	m.proj = rows[0]
	if ss, _ := m.sessions(); len(ss) != 3 {
		t.Errorf("automation folder row should list its 3 sessions, got %d", len(ss))
	}
	m.proj = rows[1]
	ss, folded := m.sessions()
	if len(ss) != 1 || folded != 1 || ss[0].subs != 1 || ss[0].subRun != 6*time.Minute {
		t.Fatalf("sessions %+v folded %d", ss, folded)
	}
	m.all = true
	if ss, folded := m.sessions(); len(ss) != 2 || folded != 0 {
		t.Errorf("[a] should list the subagent on its own: %d %d", len(ss), folded)
	}
	if !fuzzy("rw", "Range: week") || fuzzy("wr", "Range: week") {
		t.Error("fuzzy")
	}
	if got := dur(2*time.Hour + 5*time.Minute); got != "2h 05m" || dur(0) != "–" || dur(30*time.Second) != "<1m" || dur(45*time.Minute) != "45m" {
		t.Errorf("dur = %q", got)
	}
}

// The mark is the logo: the 16×5 sidebar size keeps the tags' slanted cuts (quadrant blocks).
func TestMark(t *testing.T) {
	lines := markLines(16, 5, nil)
	if len(lines) != 5 || !strings.ContainsAny(lines[0], "▗▖▟▙") || !strings.Contains(lines[1], "██") || !strings.Contains(lines[4], "██") {
		t.Errorf("mark:\n%s", strings.Join(lines, "\n"))
	}
}

// A session that moves to another repo is cut where it moved; a cd inside the same repo is not.
func TestSessionMoves(t *testing.T) {
	gitInfo = func(dir string) (string, string) { return strings.TrimSuffix(dir, "/sub"), "" } // /w/a/sub is inside repo /w/a
	dir := t.TempDir()
	at := func(cwd, typ, clock, content string) string {
		return strings.ReplaceAll(cl(typ, clock, content), "CWD", cwd)
	}
	s, _ := loadClaude(write(t, dir, "m.jsonl",
		at("/w/a", "user", "10:00:00", `"start in a"`),
		at("/w/a/sub", "assistant", "10:10:00", `[{"type":"text","text":"cd sub"}]`), // same repo
		at("/w/a", "assistant", "10:20:00", `[{"type":"text","text":"back"}]`),
		at("/w/b", "user", "10:25:00", `"resumed in b"`), // moved: 10:20–10:25 was you, in a's last folder until now
		at("/w/b", "assistant", "10:40:00", `[{"type":"text","text":"done in b"}]`),
	), false)
	if s.Cwd != "/w/a" || len(s.Moves) != 3 {
		t.Fatalf("cwd %q moves %+v", s.Cwd, s.Moves)
	}
	rules := []Rule{{Path: "/w/a", Whom: "work"}, {Path: "/w/b", Whom: "side"}}
	from, _ := time.Parse(time.RFC3339, day+"00:00:00Z")
	ats := attribute([]Session{s}, rules, 15*time.Minute, from, from.AddDate(0, 0, 1))
	if len(ats) != 2 || ats[0].Project != "/w/a" || ats[1].Project != "/w/b" || ats[1].Whom != "side" {
		t.Fatalf("parts: %+v", ats)
	}
	if a, b := sum(ats[0].Running)+sum(ats[0].Thinking), sum(ats[1].Running)+sum(ats[1].Thinking); a != 20*time.Minute || b != 20*time.Minute {
		t.Fatalf("a %v b %v, want 20m each", a, b)
	}
}

// Records: two agents at once, a 30-minute run without you, a night that ended at 01:20, and two
// days in a row.
func TestRecords(t *testing.T) {
	gitInfo = func(dir string) (string, string) { return dir, "" }
	start := weekOf(time.Date(2026, 11, 18, 12, 0, 0, 0, time.Local))
	ev := func(d, h, m int, human bool) Event {
		return Event{T: start.AddDate(0, 0, d).Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute), Human: human}
	}
	sessions := []Session{
		{Agent: "claude", File: "/a", Cwd: "/w/a", Prompt: "long one", Events: []Event{ev(0, 10, 0, true), ev(0, 10, 10, false), ev(0, 10, 20, false),
			ev(0, 10, 30, false), ev(0, 10, 35, true), ev(0, 10, 40, false)}},
		{Agent: "claude", File: "/b", Cwd: "/w/b", Events: []Event{ev(0, 10, 5, true), ev(0, 10, 15, false), ev(0, 10, 25, false)}},
		{Agent: "codex", File: "/c", Cwd: "/w/c", Events: []Event{ev(1, 1, 0, true), ev(1, 1, 10, false), ev(1, 1, 20, false)}},
	}
	now := start.AddDate(0, 0, 1).Add(12 * time.Hour)
	r := records(attribute(sessions, nil, 15*time.Minute, start, now), now)
	got := map[string]record{}
	for _, rec := range r.list {
		got[rec.name] = rec
	}
	for name, want := range map[string]string{"Most agents at once": "2", "Longest run without you": "30m", "Latest night": "01:20",
		"Busiest day": "40m", "Current streak": "2 days"} {
		if got[name].value != want {
			t.Errorf("%s = %q, want %q (all: %+v)", name, got[name].value, want, r.list)
		}
	}
	if !got["Longest run without you"].day.Equal(start.Add(10*time.Hour)) || !strings.Contains(got["Longest run without you"].note, "long one") {
		t.Errorf("longest run: %+v", got["Longest run without you"])
	}
	if r.n != 3 || !r.oldest.Equal(start.Add(10*time.Hour)) {
		t.Errorf("n %d oldest %s", r.n, r.oldest)
	}
	if v := guessVerdict(8, 10); !strings.HasPrefix(v, "under by 20%") {
		t.Errorf("verdict %q", v)
	}
}

// A guess is saved next to the settings, and a second guess for the same range replaces it.
func TestGuess(t *testing.T) {
	gitInfo = func(dir string) (string, string) { return dir, "" }
	start := weekOf(time.Date(2026, 11, 18, 12, 0, 0, 0, time.Local))
	s := Session{Agent: "claude", Cwd: "/w", Events: []Event{{T: start.Add(10 * time.Hour), Human: true}, {T: start.Add(10*time.Hour + 10*time.Minute)}}}
	m := testModel([]Session{s}, nil, weekP, start, start.AddDate(0, 0, 7), 100, 30)
	m.cfgPath = filepath.Join(t.TempDir(), "config.json")
	for _, in := range []string{"1", "2"} {
		next, _ := m.key("p")
		m = next.(model)
		next, _ = m.Update(tea.KeyPressMsg{Code: rune(in[0]), Text: in})
		next, _ = next.(model).Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		m = next.(model)
		if !m.gs.done {
			t.Fatalf("guess %s not taken", in)
		}
		next, _ = m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"}) // any key closes
		m = next.(model)
	}
	gs := readGuesses(guessPath(m.cfgPath))
	if len(gs) != 1 || gs[0].Guess != 2 || gs[0].Period != "week" || gs[0].Measured < 0.16 || gs[0].Measured > 0.17 {
		t.Fatalf("guesses %+v", gs)
	}
}

// Live lists what is working and what waits on you: waits first, a working subagent folded into
// its session, finished subagents, quiet sessions and finished scripts left out.
func TestLive(t *testing.T) {
	gitInfo = func(dir string) (string, string) { return dir, "" }
	now := time.Date(2026, 11, 26, 13, 0, 0, 0, time.Local)
	at := func(min int) time.Time { return now.Add(-time.Duration(min) * time.Minute) }
	working := Session{Agent: "claude", File: "/p/a.jsonl", Cwd: "/w/app", Prompt: "build",
		Events: []Event{{T: at(40), Human: true}, {T: at(30)}, {T: at(12), Human: true}, {T: at(5)}, {T: at(1)}}}
	sub := Session{Agent: "claude-sub", File: "/p/a/subagents/agent-1.jsonl", Cwd: "/w/app", Events: []Event{{T: at(4)}, {T: at(2)}}}
	done := Session{Agent: "claude-sub", File: "/p/a/subagents/agent-2.jsonl", Cwd: "/w/app", Waiting: true, Events: []Event{{T: at(6)}, {T: at(3)}}}
	waiting := Session{Agent: "codex", File: "/c.jsonl", Cwd: "/w/side", Prompt: "fix", Waiting: true, Events: []Event{{T: at(9), Human: true}, {T: at(4)}}}
	quiet := Session{Agent: "claude", File: "/o.jsonl", Cwd: "/w/app", Events: []Event{{T: at(50), Human: true}, {T: at(20)}}}
	job := Session{Agent: "claude", File: "/j.jsonl", Cwd: "/jobs/x", Waiting: true, Events: []Event{{T: at(3), Human: true}, {T: at(2)}}}
	rules := []Rule{{Path: "/jobs", Whom: "automation"}, {Path: "/w/side", Whom: "side"}}
	d := liveFrom([]Session{working, sub, done, waiting, quiet, job}, rules, 15*time.Minute, now)
	if len(d.rows) != 2 || d.rows[0].File != "/c.jsonl" || d.rows[1].File != "/p/a.jsonl" || d.rows[1].subs != 1 {
		t.Fatalf("rows = %+v", d.rows)
	}
	if a, b := d.state(d.rows[0]), d.state(d.rows[1]); a != "waiting 4m" || b != "working 12m" {
		t.Errorf("states = %q, %q", a, b)
	}
	if w, x := d.counts(); w != 1 || x != 1 || !strings.HasPrefix(shortLine(w, x, d.today.ActiveHours), "●1 ◐1 · ") {
		t.Errorf("short = %q", shortLine(w, x, d.today.ActiveHours))
	}
	if got := shortLine(0, 0, 0); got != "· –" {
		t.Errorf("idle short = %q", got)
	}
}

// Each agent's log says when a turn ends: then the agent waits on you.
func TestWaiting(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		kind  string
		lines []string
		want  []bool // after each line
	}{
		{"claude", []string{
			`{"type":"user","timestamp":"2026-11-26T06:00:00Z","message":{"content":"hi"}}`,
			`{"type":"assistant","timestamp":"2026-11-26T06:00:01Z","message":{"content":[{"type":"tool_use"}],"stop_reason":"tool_use"}}`,
			`{"type":"assistant","timestamp":"2026-11-26T06:00:02Z","message":{"content":[{"type":"text","text":"done"}],"stop_reason":"end_turn"}}`,
			`{"type":"system","subtype":"turn_duration","timestamp":"2026-11-26T06:00:03Z"}`,
			`{"type":"user","timestamp":"2026-11-26T06:01:00Z","message":{"content":"more"}}`,
			`{"type":"user","timestamp":"2026-11-26T06:01:05Z","message":{"content":[{"type":"text","text":"[Request interrupted by user]"}]}}`,
		}, []bool{false, false, true, true, false, true}},
		{"codex", []string{
			`{"timestamp":"2026-11-26T06:00:00Z","type":"event_msg","payload":{"type":"task_started"}}`,
			`{"timestamp":"2026-11-26T06:00:01Z","type":"response_item","payload":{"type":"message","role":"assistant"}}`,
			`{"timestamp":"2026-11-26T06:00:02Z","type":"event_msg","payload":{"type":"task_complete"}}`,
			`{"timestamp":"2026-11-26T06:00:03Z","type":"event_msg","payload":{"type":"token_count"}}`,
			`{"timestamp":"2026-11-26T06:01:00Z","type":"event_msg","payload":{"type":"user_message"}}`,
		}, []bool{false, false, true, true, false}},
		{"antigravity", []string{
			`{"source":"USER_EXPLICIT","type":"USER_INPUT","created_at":"2026-11-26T06:00:00Z"}`,
			`{"source":"MODEL","type":"PLANNER_RESPONSE","created_at":"2026-11-26T06:00:01Z","tool_calls":[{"name":"run_command"}]}`,
			`{"source":"MODEL","type":"GENERIC","created_at":"2026-11-26T06:00:02Z"}`,
			`{"source":"MODEL","type":"PLANNER_RESPONSE","created_at":"2026-11-26T06:00:03Z"}`,
		}, []bool{false, false, false, true}},
	}
	for _, c := range cases {
		p := filepath.Join(dir, c.kind+".jsonl")
		s := Session{Agent: c.kind, File: p}
		var off int64
		for i, l := range c.lines {
			f, _ := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
			f.WriteString(l + "\n")
			f.Close()
			var err error
			if off, err = parseFrom(c.kind, p, off, &s); err != nil || s.Waiting != c.want[i] {
				t.Errorf("%s line %d: waiting = %v, want %v (err %v)", c.kind, i, s.Waiting, c.want[i], err)
			}
		}
	}
}

// The card hides who and what, keeps the numbers, and drops a line that would name them.
func TestCard(t *testing.T) {
	gitInfo = func(dir string) (string, string) { return dir, "" }
	day := time.Date(2026, 11, 23, 0, 0, 0, 0, time.Local)
	ev := func(m int, human bool) Event {
		return Event{T: day.Add(10*time.Hour + time.Duration(m)*time.Minute), Human: human}
	}
	sessions := []Session{{Agent: "claude", File: "/a", Cwd: "/w/office/app", Events: []Event{ev(0, true), ev(30, false), ev(40, true), ev(50, false)}}}
	rules := []Rule{{Path: "/w/office", Whom: "office"}}
	d := rangeFrom(sessions, weekP, day, day.AddDate(0, 0, 7), rules, 15*time.Minute, day.AddDate(0, 0, 7))
	shown, hidden := cardFrom(d, "week of Nov 23", rules, false), cardFrom(d, "week of Nov 23", rules, true)
	if shown.For[0].Name != "Office" || shown.Top[0].Name != "app" || !strings.Contains(shown.Line, "Office") {
		t.Errorf("shown = %+v", shown)
	}
	out := ansi.Strip(hidden.render())
	if hidden.For[0].Name != "" || hidden.Line != "" || strings.Contains(out, "Office") || strings.Contains(out, "app ") || !strings.Contains(out, "20m") {
		t.Errorf("hidden card:\n%s", out)
	}
	if got := roundH(8.04) + " " + roundH(12.6) + " " + roundH(0.5); got != "8h 13h 30m" {
		t.Errorf("roundH = %q", got)
	}
}

// The demo is a whole made-up developer: weeks of history, a record day, and right now two
// agents working and one waiting, each one openable.
func TestDemo(t *testing.T) {
	_, _ = startDemo()
	defer func() { demo, gitInfo = false, readGit }()
	now := time.Now()
	lv := readLive(demoConfig().Rules, 15*time.Minute)
	if w, x := lv.counts(); w != 2 || x != 1 || lv.rows[1].subs != 1 && lv.rows[2].subs != 1 {
		t.Fatalf("live: %d working, %d waiting: %+v", w, x, lv.rows)
	}
	for _, r := range lv.rows {
		if s, err := demoRead(r.File); err != nil || len(s.Events) == 0 || s.Events[0].text == "" {
			t.Errorf("can't open %s: %v", r.File, err)
		}
	}
	from, to := periodSpan(weekP, now.AddDate(0, 0, -7))
	tot := total(build(load(from, to), demoConfig().Rules, 15*time.Minute, from, to))
	if tot.AgentHours < 10 || tot.YouHours < 3 || tot.Peak < 3 {
		t.Errorf("last week: %+v", tot)
	}
	rec := records(attribute(load(now.AddDate(0, 0, -60), now), demoConfig().Rules, 15*time.Minute, now.AddDate(0, 0, -60), now), now)
	got := map[string]string{}
	for _, r := range rec.list {
		got[r.name] = r.value
	}
	if n, _ := strconv.Atoi(got["Most agents at once"]); n < 7 || got["Current streak"] != "17 days" {
		t.Errorf("records: %v", got)
	}
}

// A click in Settings picks the row it lands on, past the blank lines and headings between them.
func TestSettingsClick(t *testing.T) {
	gitInfo = func(dir string) (string, string) { return dir, "" }
	start := weekOf(time.Date(2026, 11, 18, 12, 0, 0, 0, time.Local))
	ev := func(m int, human bool) Event {
		return Event{T: start.Add(10*time.Hour + time.Duration(m)*time.Minute), Human: human}
	}
	sessions := []Session{{Agent: "claude", File: "/a", Cwd: "/w/nobody", Events: []Event{ev(0, true), ev(10, false), ev(20, false)}}}
	rules := []Rule{{Path: "/w/a", Whom: "a"}, {Path: "/w/b", Whom: "b"}}
	m := testModel(sessions, rules, weekP, start, start.AddDate(0, 0, 7), 120, 30)
	m.scr = settingsScr
	find := func(s string) int {
		for y, l := range strings.Split(ansi.Strip(m.View().Content), "\n") {
			if strings.Contains(l, s) {
				return y
			}
		}
		t.Fatalf("no %q on screen", s)
		return 0
	}
	for _, c := range []struct {
		text string
		want int
	}{{"Idle gap", 1}, {"/w/nobody", 4}, {"/w/b", 3}, {"Week starts", 0}} {
		next, _ := m.click(sideW+6, find(c.text))
		if m = next.(model); m.cur[settingsScr] != c.want {
			t.Errorf("click on %q: cursor %d, want %d", c.text, m.cur[settingsScr], c.want)
		}
	}
	if _, _ = m.click(sideW+6, find("Rules")); m.cur[settingsScr] != 0 {
		t.Errorf("a heading should pick nothing")
	}
}

func TestClaudeCleanupWarning(t *testing.T) {
	now := time.Now()
	sessions := []Session{{
		Agent:  "claude",
		File:   "/a.jsonl",
		Cwd:    "/w/app",
		Events: []Event{{T: now.Add(-2 * time.Hour), Human: true}, {T: now.Add(-1 * time.Hour), Human: false}},
	}}
	rules := []Rule{{Path: "/w/app", Whom: "app"}}

	newModel := func() model {
		gitInfo = func(dir string) (string, string) { return dir, "" }
		m := testModel(sessions, rules, weekP, now.Add(-24*time.Hour), now, 120, 40)
		m.scr = recordsScr
		recs := records(m.d.ats, now)
		m.rec = &recs
		return m
	}

	// 1. Unset (missing settings file) -> defaults to 30 and shows warning.
	t.Setenv("HOME", t.TempDir())
	out := ansi.Strip(newModel().View().Content)
	if !strings.Contains(out, "Claude Code keeps 30 days of transcripts (cleanupPeriodDays); raise it to keep your history") {
		t.Errorf("unset settings should warn with 30 days, got:\n%s", out)
	}

	// 2. Explicitly 30 (< 90) -> shows warning with 30.
	home30 := t.TempDir()
	claudeDir30 := filepath.Join(home30, ".claude")
	if err := os.MkdirAll(claudeDir30, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, claudeDir30, "settings.json", `{"cleanupPeriodDays": 30}`)
	t.Setenv("HOME", home30)
	out = ansi.Strip(newModel().View().Content)
	if !strings.Contains(out, "Claude Code keeps 30 days of transcripts (cleanupPeriodDays); raise it to keep your history") {
		t.Errorf("cleanupPeriodDays=30 should warn, got:\n%s", out)
	}

	// 3. Raised to 3650 (>= 90) -> suppresses warning.
	home3650 := t.TempDir()
	claudeDir3650 := filepath.Join(home3650, ".claude")
	if err := os.MkdirAll(claudeDir3650, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, claudeDir3650, "settings.json", `{"cleanupPeriodDays": 3650}`)
	t.Setenv("HOME", home3650)
	out = ansi.Strip(newModel().View().Content)
	if strings.Contains(out, "cleanupPeriodDays") {
		t.Errorf("cleanupPeriodDays=3650 should not warn, got:\n%s", out)
	}
}
