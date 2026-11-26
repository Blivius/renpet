package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// Live: what your agents are doing right now, from the logs, polled every 2 seconds while the
// view is open. A session is live while its last event is younger than the idle gap, the same
// rule that decides when time stops counting. It waits on you once its agent ended its turn;
// otherwise it is working. `renpet now` prints the same thing once.

const livePoll = 2 * time.Second

type liveData struct {
	at    time.Time
	win   time.Duration        // how far back each lane reaches
	rows  []sessRow            // waiting on you first, longest wait first; then working, longest run first
	since map[string]time.Time // per file: when the wait or the run began
	today Row
}

type liveMsg struct {
	d   liveData
	gen int
}

// liveFrom finds the live sessions among sessions (which should cover today).
func liveFrom(sessions []Session, rules []Rule, idle time.Duration, now time.Time) liveData {
	day := startOfDay(now)
	d := liveData{at: now, win: max(30*time.Minute, idle), since: map[string]time.Time{}}
	d.today = total(build(sessions, rules, idle, day, now)) // also sorts every session's events
	byFile, live := map[string]Session{}, map[string]Session{}
	for _, s := range sessions {
		byFile[s.File] = s
	}
	for _, s := range sessions {
		if len(s.Events) == 0 || now.Sub(s.Events[len(s.Events)-1].T) > idle || (s.Agent == "claude-sub" && s.Waiting) {
			continue // quiet too long, or a subagent that finished
		}
		live[s.File] = s
		if s.Agent == "claude-sub" { // a subagent at work keeps its parent live, however long since the parent wrote
			if p, ok := byFile[filepath.Dir(filepath.Dir(s.File))+".jsonl"]; ok {
				live[p.File] = p
			}
		}
	}
	var in []Session
	for _, s := range live {
		in = append(in, s)
	}
	// A session that moved repos is several parts; Live shows the part it is in now.
	latest := map[string]int{}
	var keep []Attributed
	for _, at := range attribute(in, rules, idle, now.Add(-d.win), now) {
		_, end := bounds(at)
		if i, ok := latest[at.File]; ok {
			if _, e := bounds(keep[i]); end.After(e) {
				keep[i] = at
			}
			continue
		}
		latest[at.File] = len(keep)
		keep = append(keep, at)
	}
	rows, _ := fold(keep, false)
	for _, r := range rows {
		if r.Waiting && r.Whom == "automation" { // a scripted run that finished waits on nobody
			continue
		}
		d.rows = append(d.rows, r)
		d.since[r.File] = since(r.Session, idle)
	}
	sort.SliceStable(d.rows, func(i, j int) bool {
		a, b := d.rows[i], d.rows[j]
		if a.Waiting != b.Waiting {
			return a.Waiting
		}
		return d.since[a.File].Before(d.since[b.File])
	})
	return d
}

// since is when a session's state began: the end of its turn when it waits on you, else the
// prompt (or first event) that started the run still going. Events must be sorted.
func since(s Session, idle time.Duration) time.Time {
	ev := s.Events
	i := len(ev) - 1
	if s.Waiting {
		return ev[i].T
	}
	for i > 0 && !ev[i].Human && ev[i].T.Sub(ev[i-1].T) <= idle {
		i--
	}
	return ev[i].T
}

// readLive reads what can be live now: today, and anything younger than the idle gap.
func readLive(rules []Rule, idle time.Duration) liveData {
	now := time.Now()
	return liveFrom(load(minT(startOfDay(now), now.Add(-idle)), now), rules, idle, now)
}

func (m model) loadLive() tea.Cmd {
	rules, idle, gen := m.rules, m.idle, m.gen
	return func() tea.Msg { return liveMsg{readLive(rules, idle), gen} }
}

func (d liveData) counts() (working, waiting int) {
	for _, r := range d.rows {
		if r.Waiting {
			waiting++
		} else {
			working++
		}
	}
	return
}

// state is a row's word and how long it has been in it: "waiting 4m", "working 12m".
func (d liveData) state(r sessRow) string {
	w := "working "
	if r.Waiting {
		w = "waiting "
	}
	return w + dur(d.at.Sub(d.since[r.File]))
}

func (m model) viewLive() string {
	var b strings.Builder
	if m.live == nil {
		return m.head("Live", "", "") + paint(accent, "reading the logs…") + "\n"
	}
	d := *m.live
	working, waiting := d.counts()
	right := paint(accent, fmt.Sprintf("● %d working", working)) + "  " + paint(voiceHex, fmt.Sprintf("◐ %d waiting on you", waiting))
	b.WriteString(m.head("Live", "updated "+d.at.Format("15:04:05"), right))
	fmt.Fprintf(&b, "%s %s with agents · agents %s · you %s\n\n", dim.Render("today so far"), bold.Render(hdur(d.today.ActiveHours)),
		bold.Render(hdur(d.today.AgentHours)), bold.Render(hdur(d.today.YouHours)))
	keys := m.keys("[↑/↓] session", "[⏎] prompts", "[c] card")
	if len(d.rows) == 0 {
		b.WriteString(dim.Render("   no agent is working or waiting on you right now") + "\n")
		b.WriteString(dim.Render(fit(fmt.Sprintf("   a session shows here until its log has been quiet for %s (your idle gap)", fmtDur(m.idle)), m.wide())) + "\n")
		return b.String() + keys
	}
	labelW := max(26, min(40, m.wide()/3))
	n := m.wide() - 3 - 2 - 12 - labelW - 2
	t0 := d.at.Add(-d.win)
	step := d.win / time.Duration(n)
	ruler := []rune(strings.Repeat(" ", n))
	for k := 0; k*10 < int(d.win.Minutes()); k++ { // every 10 minutes, counted back from now
		mins := int(d.win.Minutes()) - k*10
		label := []rune(fmt.Sprintf("-%dm", mins))
		if col := int(time.Duration(k*10) * time.Minute / step); col+len(label) < n-3 {
			copy(ruler[col:], label)
		}
	}
	copy(ruler[n-3:], []rune("now"))
	fmt.Fprintf(&b, "   %s %s\n", strings.Repeat(" ", 2+12+labelW), dim.Render(string(ruler)))

	cur := min(m.cur[liveScr], len(d.rows)-1)
	pulse := 0.75 + 0.25*math.Sin(2*math.Pi*m.now.Sub(time.Time{}).Seconds()/breathe.Seconds())
	lo, hi := visible(len(d.rows), cur, m.room(&b, 5))
	for i := lo; i < hi; i++ {
		r := d.rows[i]
		glyph, state := paint(scaleHex(accent, pulse), "●"), d.state(r)
		if r.Waiting {
			glyph = paint(voiceHex, "◐")
		}
		proj := filepath.Base(r.Project)
		label := dim.Render(fit(proj, 10)) + " " + fit(r.Name(), labelW-11)
		mark := "   "
		if i == cur {
			mark, label = paint(accent, " › "), bold.Render(fit(proj, 10)+" "+fit(r.Name(), labelW-11))
		}
		b.WriteString(mark + glyph + " " + fit(state, 12) + label + "  " + m.lane(r, t0, step, n) + "\n")
	}
	s := d.rows[cur]
	info := fmt.Sprintf("%s · %s", s.Agent, s.Project)
	if s.subs > 0 {
		info += fmt.Sprintf(" · +%d subagents", s.subs)
	}
	fmt.Fprintf(&b, "\n   %s %s\n", paint(whomColor(m.rules, s.Whom), title(s.Whom)), dim.Render(fit(info, m.wide()-5-len(title(s.Whom)))))
	b.WriteString(dim.Render(fit("   ◐ its agent ended its turn · read from the logs every 2s, nothing sent anywhere", m.wide())) + "\n")
	return b.String() + keys
}

// The `renpet now` command: the Live view once. --short is one line for tmux or a shell
// prompt, --json everything.

type nowSession struct {
	State     string    `json:"state"` // working or waiting
	Since     time.Time `json:"since"`
	Agent     string    `json:"agent"`
	Name      string    `json:"name"`
	Project   string    `json:"project"`
	Whom      string    `json:"whom"`
	Subagents int       `json:"subagents"`
	File      string    `json:"file"`
}

func printNow(w io.Writer, d liveData, short, asJSON bool) {
	working, waiting := d.counts()
	switch {
	case asJSON:
		out := struct {
			At       time.Time    `json:"at"`
			Working  int          `json:"working"`
			Waiting  int          `json:"waiting_on_you"`
			Sessions []nowSession `json:"sessions"`
			Today    Row          `json:"today"`
		}{d.at, working, waiting, []nowSession{}, d.today}
		for _, r := range d.rows {
			st := map[bool]string{false: "working", true: "waiting"}[r.Waiting]
			out.Sessions = append(out.Sessions, nowSession{st, d.since[r.File], r.Agent, r.Name(), r.Project, r.Whom, r.subs, tilde(r.File)})
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
	case short:
		fmt.Fprintln(w, shortLine(working, waiting, d.today.ActiveHours))
	default:
		for _, r := range d.rows {
			glyph := lipgloss.NewStyle().Foreground(lipgloss.Color(accent)).Render("●")
			if r.Waiting {
				glyph = lipgloss.NewStyle().Foreground(lipgloss.Color(voiceHex)).Render("◐")
			}
			extra := ""
			if r.subs > 0 {
				extra = fmt.Sprintf("  +%d subagents", r.subs)
			}
			lipgloss.Fprintln(w, glyph+" "+fit(d.state(r), 13)+fit(filepath.Base(r.Project), 14)+" "+fit(r.Name(), 40)+" "+r.Agent+extra)
		}
		if len(d.rows) == 0 {
			fmt.Fprintln(w, "no agent is working or waiting on you right now")
		}
		fmt.Fprintf(w, "today: %s with agents · agents %s · you %s\n", hdur(d.today.ActiveHours), hdur(d.today.AgentHours), hdur(d.today.YouHours))
	}
}

// shortLine is `renpet now --short`: "●2 ◐1 · 4h 12m" (working, waiting on you, today with
// agents). Plain text, so tmux and shell prompts show it as is.
func shortLine(working, waiting int, todayHours float64) string {
	var parts []string
	if working > 0 {
		parts = append(parts, fmt.Sprintf("●%d", working))
	}
	if waiting > 0 {
		parts = append(parts, fmt.Sprintf("◐%d", waiting))
	}
	return strings.TrimSpace(strings.Join(parts, " ") + " · " + hdur(todayHours))
}
