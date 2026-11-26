// Command renpet reads local AI-agent transcripts (Claude Code, Codex) and reports where the
// time went: per project, for whom (work / side / self ...), agent-running vs your
// think time, and how many agents ran in parallel. P0 spike: read-only, no daemon, no DB.
package main

import (
	"bufio"
	_ "embed"
	"encoding/gob"
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
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/charmbracelet/x/ansi"
)

//go:embed rules.json
var defaultRules []byte

// Config lives in ~/.config/renpet/config.json ($XDG_CONFIG_HOME and $RENPET_CONFIG respected).
// Everything personal (who your folders and remotes belong to) belongs here, never in the repo.
type Config struct {
	WeekStart string `json:"week_start"` // monday (default) … sunday
	Idle      string `json:"idle"`       // a gap longer than this is idle, e.g. "15m"
	Rules     []Rule `json:"rules"`      // ordered "for whom" rules; first match wins
}

func configPath() string {
	if p := os.Getenv("RENPET_CONFIG"); p != "" {
		return p
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "renpet", "config.json")
}

// loadConfig returns the defaults overlaid with the config file, if there is one.
func loadConfig(path string) (Config, error) {
	c := Config{WeekStart: "monday", Idle: "15m"}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &c); err != nil {
			return c, fmt.Errorf("%s: %w", path, err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return c, err
	}
	if c.Rules == nil {
		if err := json.Unmarshal(defaultRules, &c.Rules); err != nil {
			return c, err
		}
	}
	if _, ok := weekdays[strings.ToLower(c.WeekStart)]; !ok {
		return c, fmt.Errorf("%s: week_start %q: want monday … sunday", path, c.WeekStart)
	}
	if _, err := time.ParseDuration(c.Idle); err != nil {
		return c, fmt.Errorf("%s: idle: %w", path, err)
	}
	return c, nil
}

var weekdays = map[string]time.Weekday{"sunday": time.Sunday, "monday": time.Monday, "tuesday": time.Tuesday,
	"wednesday": time.Wednesday, "thursday": time.Thursday, "friday": time.Friday, "saturday": time.Saturday}

// weekStart is the first day of a week in every view; set once from config or --week-start.
var weekStart = time.Monday

// Event is one timestamped line in a transcript. Human marks a prompt you typed; everything
// else (model output, tool results, injected notifications) is agent activity.
type Event struct {
	T     time.Time
	Human bool
	text  string // your prompt, cleaned; never cached (gob skips unexported fields)
}

// Session is one transcript. Title is the name you gave it, AITitle the one Claude Code gave
// it, Prompt the start of your first prompt: the cache keeps these, not your other prompts.
// Cwd is the folder it started in; Moves are the folders it moved to later (a cd, or a resume
// from another repo), each with when. Waiting is true when the agent's last turn ended (Claude's
// end_turn, Codex's task_complete, Antigravity's answer with no tool calls): it waits on you.
type Session struct {
	Agent, File, Cwd       string
	Title, AITitle, Prompt string
	Events                 []Event
	Moves                  []Move
	Waiting                bool
}

type Move struct {
	T   time.Time
	Cwd string
}

// move notes the folder a line was written in, keeping only changes.
func (s *Session) move(t time.Time, cwd string) {
	if cwd == "" {
		return
	}
	if s.Cwd == "" {
		s.Cwd = cwd
		return
	}
	cur := s.Cwd
	if n := len(s.Moves); n > 0 {
		cur = s.Moves[n-1].Cwd
	}
	if cwd != cur {
		if n := len(s.Events); n > 0 { // a gap counts for whatever ends it: the cut goes at the event before
			t = s.Events[n-1].T
		}
		s.Moves = append(s.Moves, Move{t, cwd})
	}
}

// Name is how a session is shown: your title, the agent's title, or your first prompt.
func (s Session) Name() string {
	for _, n := range []string{s.Title, s.AITitle, s.Prompt} {
		if n != "" {
			return n
		}
	}
	return "(no prompt)"
}

// add appends an event. A prompt you typed keeps its text, and the first one names the session.
func (s *Session) add(t time.Time, human bool, text string) {
	e := Event{T: t, Human: human}
	if human {
		e.text = cleanPrompt(text)
		if s.Prompt == "" && !bareCommand.MatchString(e.text) { // "/clear" names nothing
			s.Prompt = ansi.Truncate(e.text, 120, "…")
		}
	}
	s.Events = append(s.Events, e)
}

var (
	tagRe       = regexp.MustCompile(`</?[A-Za-z][\w-]*>`)
	bareCommand = regexp.MustCompile(`^/\S+$`)
)

// cleanPrompt turns a raw prompt into one readable line: a slash command reads as you typed it
// ("/moeen go there…"), wrapper tags go, and whitespace collapses.
func cleanPrompt(s string) string {
	if _, rest, ok := strings.Cut(s, "<command-name>"); ok {
		name, _, _ := strings.Cut(rest, "</command-name>")
		_, args, _ := strings.Cut(rest, "<command-args>")
		args, _, _ = strings.Cut(args, "</command-args>")
		s = name + " " + args
	}
	return ansi.Truncate(strings.Join(strings.Fields(tagRe.ReplaceAllString(s, " ")), " "), 1000, "…")
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

// eachLine calls fn for every complete line from byte offset off and returns the offset just
// past the last complete line. A trailing line without its newline is still being written: it
// is left for the next read. Lines can be megabytes, so no bufio.Scanner.
func eachLine(path string, off int64, fn func([]byte)) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return off, err
	}
	defer f.Close()
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return off, err
	}
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if err == nil {
			fn(line)
			off += int64(len(line))
			continue
		}
		if errors.Is(err, io.EOF) {
			return off, nil
		}
		return off, err
	}
}

// loadClaude reads a Claude Code transcript. In a subagent transcript every "user" line was
// written by the parent agent, so nothing in it counts as your prompt.
func loadClaude(path string, sub bool) (Session, error) {
	s := Session{Agent: "claude", File: path}
	if sub {
		s.Agent = "claude-sub"
	}
	_, err := parseClaude(path, 0, &s, sub)
	return s, err
}

func parseClaude(path string, off int64, s *Session, sub bool) (int64, error) {
	return eachLine(path, off, func(b []byte) {
		var l struct {
			Type        string    `json:"type"`
			Timestamp   time.Time `json:"timestamp"`
			Cwd         string    `json:"cwd"`
			IsMeta      bool      `json:"isMeta"`
			CustomTitle string    `json:"customTitle"`
			AITitle     string    `json:"aiTitle"`
			Message     struct {
				Content    json.RawMessage `json:"content"`
				StopReason string          `json:"stop_reason"`
			} `json:"message"`
		}
		if json.Unmarshal(b, &l) != nil {
			return
		}
		switch l.Type {
		case "custom-title": // you renamed the session; the latest name wins
			s.Title = l.CustomTitle
			return
		case "ai-title":
			s.AITitle = l.AITitle
			return
		}
		if l.Timestamp.IsZero() || (l.Type != "user" && l.Type != "assistant") {
			return
		}
		s.move(l.Timestamp, l.Cwd)
		s.Waiting = l.Message.StopReason == "end_turn"
		human, text := false, ""
		if l.Type == "user" && !l.IsMeta && !sub {
			var arr []struct{ Type, Text string }
			if json.Unmarshal(l.Message.Content, &text) != nil &&
				json.Unmarshal(l.Message.Content, &arr) == nil && len(arr) > 0 && arr[0].Type == "text" {
				text = arr[0].Text // tool_result lines stay agent activity
			}
			human = typedByHuman(text)
			s.Waiting = strings.HasPrefix(text, "[Request interrupted") // you pressed esc: the turn is over
		}
		s.add(l.Timestamp, human, text)
	})
}

func loadCodex(path string) (Session, error) {
	s := Session{Agent: "codex", File: path}
	_, err := parseCodex(path, 0, &s)
	return s, err
}

func parseCodex(path string, off int64, s *Session) (int64, error) {
	return eachLine(path, off, func(b []byte) {
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
		case "turn_context": // one per turn, with the folder that turn ran in
			s.move(l.Timestamp, l.Payload.Cwd)
			return
		case "event_msg":
			switch l.Payload.Type {
			case "task_complete":
				s.Waiting = true
			case "task_started", "user_message":
				s.Waiting = false
			}
		}
		human, text := false, ""
		if l.Type == "response_item" && l.Payload.Type == "message" && l.Payload.Role == "user" {
			var parts []struct{ Text string }
			if json.Unmarshal(l.Payload.Content, &parts) == nil && len(parts) > 0 {
				text = parts[0].Text
				human = typedByHuman(text)
			}
		}
		s.add(l.Timestamp, human, text)
	})
}

// loadAntigravity reads brain/<id>/.system_generated/logs/transcript.jsonl. Only
// USER_EXPLICIT lines are you; SYSTEM lines include other agents' dispatches. The transcript
// has no cwd, so the caller passes the workspace from conversation_metadata.json ("" if unknown).
func loadAntigravity(path, cwd string) (Session, error) {
	s := Session{Agent: "antigravity", File: path, Cwd: cwd}
	_, err := parseAntigravity(path, 0, &s)
	return s, err
}

func parseAntigravity(path string, off int64, s *Session) (int64, error) {
	return eachLine(path, off, func(b []byte) {
		var l struct {
			CreatedAt time.Time         `json:"created_at"`
			Source    string            `json:"source"`
			Type      string            `json:"type"`
			Content   string            `json:"content"`
			ToolCalls []json.RawMessage `json:"tool_calls"`
		}
		if json.Unmarshal(b, &l) != nil || l.CreatedAt.IsZero() {
			return
		}
		switch {
		case l.Source == "USER_EXPLICIT":
			s.Waiting = false
		case l.Type == "PLANNER_RESPONSE": // an answer that calls no tool ends the turn
			s.Waiting = len(l.ToolCalls) == 0
		}
		s.add(l.CreatedAt, l.Source == "USER_EXPLICIT", l.Content)
	})
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

// The gaps histogram's buckets. The upper edges double as the idle gaps you can pick on the
// gaps screen; the last one is a catch-all.
var (
	gapEdges = []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 15 * time.Minute,
		20 * time.Minute, 30 * time.Minute, time.Hour, 2 * time.Hour, 1 << 62}
	gapLabels = []string{"<1m", "1–2m", "2–5m", "5–10m", "10–15m", "15–20m", "20–30m", "30–60m", "1–2h", ">2h"}
)

func gapCounts(gaps []time.Duration) []int {
	counts := make([]int, len(gapEdges))
	for _, g := range gaps {
		for i, e := range gapEdges {
			if g < e {
				counts[i]++
				break
			}
		}
	}
	return counts
}

func printGaps(gaps []time.Duration) {
	if len(gaps) == 0 {
		fmt.Println("no prompts in this window")
		return
	}
	fmt.Printf("how long you took to send each prompt after the agent's last event (%d prompts)\n\n", len(gaps))
	cum := 0
	for i, c := range gapCounts(gaps) {
		cum += c
		bar := strings.Repeat("█", c*50/len(gaps))
		fmt.Printf("%7s %5d  %5.1f%%  %s\n", gapLabels[i], c, 100*float64(cum)/float64(len(gaps)), bar)
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
	n, _ := peakAt(sp)
	return n
}

// peakAt is peak plus the first moment it was reached.
func peakAt(sp []Span) (int, time.Time) {
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
	cur, hi, at := 0, 0, time.Time{}
	for _, e := range edges {
		if cur += e.d; cur > hi {
			hi, at = cur, e.t
		}
	}
	return hi, at
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
var gitInfo = readGit

// readGit finds the enclosing repo by walking up to .git and reads origin from .git/config
// directly: spawning git costs ~5 ms a call, which dominated warm runs. Worktrees and
// submodules (where .git is a file) fall back to asking git.
func readGit(dir string) (top, remote string) {
	for d := dir; ; d = filepath.Dir(d) {
		st, err := os.Stat(filepath.Join(d, ".git"))
		if err == nil && !st.IsDir() {
			if out, err := exec.Command("git", "-C", d, "remote", "get-url", "origin").Output(); err == nil {
				remote = strings.TrimSpace(string(out))
			}
			return d, remote
		}
		if err == nil {
			return d, originURL(filepath.Join(d, ".git", "config"))
		}
		if filepath.Dir(d) == d {
			return dir, ""
		}
	}
}

// originURL reads url from the [remote "origin"] section of a git config file.
func originURL(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	in := false
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			in = line == `[remote "origin"]`
			continue
		}
		if k, v, ok := strings.Cut(line, "="); in && ok && strings.TrimSpace(k) == "url" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

var gitCache sync.Map // cwd → [2]string{top, remote}; one git lookup per directory per run

func cachedGitInfo(dir string) (string, string) {
	if v, ok := gitCache.Load(dir); ok {
		info := v.([2]string)
		return info[0], info[1]
	}
	top, remote := gitInfo(dir)
	gitCache.Store(dir, [2]string{top, remote})
	return top, remote
}

type SessionLine struct {
	Agent      string    `json:"agent"`
	Name       string    `json:"name"`
	File       string    `json:"file"`
	First      time.Time `json:"first"`
	Last       time.Time `json:"last"`
	AgentHours float64   `json:"agent_hours"`
	ThinkHours float64   `json:"think_hours"`
}

// Row is one project's (or whom's, or the total's) time. You is wall-clock time you spent
// thinking; Think sums every session's wait for your next prompt, so with parallel sessions it
// can exceed You: it is how long your agents waited on you.
type Row struct {
	Project     string        `json:"project"`
	Whom        string        `json:"whom"`
	ActiveHours float64       `json:"with_agents_hours"`
	AgentHours  float64       `json:"agent_run_hours"`
	YouHours    float64       `json:"you_hours"`
	ThinkHours  float64       `json:"waited_on_you_hours"`
	Peak        int           `json:"peak_parallel"`
	PeakAt      time.Time     `json:"peak_at"`
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
	n, at := peakAt(a.agent)
	return Row{Project: project, Whom: who, ActiveHours: hours(unionLen(all)), AgentHours: hours(sum(a.agent)),
		YouHours: hours(unionLen(a.think)), ThinkHours: hours(sum(a.think)), Peak: n, PeakAt: at, Sessions: a.sessions}
}

func hours(d time.Duration) float64 { return d.Hours() }

// dur writes a duration the way people say it: 2h 05m, 45m, <1m, and – for none.
func dur(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d <= 0:
		return "–"
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Round(time.Minute).Minutes()))
	}
	d = d.Round(time.Minute)
	return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
}

// hdur is dur for hours.
func hdur(x float64) string { return dur(time.Duration(x * float64(time.Hour))) }

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

// attribute splits every session into spans and labels it. A session that moved to another
// project or whom is cut where it moved, and each part counts for its own folder. Parts with
// no time in the window are dropped.
func attribute(sessions []Session, rules []Rule, idle time.Duration, from, to time.Time) []Attributed {
	names := map[string]string{}
	for _, s := range sessions {
		names[s.File] = s.Name()
	}
	type part struct {
		from          time.Time
		cwd           string
		project, whom string
	}
	var out []Attributed
	for _, s := range sessions {
		if s.Agent == "claude-sub" && s.Name() == "(no prompt)" { // <parent>/subagents/agent-x.jsonl
			if n, ok := names[filepath.Dir(filepath.Dir(s.File))+".jsonl"]; ok {
				s.AITitle = "↳ " + n
			}
		}
		agent, think := spans(s, idle, from, to)
		if len(agent)+len(think) == 0 {
			continue
		}
		moves := append([]Move{{Cwd: s.Cwd}}, s.Moves...)
		sort.SliceStable(moves[1:], func(i, j int) bool { return moves[1+i].T.Before(moves[1+j].T) })
		var parts []part
		for _, mv := range moves {
			project, w := "(no workspace)", "unknown"
			if mv.Cwd != "" {
				top, remote := cachedGitInfo(mv.Cwd)
				project, w = tilde(top), whom(rules, mv.Cwd, remote)
			}
			if n := len(parts); n > 0 && parts[n-1].project == project && parts[n-1].whom == w {
				continue // a cd inside the same repo: nothing to split
			}
			parts = append(parts, part{mv.T, mv.Cwd, project, w})
		}
		for i, p := range parts {
			end := to
			if i+1 < len(parts) {
				end = parts[i+1].from
			}
			run, th := clip(agent, p.from, end), clip(think, p.from, end)
			if len(run)+len(th) == 0 {
				continue
			}
			ps := s
			ps.Cwd = p.cwd
			out = append(out, Attributed{ps, run, th, p.project, p.whom})
		}
	}
	return out
}

// build is the pure core: sessions in, attributed rows out.
func build(sessions []Session, rules []Rule, idle time.Duration, from, to time.Time) Report {
	return report(attribute(sessions, rules, idle, from, to), idle, from, to)
}

// clipAts cuts attributed sessions to [from, to), dropping the ones with no time left in it.
func clipAts(ats []Attributed, from, to time.Time) []Attributed {
	var out []Attributed
	for _, at := range ats {
		at.Running, at.Thinking = clip(at.Running, from, to), clip(at.Thinking, from, to)
		if len(at.Running)+len(at.Thinking) > 0 {
			out = append(out, at)
		}
	}
	return out
}

// report sums attributed sessions into rows (per project, per whom, the total), counting only
// their time inside [from, to). Attributing once and reporting each day of a month from that is
// what keeps the month view fast.
func report(ats []Attributed, idle time.Duration, from, to time.Time) Report {
	type key struct{ project, who string }
	projects, whoms := map[key]*acc{}, map[string]*acc{}
	total := &acc{}
	for _, at := range clipAts(ats, from, to) {
		k := key{at.Project, at.Whom}
		if projects[k] == nil {
			projects[k] = &acc{}
		}
		if whoms[at.Whom] == nil {
			whoms[at.Whom] = &acc{}
		}
		first, last := bounds(at) // inside the window: what these hours are made of
		line := SessionLine{Agent: at.Agent, Name: at.Name(), File: tilde(at.File), First: first, Last: last,
			AgentHours: hours(sum(at.Running)), ThinkHours: hours(sum(at.Thinking))}
		for _, a := range []*acc{projects[k], whoms[at.Whom], total} {
			a.agent = append(a.agent, at.Running...)
			a.think = append(a.think, at.Thinking...)
		}
		projects[k].sessions = append(projects[k].sessions, line)
	}
	r := Report{From: from, To: to, Idle: fmtDur(idle)}
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

// firstTime is the earliest timestamp in a file's first lines (zero if none is found).
func firstTime(path string) time.Time {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64<<10)
	for range 32 {
		line, err := r.ReadBytes('\n')
		var l struct {
			Timestamp time.Time `json:"timestamp"`
			CreatedAt time.Time `json:"created_at"`
		}
		if json.Unmarshal(line, &l) == nil {
			if !l.Timestamp.IsZero() {
				return l.Timestamp
			}
			if !l.CreatedAt.IsZero() {
				return l.CreatedAt
			}
		}
		if err != nil {
			break
		}
	}
	return time.Time{}
}

// cacheEntry remembers how far a transcript has been read and what was in it. Transcripts only
// grow, so the next run parses just the new tail. Stored: timestamps, folders, session titles
// and the first 120 characters of your first prompt. Your other prompts are never stored.
type cacheEntry struct {
	Size, Off int64
	ModTime   time.Time
	S         Session
}

const cacheVersion = "v4" // bump when parsing rules change, so old entries are re-read

func cachePath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "renpet", "sessions-"+cacheVersion+".gob")
}

// The cache stays in memory between loads, so Live can poll every 2 seconds without reading
// and rewriting the file each time; the file is written at most once a minute.
var (
	cacheMu      sync.Mutex
	memCache     map[string]cacheEntry
	cacheWritten time.Time
)

func readCache(path string) map[string]cacheEntry {
	c := map[string]cacheEntry{}
	if f, err := os.Open(path); err == nil {
		defer f.Close()
		if gob.NewDecoder(f).Decode(&c) != nil {
			return map[string]cacheEntry{} // unreadable cache: start over, never fail
		}
	}
	return c
}

func writeCache(path string, c map[string]cacheEntry) {
	if path == "" || os.MkdirAll(filepath.Dir(path), 0o755) != nil {
		return
	}
	f, err := os.CreateTemp(filepath.Dir(path), "sessions-*.tmp") // unique: two runs can't clash
	if err != nil {
		return
	}
	if gob.NewEncoder(f).Encode(c) == nil && f.Close() == nil {
		_ = os.Rename(f.Name(), path) // atomic: a crash never leaves half a cache
	} else {
		f.Close()
		os.Remove(f.Name())
	}
}

// parseFrom continues one transcript from off, appending to s.
func parseFrom(kind, path string, off int64, s *Session) (int64, error) {
	switch kind {
	case "claude", "claude-sub":
		return parseClaude(path, off, s, kind == "claude-sub")
	case "codex":
		return parseCodex(path, off, s)
	default:
		return parseAntigravity(path, off, s)
	}
}

// load returns every session that can hold events in [from, to). Unchanged transcripts come
// from the cache, grown ones are parsed from where the last run stopped, new ones from the start,
// all CPUs at once.
func load(from, to time.Time) []Session {
	home, _ := os.UserHomeDir()
	workspaces := antigravityWorkspaces(home)
	files := discover(from)
	cp := cachePath()
	cacheMu.Lock()
	old := memCache
	cacheMu.Unlock()
	if old == nil {
		old = readCache(cp)
	}
	next := make(map[string]cacheEntry, len(old))
	for k, v := range old {
		next[k] = v
	}
	paths := make(chan string)
	var mu sync.Mutex
	var wg sync.WaitGroup
	var out []Session
	for range runtime.NumCPU() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range paths {
				st, err := os.Stat(p)
				if err != nil {
					continue
				}
				kind := files[p]
				e, ok := old[p]
				switch {
				case ok && e.Size == st.Size() && e.ModTime.Equal(st.ModTime()):
					// unchanged
				case ok && st.Size() >= e.Off:
					e.S.Events = append([]Event(nil), e.S.Events...) // don't share the old slice
					e.Off, err = parseFrom(kind, p, e.Off, &e.S)
				default:
					// A transcript that starts more than a day after the window can't reach into it.
					// ponytail: the day of slack covers resumed sessions' out-of-order lines.
					if t := firstTime(p); !t.IsZero() && t.After(to.Add(24*time.Hour)) {
						continue
					}
					e = cacheEntry{S: Session{Agent: kind, File: p}}
					e.Off, err = parseFrom(kind, p, 0, &e.S)
				}
				e.Size, e.ModTime = st.Size(), st.ModTime()
				s := e.S
				s.Events = slices.Clone(s.Events) // callers sort it in place; the cache keeps its own
				// the antigravity workspace lives outside the transcript and can change
				if kind == "antigravity" {
					s.Cwd = workspaces[filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(p))))]
				}
				mu.Lock()
				if err != nil {
					fmt.Fprintln(os.Stderr, "skip:", p, err)
				} else {
					next[p] = e
					if len(s.Events) > 0 {
						out = append(out, s)
					}
				}
				mu.Unlock()
			}
		}()
	}
	for p := range files {
		paths <- p
	}
	close(paths)
	wg.Wait()
	cacheMu.Lock()
	memCache = next
	write := time.Since(cacheWritten) > time.Minute
	if write {
		cacheWritten = time.Now()
	}
	cacheMu.Unlock()
	if write {
		writeCache(cp, next)
	}
	return out
}

// printWeek draws the report as a table. Colour shows on a terminal and is stripped for pipes
// and NO_COLOR (lipgloss.Fprint downsamples to whatever w can show).
func printWeek(w io.Writer, r Report, all bool, rules []Rule) {
	t := table.New().Border(lipgloss.RoundedBorder()).BorderStyle(dim).
		Headers("project", "for whom", "with agents", "agents ran", "you", "peak ∥", "sessions").
		StyleFunc(func(row, col int) lipgloss.Style {
			s := lipgloss.NewStyle().Padding(0, 1)
			if row == table.HeaderRow {
				return s.Faint(true)
			}
			if col >= 2 {
				return s.Align(lipgloss.Right)
			}
			return s
		})
	var whoms []string
	var total Row
	hidden := 0
	for _, row := range r.Rows {
		switch {
		case row.Project == "*" && row.Whom == "TOTAL":
			total = row
		case row.Project == "*":
			whoms = append(whoms, paint(whomColor(rules, row.Whom), title(row.Whom))+" "+bold.Render(hdur(row.ActiveHours)))
		case !all && row.ActiveHours < 0.1:
			hidden++ // still counted in the per-whom and TOTAL rows
		default:
			t.Row(row.Project, paint(whomColor(rules, row.Whom), row.Whom), hdur(row.ActiveHours), hdur(row.AgentHours),
				hdur(row.YouHours), strconv.Itoa(row.Peak), strconv.Itoa(len(row.Sessions)))
		}
	}
	lipgloss.Fprintf(w, "\n%s%s\n%s\n", bold.Render(r.From.Format("Mon Jan 2 15:04")+" → "+r.To.Format("Mon Jan 2 15:04")),
		dim.Render("  idle gap "+r.Idle), t.Render())
	if hidden > 0 {
		lipgloss.Fprintln(w, dim.Render(fmt.Sprintf("%d projects under 6 min hidden (still in the totals; --all shows them)", hidden)))
	}
	lipgloss.Fprintf(w, "\n%s\n%s %s with agents · %s agents ran · %s you · agents waited on you %s · peak %s\n\n", strings.Join(whoms, " · "),
		bold.Render("Total"), bold.Render(hdur(total.ActiveHours)), bold.Render(hdur(total.AgentHours)), bold.Render(hdur(total.YouHours)),
		bold.Render(hdur(total.ThinkHours)), bold.Render(strconv.Itoa(total.Peak)))
	for _, l := range []string{"Per-whom hours can add up to more than the total: agents for different whoms ran at once.",
		"with agents = wall-clock with any agent session active · agents ran = agent working time, summed per session",
		"you = wall-clock in gaps your prompt ended · waited on you = those gaps summed per session · peak ∥ = most agents at once"} {
		lipgloss.Fprintln(w, dim.Render(l))
	}
}

// Cell is one slice of a day (15 min, an hour…): how many agents were working at once, how long
// any agent was working (wall-clock), how long you spent thinking, how much of the cell anyone
// was active, and who the cell's time mostly went to ("" when nobody worked).
type Cell struct {
	Peak               int
	Run, Think, Active time.Duration
	Whom               string
}

// laneCells splits a local day into cells of the given size (15m → 96 cells, 30m → 48).
func laneCells(ats []Attributed, day time.Time, cell time.Duration) []Cell {
	out := make([]Cell, int(24*time.Hour/cell))
	for i := range out {
		start := day.Add(time.Duration(i) * cell)
		end := start.Add(cell)
		var running, thinking []Span
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
					thinking = append(thinking, c)
					per[at.Whom] += c.Len()
				}
			}
		}
		out[i].Peak = peak(running)
		out[i].Run = unionLen(running)
		out[i].Think = unionLen(thinking)
		out[i].Active = unionLen(append(running, thinking...))
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
	fmt.Println("agents: most working at once · you: ▄ thinking, █ ≥5 min · whom: first letter of who it was for, ? unknown")
	fmt.Println()
}

func printWhy(r Report, needle string, rules []Rule) {
	needle = strings.ToLower(needle)
	found := false
	for _, row := range r.Rows {
		if row.Project == "*" || !strings.Contains(strings.ToLower(row.Project), needle) {
			continue
		}
		found = true
		lipgloss.Printf("\n%s  %s\n%s with agents · %s agents ran · %s you %s\n", bold.Render(row.Project),
			paint(whomColor(rules, row.Whom), title(row.Whom)), bold.Render(hdur(row.ActiveHours)), bold.Render(hdur(row.AgentHours)),
			bold.Render(hdur(row.YouHours)), dim.Render(fmt.Sprintf("· peak %d", row.Peak)))
		sort.Slice(row.Sessions, func(i, j int) bool { return row.Sessions[i].First.Before(row.Sessions[j].First) })
		t := table.New().Border(lipgloss.RoundedBorder()).BorderStyle(dim).BorderRow(true).
			Headers("when", "agent", "agents ran", "you", "what").
			StyleFunc(func(row, col int) lipgloss.Style {
				s := lipgloss.NewStyle().Padding(0, 1)
				switch {
				case row == table.HeaderRow:
					return s.Faint(true)
				case col == 2 || col == 3:
					return s.Align(lipgloss.Right)
				}
				return s
			})
		for _, s := range row.Sessions {
			t.Row(when(s.First, s.Last), s.Agent, hdur(s.AgentHours), hdur(s.ThinkHours),
				ansi.Truncate(s.Name, 60, "…")+"\n"+dim.Render(short(s.File, 60)))
		}
		lipgloss.Println(t.Render())
	}
	if !found {
		fmt.Println("no project matches", needle)
	}
}

// when shows a time range compactly: the end drops the date when it is the same day.
func when(a, b time.Time) string {
	a, b = a.Local(), b.Local()
	if startOfDay(a).Equal(startOfDay(b)) {
		return a.Format("Mon Jan 2 15:04") + "–" + b.Format("15:04")
	}
	return a.Format("Mon Jan 2 15:04") + "–" + b.Format("Mon 15:04")
}

// animate is true only on an interactive terminal that hasn't opted out of colour, so pipes,
// CI and NO_COLOR get one static frame, as in termheat.
func animate() bool {
	st, err := os.Stdout.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0 && os.Getenv("NO_COLOR") == ""
}

// runConfig shows where settings live and what is in effect; `init` writes a starter file.
func runConfig(path string, cfg Config, args []string) {
	if len(args) > 0 && args[0] == "init" {
		if _, err := os.Stat(path); err == nil {
			fmt.Println("already exists:", path)
			return
		}
		b, _ := json.MarshalIndent(cfg, "", "  ")
		err := os.MkdirAll(filepath.Dir(path), 0o755)
		if err == nil {
			err = os.WriteFile(path, append(b, '\n'), 0o644)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "config init:", err)
			os.Exit(1)
		}
		fmt.Println("wrote", path)
		return
	}
	_, err := os.Stat(path)
	state := "in use"
	if err != nil {
		state = "not created yet: `renpet config init` writes one"
	}
	fmt.Printf("%s (%s)\n\n", path, state)
	b, _ := json.MarshalIndent(cfg, "", "  ")
	fmt.Println(string(b))
}

func main() {
	if len(os.Args) == 1 {
		os.Args = append(os.Args, "day") // bare `renpet` opens today
	}
	cfgPath := configPath()
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}
	if os.Args[1] == "config" {
		runConfig(cfgPath, cfg, os.Args[2:])
		return
	}
	if !slices.Contains([]string{"week", "why", "day", "gaps", "now", "card"}, os.Args[1]) {
		fmt.Fprintln(os.Stderr, `usage: renpet                          this week, animated
       renpet day [YYYY-MM-DD]          the week holding that day ([--static] for plain text)
       renpet week [--since D] [--until D] [--json] [--all]
       renpet why <project> [week flags]
       renpet now [--short] [--json]    agents working and waiting on you right now
       renpet card [week flags] [--hide-names]   the range as a card to screenshot
       renpet gaps [--since D]          helps pick the idle gap
       renpet config [init]             where settings live; init writes a starter file
common flags: --idle 15m --rules file.json --week-start monday`)
		os.Exit(2)
	}
	cmd := os.Args[1]
	fl := flag.NewFlagSet(cmd, flag.ExitOnError)
	now := time.Now()
	y, m, d := now.AddDate(0, 0, -7).Date()
	since := fl.String("since", time.Date(y, m, d, 0, 0, 0, 0, time.Local).Format("2006-01-02"), "start date (local)")
	until := fl.String("until", "", "end date, exclusive (local); default now")
	cfgIdle, _ := time.ParseDuration(cfg.Idle)
	idle := fl.Duration("idle", cfgIdle, "a gap longer than this between events is idle, not work")
	rulesPath := fl.String("rules", "", "rules JSON file; default: rules in "+cfgPath)
	weekFlag := fl.String("week-start", cfg.WeekStart, "first day of the week")
	asJSON := fl.Bool("json", false, "print JSON")
	all := fl.Bool("all", false, "show projects under 6 minutes too")
	static := fl.Bool("static", false, "day: print plain text instead of the animated view")
	short := fl.Bool("short", false, "now: one line for tmux or a shell prompt")
	hide := fl.Bool("hide-names", false, "card: blank out who and which projects")
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
	ws, ok := weekdays[strings.ToLower(*weekFlag)]
	if !ok {
		fmt.Fprintln(os.Stderr, "bad --week-start:", *weekFlag)
		os.Exit(2)
	}
	weekStart = ws

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
	rules := cfg.Rules
	if *rulesPath != "" {
		raw, err := os.ReadFile(*rulesPath)
		if err == nil {
			err = json.Unmarshal(raw, &rules)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "bad rules:", err)
			os.Exit(1)
		}
	}

	if cmd == "day" && !*asJSON && !*static && animate() {
		if err := runTUI(cfg, cfgPath, rules, *idle, from); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if cmd == "now" {
		printNow(os.Stdout, readLive(rules, *idle), *short, *asJSON)
		return
	}
	sessions := load(from, to)
	if cmd == "card" {
		c := cardFrom(rangeFrom(sessions, customP, from, to, rules, *idle, now), from.Format("Jan 2")+" – "+to.Add(-time.Nanosecond).Format("Jan 2"), rules, *hide)
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(c)
		} else {
			lipgloss.Println(c.render())
		}
		return
	}
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
		printWhy(r, needle, rules)
	default:
		printWeek(os.Stdout, r, *all, rules)
	}
}
