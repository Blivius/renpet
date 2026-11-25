// Command renpet reads local AI-agent transcripts (Claude Code, Codex) and reports where the
// time went: per project, for whom (office / blivius / self ...), agent-running vs your
// think time, and how many agents ran in parallel. P0 spike: read-only, no daemon, no DB.
package main

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

//go:embed rules.json
var defaultRules []byte

// Event is one timestamped line in a transcript. Human marks a prompt you typed; everything
// else (model output, tool results, injected notifications) is agent activity.
type Event struct {
	T     time.Time
	Human bool
}

type Session struct {
	Agent, File, Cwd string
	Events           []Event
}

type Span struct{ Start, End time.Time }

func (s Span) Len() time.Duration { return s.End.Sub(s.Start) }

// Rule maps a git remote substring or a path prefix to "for whom". First match wins.
type Rule struct {
	Remote string `json:"remote,omitempty"`
	Path   string `json:"path,omitempty"`
	Whom   string `json:"whom"`
}

// Lines that arrive as "user" messages but were not typed by a human.
var machinePrefixes = []string{
	"<task-notification", "<local-command-stdout", "<local-command-caveat", "<system-reminder",
	"This session is being continued", "<artifact-content", "<external_codex", "<send_user_message",
	"<bash-stdout", "<bash-stderr", "<environment_context", "<user_instructions", "<turn_aborted",
	"# AGENTS.md", "Caveat:",
}

func typedByHuman(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	for _, p := range machinePrefixes {
		if strings.HasPrefix(text, p) {
			return false
		}
	}
	return true
}

// eachLine calls fn for every line; lines can be megabytes, so no bufio.Scanner.
func eachLine(path string, fn func([]byte)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			fn(line)
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// loadClaude reads a Claude Code transcript. In a subagent transcript every "user" line was
// written by the parent agent, so nothing in it counts as your prompt.
func loadClaude(path string, sub bool) (Session, error) {
	s := Session{Agent: "claude", File: path}
	if sub {
		s.Agent = "claude-sub"
	}
	err := eachLine(path, func(b []byte) {
		var l struct {
			Type      string    `json:"type"`
			Timestamp time.Time `json:"timestamp"`
			Cwd       string    `json:"cwd"`
			IsMeta    bool      `json:"isMeta"`
			Message   struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(b, &l) != nil || l.Timestamp.IsZero() || (l.Type != "user" && l.Type != "assistant") {
			return
		}
		if s.Cwd == "" {
			s.Cwd = l.Cwd
		}
		human := false
		if l.Type == "user" && !l.IsMeta && !sub {
			var str string
			var arr []struct{ Type, Text string }
			if json.Unmarshal(l.Message.Content, &str) == nil {
				human = typedByHuman(str)
			} else if json.Unmarshal(l.Message.Content, &arr) == nil && len(arr) > 0 && arr[0].Type == "text" {
				human = typedByHuman(arr[0].Text) // tool_result lines stay agent activity
			}
		}
		s.Events = append(s.Events, Event{l.Timestamp, human})
	})
	return s, err
}

func loadCodex(path string) (Session, error) {
	s := Session{Agent: "codex", File: path}
	err := eachLine(path, func(b []byte) {
		var l struct {
			Timestamp time.Time `json:"timestamp"`
			Type      string    `json:"type"`
			Payload   struct {
				Type    string          `json:"type"`
				Role    string          `json:"role"`
				Cwd     string          `json:"cwd"`
				Content json.RawMessage `json:"content"`
			} `json:"payload"`
		}
		if json.Unmarshal(b, &l) != nil || l.Timestamp.IsZero() {
			return
		}
		switch l.Type {
		case "session_meta":
			s.Cwd = l.Payload.Cwd
			return
		case "turn_context":
			return
		}
		human := false
		if l.Type == "response_item" && l.Payload.Type == "message" && l.Payload.Role == "user" {
			var parts []struct{ Text string }
			if json.Unmarshal(l.Payload.Content, &parts) == nil && len(parts) > 0 {
				human = typedByHuman(parts[0].Text)
			}
		}
		s.Events = append(s.Events, Event{l.Timestamp, human})
	})
	return s, err
}

// loadAntigravity reads brain/<id>/.system_generated/logs/transcript.jsonl. Only
// USER_EXPLICIT lines are you; SYSTEM lines include other agents' dispatches. The transcript
// has no cwd, so the caller passes the workspace from conversation_metadata.json ("" if unknown).
func loadAntigravity(path, cwd string) (Session, error) {
	s := Session{Agent: "antigravity", File: path, Cwd: cwd}
	err := eachLine(path, func(b []byte) {
		var l struct {
			CreatedAt time.Time `json:"created_at"`
			Source    string    `json:"source"`
		}
		if json.Unmarshal(b, &l) != nil || l.CreatedAt.IsZero() {
			return
		}
		s.Events = append(s.Events, Event{l.CreatedAt, l.Source == "USER_EXPLICIT"})
	})
	return s, err
}

// antigravityWorkspaces maps conversation id → workspace path from the CLI's (undocumented)
// cache/conversation_metadata.json. Missing or changed format just means "unknown".
func antigravityWorkspaces(home string) map[string]string {
	out := map[string]string{}
	files, _ := filepath.Glob(filepath.Join(home, ".gemini", "antigravity*", "cache", "conversation_metadata.json"))
	for _, f := range files {
		var meta struct {
			Conversations map[string]struct {
				Summary struct {
					WorkspaceURIs []string `json:"WorkspaceURIs"`
				} `json:"summary"`
			} `json:"conversations"`
		}
		b, err := os.ReadFile(f)
		if err != nil || json.Unmarshal(b, &meta) != nil {
			continue
		}
		for id, c := range meta.Conversations {
			if len(c.Summary.WorkspaceURIs) == 0 {
				continue
			}
			if u, err := url.Parse(c.Summary.WorkspaceURIs[0]); err == nil && u.Scheme == "file" {
				out[id] = u.Path // decodes %20 etc.
			}
		}
	}
	return out
}

// spans classifies each gap between consecutive events by what ends it: a gap ending in your
// prompt was you reading and thinking; a gap ending in agent output was the agent working.
// Gaps longer than idle are nobody's time. Waiting on a permission prompt counts as agent
// time here; P1 hooks can split it out.
func spans(s Session, idle time.Duration, from, to time.Time) (agent, think []Span) {
	ev := s.Events // sorted in place so callers can read first/last afterwards
	sort.SliceStable(ev, func(i, j int) bool { return ev[i].T.Before(ev[j].T) })
	for i := 1; i < len(ev); i++ {
		gap := ev[i].T.Sub(ev[i-1].T)
		if gap <= 0 || gap > idle {
			continue
		}
		sp := Span{maxT(ev[i-1].T, from), minT(ev[i].T, to)}
		if sp.Len() <= 0 {
			continue
		}
		if ev[i].Human {
			think = append(think, sp)
		} else {
			agent = append(agent, sp)
		}
	}
	return
}

// promptGaps returns, for every prompt you typed in the window, how long you took after the
// event before it. Short gaps are you reading and thinking; long ones are you being away. The
// idle gap should sit where the first kind ends.
func promptGaps(sessions []Session, from, to time.Time) []time.Duration {
	var out []time.Duration
	for _, s := range sessions {
		ev := s.Events
		sort.SliceStable(ev, func(i, j int) bool { return ev[i].T.Before(ev[j].T) })
		for i := 1; i < len(ev); i++ {
			if ev[i].Human && !ev[i].T.Before(from) && ev[i].T.Before(to) {
				if g := ev[i].T.Sub(ev[i-1].T); g > 0 {
					out = append(out, g)
				}
			}
		}
	}
	return out
}

func printGaps(gaps []time.Duration) {
	if len(gaps) == 0 {
		fmt.Println("no prompts in this window")
		return
	}
	edges := []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 15 * time.Minute,
		20 * time.Minute, 30 * time.Minute, time.Hour, 2 * time.Hour, 1 << 62}
	labels := []string{"<1m", "1–2m", "2–5m", "5–10m", "10–15m", "15–20m", "20–30m", "30–60m", "1–2h", ">2h"}
	counts := make([]int, len(edges))
	for _, g := range gaps {
		for i, e := range edges {
			if g < e {
				counts[i]++
				break
			}
		}
	}
	fmt.Printf("how long you took to send each prompt after the agent's last event (%d prompts)\n\n", len(gaps))
	cum := 0
	for i, c := range counts {
		cum += c
		bar := strings.Repeat("█", c*50/len(gaps))
		fmt.Printf("%7s %5d  %5.1f%%  %s\n", labels[i], c, 100*float64(cum)/float64(len(gaps)), bar)
	}
	fmt.Println("\nThe % column is cumulative. Set --idle where the bars thin out: gaps past that point are")
	fmt.Println("you being away, not you thinking. Too low drops real review time; too high counts lunch.")
}

func maxT(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minT(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func sum(sp []Span) (d time.Duration) {
	for _, s := range sp {
		d += s.Len()
	}
	return
}

// unionLen is wall-clock time covered by at least one span.
func unionLen(sp []Span) time.Duration {
	s := append([]Span(nil), sp...)
	sort.Slice(s, func(i, j int) bool { return s[i].Start.Before(s[j].Start) })
	var total time.Duration
	var cur *Span
	for i := range s {
		if cur != nil && !s[i].Start.After(cur.End) {
			cur.End = maxT(cur.End, s[i].End)
			continue
		}
		if cur != nil {
			total += cur.Len()
		}
		cur = &s[i]
	}
	if cur != nil {
		total += cur.Len()
	}
	return total
}

// peak is the most spans open at once (sweep line; an end at t closes before a start at t).
func peak(sp []Span) int {
	type edge struct {
		t time.Time
		d int
	}
	edges := make([]edge, 0, 2*len(sp))
	for _, s := range sp {
		edges = append(edges, edge{s.Start, 1}, edge{s.End, -1})
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].t.Equal(edges[j].t) {
			return edges[i].d < edges[j].d
		}
		return edges[i].t.Before(edges[j].t)
	})
	cur, hi := 0, 0
	for _, e := range edges {
		cur += e.d
		hi = max(hi, cur)
	}
	return hi
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}

func within(path, prefix string) bool {
	return path == prefix || strings.HasPrefix(path, strings.TrimSuffix(prefix, "/")+"/")
}

func whom(rules []Rule, cwd, remote string) string {
	rl := strings.ToLower(remote)
	for _, r := range rules {
		if r.Remote != "" && rl != "" && strings.Contains(rl, strings.ToLower(r.Remote)) {
			return r.Whom
		}
		if r.Path != "" && within(cwd, expandHome(r.Path)) {
			return r.Whom
		}
	}
	return "unknown"
}

// gitInfo returns the repo root (or dir itself) and origin URL. A var so tests stay hermetic.
var gitInfo = func(dir string) (top, remote string) {
	top = dir
	if out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output(); err == nil {
		top = strings.TrimSpace(string(out))
	}
	if out, err := exec.Command("git", "-C", dir, "remote", "get-url", "origin").Output(); err == nil {
		remote = strings.TrimSpace(string(out))
	}
	return
}

type SessionLine struct {
	Agent      string    `json:"agent"`
	File       string    `json:"file"`
	First      time.Time `json:"first"`
	Last       time.Time `json:"last"`
	AgentHours float64   `json:"agent_hours"`
	ThinkHours float64   `json:"think_hours"`
}

type Row struct {
	Project     string        `json:"project"`
	Whom        string        `json:"whom"`
	ActiveHours float64       `json:"with_agents_hours"`
	AgentHours  float64       `json:"agent_run_hours"`
	ThinkHours  float64       `json:"think_hours"`
	Peak        int           `json:"peak_parallel"`
	Sessions    []SessionLine `json:"sessions"`
}

type Report struct {
	From, To time.Time
	Idle     string
	Rows     []Row // per project, then per whom (Project == "*"), then the grand total
}

type acc struct {
	agent, think []Span
	sessions     []SessionLine
}

func (a *acc) row(project, who string) Row {
	all := append(append([]Span(nil), a.agent...), a.think...)
	return Row{project, who, hours(unionLen(all)), hours(sum(a.agent)), hours(sum(a.think)), peak(a.agent), a.sessions}
}

func hours(d time.Duration) float64 { return d.Hours() }

func tilde(p string) string {
	if h, err := os.UserHomeDir(); err == nil && within(p, h) {
		return "~" + strings.TrimPrefix(p, h)
	}
	return p
}

// Attributed is one session's time inside the window, with its project and "for whom".
type Attributed struct {
	Session
	Running, Thinking []Span
	Project, Whom     string
}

// attribute splits every session into spans and labels it. Sessions with no time in the
// window are dropped.
func attribute(sessions []Session, rules []Rule, idle time.Duration, from, to time.Time) []Attributed {
	var out []Attributed
	cache := map[string][2]string{}
	for _, s := range sessions {
		agent, think := spans(s, idle, from, to)
		if len(agent)+len(think) == 0 {
			continue
		}
		project, w := "(no workspace)", "unknown"
		if s.Cwd != "" {
			info, ok := cache[s.Cwd]
			if !ok {
				top, remote := gitInfo(s.Cwd)
				info = [2]string{top, remote}
				cache[s.Cwd] = info
			}
			project, w = tilde(info[0]), whom(rules, s.Cwd, info[1])
		}
		out = append(out, Attributed{s, agent, think, project, w})
	}
	return out
}

// build is the pure core: sessions in, attributed rows out.
func build(sessions []Session, rules []Rule, idle time.Duration, from, to time.Time) Report {
	type key struct{ project, who string }
	projects, whoms := map[key]*acc{}, map[string]*acc{}
	total := &acc{}
	for _, at := range attribute(sessions, rules, idle, from, to) {
		k := key{at.Project, at.Whom}
		if projects[k] == nil {
			projects[k] = &acc{}
		}
		if whoms[at.Whom] == nil {
			whoms[at.Whom] = &acc{}
		}
		ev := at.Session.Events
		line := SessionLine{at.Session.Agent, tilde(at.File), ev[0].T, ev[len(ev)-1].T, hours(sum(at.Running)), hours(sum(at.Thinking))}
		for _, a := range []*acc{projects[k], whoms[at.Whom], total} {
			a.agent = append(a.agent, at.Running...)
			a.think = append(a.think, at.Thinking...)
		}
		projects[k].sessions = append(projects[k].sessions, line)
	}
	r := Report{From: from, To: to, Idle: idle.String()}
	for k, a := range projects {
		r.Rows = append(r.Rows, a.row(k.project, k.who))
	}
	sort.Slice(r.Rows, func(i, j int) bool { return r.Rows[i].ActiveHours > r.Rows[j].ActiveHours })
	var byWhom []Row
	for w, a := range whoms {
		byWhom = append(byWhom, a.row("*", w))
	}
	sort.Slice(byWhom, func(i, j int) bool { return byWhom[i].ActiveHours > byWhom[j].ActiveHours })
	for i := range byWhom {
		byWhom[i].Sessions = nil
	}
	tot := total.row("*", "TOTAL")
	tot.Sessions = nil
	r.Rows = append(append(r.Rows, byWhom...), tot)
	return r
}

func discover(from time.Time) (files map[string]string) {
	home, _ := os.UserHomeDir()
	files = map[string]string{}
	keep := func(p string) bool { // a file last written before `from` cannot hold events in range
		st, err := os.Stat(p)
		return err == nil && !st.ModTime().Before(from)
	}
	globs := map[string]string{
		filepath.Join(home, ".claude", "projects", "*", "*.jsonl"):                                                    "claude",
		filepath.Join(home, ".claude", "projects", "*", "*", "subagents", "*.jsonl"):                                  "claude-sub",
		filepath.Join(home, ".gemini", "antigravity*", "brain", "*", ".system_generated", "logs", "transcript.jsonl"): "antigravity",
	}
	for g, kind := range globs {
		matches, _ := filepath.Glob(g)
		for _, p := range matches {
			if keep(p) {
				files[p] = kind
			}
		}
	}
	_ = filepath.WalkDir(filepath.Join(home, ".codex", "sessions"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasPrefix(d.Name(), "rollout-") && strings.HasSuffix(p, ".jsonl") && keep(p) {
			files[p] = "codex"
		}
		return nil
	})
	return
}

func load(from time.Time) []Session {
	var out []Session
	home, _ := os.UserHomeDir()
	workspaces := antigravityWorkspaces(home)
	for p, kind := range discover(from) {
		var s Session
		var err error
		switch kind {
		case "claude", "claude-sub":
			s, err = loadClaude(p, kind == "claude-sub")
		case "codex":
			s, err = loadCodex(p)
		case "antigravity": // .../brain/<conversation id>/.system_generated/logs/transcript.jsonl
			id := filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(p))))
			s, err = loadAntigravity(p, workspaces[id])
		}
		if err == nil && len(s.Events) > 0 {
			out = append(out, s)
		} else if err != nil {
			fmt.Fprintln(os.Stderr, "skip:", p, err)
		}
	}
	return out
}

func h(x float64) string { return fmt.Sprintf("%.1f", x) }

func printWeek(w io.Writer, r Report, all bool) {
	fmt.Fprintf(w, "%s → %s  (idle gap %s)\n\n", r.From.Format("2006-01-02 15:04"), r.To.Format("2006-01-02 15:04"), r.Idle)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "project\tfor whom\twith agents h\tagent-run h\tthink h\tpeak ∥\tsessions\t")
	hidden := 0
	for _, row := range r.Rows {
		if !all && row.Project != "*" && row.ActiveHours < 0.1 {
			hidden++ // still counted in the per-whom and TOTAL rows
			continue
		}
		name := row.Project
		if name == "*" {
			name = "— " + row.Whom
		}
		n := ""
		if row.Sessions != nil {
			n = fmt.Sprint(len(row.Sessions))
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t\n", name, row.Whom, h(row.ActiveHours), h(row.AgentHours), h(row.ThinkHours), row.Peak, n)
	}
	tw.Flush()
	if hidden > 0 {
		fmt.Fprintf(w, "\n%d projects under 6 min hidden (still in the totals; --all shows them)\n", hidden)
	}
	fmt.Fprintln(w, "per-whom rows can add up to more than TOTAL: overlap = agents for different whoms at once")
	fmt.Fprintln(w, "\nwith agents = wall-clock with any agent session active · agent-run = summed agent working time")
	fmt.Fprintln(w, "think = gaps ending in your prompt · peak ∥ = most agents working at once")
}

// Cell is one quarter hour of a day: how many agents were working at once, how long you spent
// thinking, and who the cell's time mostly went to ("" when nobody worked).
type Cell struct {
	Peak  int
	Think time.Duration
	Whom  string
}

// laneCells splits a local day into cells of the given size (15m → 96 cells, 30m → 48).
func laneCells(ats []Attributed, day time.Time, cell time.Duration) []Cell {
	out := make([]Cell, int(24*time.Hour/cell))
	for i := range out {
		start := day.Add(time.Duration(i) * cell)
		end := start.Add(cell)
		var running []Span
		per := map[string]time.Duration{}
		for _, at := range ats {
			for _, s := range at.Running {
				if c := (Span{maxT(s.Start, start), minT(s.End, end)}); c.Len() > 0 {
					running = append(running, c)
					per[at.Whom] += c.Len()
				}
			}
			for _, s := range at.Thinking {
				if c := (Span{maxT(s.Start, start), minT(s.End, end)}); c.Len() > 0 {
					out[i].Think += c.Len()
					per[at.Whom] += c.Len()
				}
			}
		}
		out[i].Peak = peak(running)
		var best time.Duration
		for k, d := range per {
			if d > best || (d == best && k < out[i].Whom) { // ties break alphabetically: deterministic
				out[i].Whom, best = k, d
			}
		}
	}
	return out
}

// laneRows draws the cells as plain text, one character each:
// agents = most agents working at once (. none, 1-9, + more); you = thinking (▄ some, █ ≥5 min);
// whom = who that cell's time mostly went to (first letter, ? for unknown).
func laneRows(ats []Attributed, day time.Time) (agents, you, whom string) {
	var a, y, w strings.Builder
	for _, c := range laneCells(ats, day, 15*time.Minute) {
		switch {
		case c.Peak == 0:
			a.WriteByte('.')
		case c.Peak > 9:
			a.WriteByte('+')
		default:
			a.WriteByte(byte('0' + c.Peak))
		}
		switch {
		case c.Think >= 5*time.Minute:
			y.WriteString("█")
		case c.Think > 0:
			y.WriteString("▄")
		default:
			y.WriteByte('.')
		}
		switch c.Whom {
		case "":
			w.WriteByte('.')
		case "unknown":
			w.WriteByte('?')
		default:
			w.WriteString(c.Whom[:1])
		}
	}
	return a.String(), y.String(), w.String()
}

func printDay(ats []Attributed, day time.Time) {
	agents, you, whom := laneRows(ats, day)
	var ruler strings.Builder
	for hr := range 24 {
		fmt.Fprintf(&ruler, "%-4d", hr)
	}
	fmt.Printf("%s  (one cell = 15 min, local time)\n\n", day.Format("Mon 2006-01-02"))
	fmt.Printf("        %s\n agents %s\n    you %s\n   whom %s\n\n", ruler.String(), agents, you, whom)
	fmt.Println("agents: most working at once · you: ▄ thinking, █ ≥5 min · whom: o office, b blivius, s self, a automation, ? unknown")
	fmt.Println()
}

func printWhy(r Report, needle string) {
	needle = strings.ToLower(needle)
	found := false
	for _, row := range r.Rows {
		if row.Project == "*" || !strings.Contains(strings.ToLower(row.Project), needle) {
			continue
		}
		found = true
		fmt.Printf("%s  [%s]  with agents %sh = agent-run %sh + think %sh (minus overlap)\n", row.Project, row.Whom, h(row.ActiveHours), h(row.AgentHours), h(row.ThinkHours))
		sort.Slice(row.Sessions, func(i, j int) bool { return row.Sessions[i].First.Before(row.Sessions[j].First) })
		for _, s := range row.Sessions {
			fmt.Printf("  %-6s %s → %s  agent %sh think %sh\n    %s\n", s.Agent, s.First.Local().Format("01-02 15:04:05"), s.Last.Local().Format("01-02 15:04:05"), h(s.AgentHours), h(s.ThinkHours), s.File)
		}
	}
	if !found {
		fmt.Println("no project matches", needle)
	}
}

// animate is true only on an interactive terminal that hasn't opted out of colour, so pipes,
// CI and NO_COLOR get one static frame, as in termheat.
func animate() bool {
	st, err := os.Stdout.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0 && os.Getenv("NO_COLOR") == ""
}

func main() {
	if len(os.Args) < 2 || (os.Args[1] != "week" && os.Args[1] != "why" && os.Args[1] != "day" && os.Args[1] != "gaps") {
		fmt.Fprintln(os.Stderr, "usage: renpet week [--since YYYY-MM-DD] [--until YYYY-MM-DD] [--idle 15m] [--rules file] [--json] [--all]\n       renpet day [YYYY-MM-DD] [--idle 15m] [--rules file] [--all] [--static]\n       renpet why <project> [week flags]\n       renpet gaps [--since …] [--until …]   # helps pick --idle")
		os.Exit(2)
	}
	cmd := os.Args[1]
	fl := flag.NewFlagSet(cmd, flag.ExitOnError)
	now := time.Now()
	y, m, d := now.AddDate(0, 0, -7).Date()
	since := fl.String("since", time.Date(y, m, d, 0, 0, 0, 0, time.Local).Format("2006-01-02"), "start date (local)")
	until := fl.String("until", "", "end date, exclusive (local); default now")
	idle := fl.Duration("idle", 15*time.Minute, "a gap longer than this between events is idle, not work")
	rulesPath := fl.String("rules", "", "rules JSON; default is the embedded rules.json")
	asJSON := fl.Bool("json", false, "print JSON")
	all := fl.Bool("all", false, "show projects under 6 minutes too")
	static := fl.Bool("static", false, "day: print plain text instead of the animated view")
	args := os.Args[2:]
	needle := ""
	if cmd == "why" {
		if len(args) == 0 || strings.HasPrefix(args[0], "-") {
			fmt.Fprintln(os.Stderr, "usage: renpet why <project> [flags]")
			os.Exit(2)
		}
		needle, args = args[0], args[1:]
	}
	dayArg := now.Format("2006-01-02")
	if cmd == "day" && len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		dayArg, args = args[0], args[1:]
	}
	_ = fl.Parse(args)
	if cmd == "day" {
		*since = dayArg
	}

	from, err := time.ParseInLocation("2006-01-02", *since, time.Local)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bad date:", err)
		os.Exit(2)
	}
	to := now
	if cmd == "day" {
		to = from.AddDate(0, 0, 1)
	} else if *until != "" {
		if to, err = time.ParseInLocation("2006-01-02", *until, time.Local); err != nil {
			fmt.Fprintln(os.Stderr, "bad --until:", err)
			os.Exit(2)
		}
	}
	raw := defaultRules
	if *rulesPath != "" {
		if raw, err = os.ReadFile(*rulesPath); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	var rules []Rule
	if err := json.Unmarshal(raw, &rules); err != nil {
		fmt.Fprintln(os.Stderr, "bad rules:", err)
		os.Exit(1)
	}

	if cmd == "day" && !*asJSON && !*static && animate() {
		if err := runDayTUI(rules, *idle, from); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	sessions := load(from)
	if cmd == "gaps" {
		printGaps(promptGaps(sessions, from, to))
		return
	}
	r := build(sessions, rules, *idle, from, to)
	if cmd == "day" && !*asJSON {
		printDay(attribute(sessions, rules, *idle, from, to), from)
	}
	switch {
	case *asJSON:
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(r)
	case cmd == "why":
		printWhy(r, needle)
	default:
		printWeek(os.Stdout, r, *all)
	}
}
