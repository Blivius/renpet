package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// Settings and the gaps screen. Every change is saved to config.json at once and the week is
// re-read with it, so you see what a rule or an idle gap does to your numbers as you set it.

// saveConfig writes the config atomically. A symlinked config (dotfiles) is written through.
func writeJSON(path string, c any) error {
	if p, err := filepath.EvalSymlinks(path); err == nil {
		path = p
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "config-*.tmp")
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		os.Remove(f.Name())
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return err
	}
	return os.Rename(f.Name(), path)
}

// apply saves c, switches to it and re-reads the range. Ranges already read are dropped: they
// were counted with the old settings. The old numbers stay on screen until the new ones land.
func (m model) apply(c Config) (tea.Model, tea.Cmd) {
	if err := writeJSON(m.cfgPath, c); err != nil {
		return m.flash("not saved: " + err.Error()), nil
	}
	m = m.flash("saved to " + tilde(m.cfgPath))
	m.cfg, m.rules = c, c.Rules
	m.idle, _ = time.ParseDuration(c.Idle)
	weekStart = weekdays[strings.ToLower(c.WeekStart)]
	m.gen++
	m.rec, m.recBusy = nil, false // records were counted with the old settings too
	m.past = map[[2]time.Time]*rangeData{}
	day, old := m.selDay(), m.d
	from, to := m.from, m.to
	if m.per != customP {
		from, to = periodSpan(m.per, day)
	}
	m, _ = m.setRange(from, to, day)
	if old != nil && old.from.Equal(from) && old.to.Equal(to) {
		m.d = old
	}
	return m, m.load()
}

// fmtDur writes a duration the way config.json and people do: 15m, 2h.
func fmtDur(d time.Duration) string {
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dm", int(d.Round(time.Minute).Minutes()))
}

// snapIdle is the first bucket edge at or above d: where the gaps screen's marker starts.
func snapIdle(d time.Duration) time.Duration {
	for _, e := range gapEdges[:len(gapEdges)-1] {
		if e >= d {
			return e
		}
	}
	return gapEdges[len(gapEdges)-2]
}

func (m model) gapsKey(k string) (tea.Model, tea.Cmd) {
	i := slices.Index(gapEdges, m.gapIdle)
	switch k {
	case "esc":
		m.scr = m.ret
	case "left", "h", "up", "k":
		m.gapIdle = gapEdges[max(0, i-1)]
	case "right", "l", "down", "j":
		m.gapIdle = gapEdges[min(len(gapEdges)-2, i+1)]
	case "enter":
		c := m.cfg
		c.Idle = fmtDur(m.gapIdle)
		mm, cmd := m.apply(c)
		next := mm.(model)
		next.scr = next.ret
		return next, cmd
	}
	return m, nil
}

func (m model) viewGaps() string {
	g := m.d.gaps
	var b strings.Builder
	b.WriteString(m.head("Prompt gaps", fmt.Sprintf("%s · %d prompts · idle now %s", m.rangeLabel(), len(g), fmtDur(m.idle)), ""))
	b.WriteString(dim.Render("   How long you took to send each prompt after the agent's last event.") + "\n\n")
	if len(g) == 0 {
		return b.String() + dim.Render("   no prompts in this "+periodNames[m.per]) + "\n" + m.keys("[esc] back")
	}
	counts := gapCounts(g)
	most := slices.Max(counts)
	cum, thinking := 0, 0
	for i, c := range counts {
		cum += c
		hex := ramp[4] // you thinking
		if gapEdges[i] > m.gapIdle {
			hex = unknownHex // you away
		}
		fmt.Fprintf(&b, "   %7s %5d %6.1f%%  %s\n", gapLabels[i], c, 100*float64(cum)/float64(len(g)),
			paint(hex, bar(float64(c)/float64(most), max(10, m.wide()-30))))
		if gapEdges[i] == m.gapIdle {
			thinking = cum
			fmt.Fprintf(&b, "   %s\n", paint(voiceHex, "──── idle "+fmtDur(m.gapIdle)+" ────"))
		}
	}
	fmt.Fprintf(&b, "\n   With %s: %s of prompts count as thinking, %s as coming back.\n",
		bold.Render(fmtDur(m.gapIdle)), bold.Render(fmt.Sprintf("%.0f%%", 100*float64(thinking)/float64(len(g)))),
		bold.Render(fmt.Sprintf("%.0f%%", 100*float64(len(g)-thinking)/float64(len(g)))))
	b.WriteString(dim.Render("   Put it where the bars thin out: too low misses review, too high counts lunch") + "\n")
	return b.String() + m.keys("[←/→] move the line", "[⏎] save as idle gap", "[esc] back")
}

// unassigned are this week's folders no rule matches: the ones the settings screen asks about.
func (m model) unassigned() []Row {
	var out []Row
	for _, r := range m.d.total.Rows {
		if r.Whom == "unknown" && r.Project != "*" && r.Project != "(no workspace)" {
			out = append(out, r)
		}
	}
	return out
}

// whoms are the names your rules use, in order: what ←/→ offers when assigning a folder.
func (m model) whoms() []string {
	var out []string
	for _, r := range m.cfg.Rules {
		if !slices.Contains(out, r.Whom) {
			out = append(out, r.Whom)
		}
	}
	return out
}

func (m model) settingsKey(k string) (tea.Model, tea.Cmd) {
	c := m.cfg
	c.Rules = slices.Clone(c.Rules)
	un := m.unassigned()
	n := 2 + len(c.Rules) + len(un)
	cur := max(0, min(m.cur[settingsScr], n-1))
	ri, ui := cur-2, cur-2-len(c.Rules) // rule and unassigned-folder index under the cursor
	isRule := ri >= 0 && ri < len(c.Rules)
	step := 1
	switch k {
	case "esc":
		return m.back(), nil
	case "up", "k":
		cur--
	case "down", "j":
		cur++
	case "left", "h":
		step = -1
		fallthrough
	case "right", "l":
		switch cur {
		case 0:
			c.WeekStart = strings.ToLower(time.Weekday((int(weekdays[strings.ToLower(c.WeekStart)]) + step + 7) % 7).String())
			return m.apply(c)
		case 1:
			d, _ := time.ParseDuration(c.Idle)
			c.Idle = fmtDur(max(time.Minute, d.Round(time.Minute)+time.Duration(step)*time.Minute))
			return m.apply(c)
		}
	case "x", "delete":
		if isRule {
			c.Rules = slices.Delete(c.Rules, ri, ri+1)
			return m.apply(c)
		}
	case "K", "shift+up":
		if isRule && ri > 0 {
			c.Rules[ri], c.Rules[ri-1] = c.Rules[ri-1], c.Rules[ri]
			m.cur[settingsScr] = cur - 1
			return m.apply(c)
		}
	case "J", "shift+down":
		if isRule && ri < len(c.Rules)-1 {
			c.Rules[ri], c.Rules[ri+1] = c.Rules[ri+1], c.Rules[ri]
			m.cur[settingsScr] = cur + 1
			return m.apply(c)
		}
	case "enter":
		if ui >= 0 && ui < len(un) {
			m.assigning, m.input, m.note = true, "", ""
		}
	case "g":
		m.scr, m.ret, m.gapIdle = gapsScr, settingsScr, snapIdle(m.idle)
	}
	m.cur[settingsScr] = max(0, min(cur, n-1))
	return m, nil
}

// typeWhom edits who the folder under the cursor is for. ←/→ steps through the names your rules
// already use; typing makes a new one. Enter adds a path rule at the end: no earlier rule
// matched this folder, so the end is where it changes nothing else.
func (m model) typeWhom(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	whoms := m.whoms()
	switch k := msg.String(); k {
	case "esc":
		m.assigning = false
	case "enter":
		un := m.unassigned()
		ui := m.cur[settingsScr] - 2 - len(m.cfg.Rules)
		whom := strings.ToLower(strings.TrimSpace(m.input))
		if whom == "" || ui < 0 || ui >= len(un) {
			return m, nil
		}
		c := m.cfg
		c.Rules = append(slices.Clone(c.Rules), Rule{Path: un[ui].Project, Whom: whom})
		m.assigning = false
		return m.apply(c)
	case "left", "right":
		if len(whoms) > 0 {
			i := slices.Index(whoms, m.input)
			if k == "left" {
				i = (i - 1 + len(whoms)) % len(whoms) // from -1 (typed) to the last
			} else {
				i = (i + 1) % len(whoms)
			}
			m.input = whoms[i]
		}
	case "backspace":
		if r := []rune(m.input); len(r) > 0 {
			m.input = string(r[:len(r)-1])
		}
	default:
		m.input += msg.Text
	}
	return m, nil
}

// settingsItems is the item on each line viewSettings draws (-1 for blanks and headings), so a
// click can find what it hit. It follows viewSettings' layout line for line.
func (m model) settingsItems() []int {
	ids := []int{0, 1, -1, -1}
	for i := range m.cfg.Rules {
		ids = append(ids, 2+i)
	}
	ids = append(ids, -1, -1)
	un := m.unassigned()
	for i := range un {
		ids = append(ids, 2+len(m.cfg.Rules)+i)
	}
	if len(un) == 0 {
		ids = append(ids, -1)
	}
	return ids
}

func (m model) viewSettings() string {
	w := m.wide()
	var b strings.Builder
	b.WriteString(m.head("Settings", tilde(m.cfgPath), ""))
	un := m.unassigned()
	n := 2 + len(m.cfg.Rules) + len(un)
	cur := max(0, min(m.cur[settingsScr], n-1))
	var lines []string
	at := 0 // the line the cursor is on
	item := func(i int, s string) {
		mark := "   "
		if i == cur {
			mark, at = paint(accent, " › "), len(lines)
			s = bold.Render(s)
		}
		lines = append(lines, mark+s)
	}
	item(0, fmt.Sprintf("%-16s ‹ %s ›", "Week starts on", title(m.cfg.WeekStart)))
	item(1, fmt.Sprintf("%-16s ‹ %s ›", "Idle gap", m.cfg.Idle)+dim.Render("   g shows your prompt gaps"))
	lines = append(lines, "", "   "+bold.Render("Rules")+dim.Render("  who each folder's time is for · first match wins"))
	for i, r := range m.cfg.Rules {
		kind, val := "path", r.Path
		if r.Remote != "" {
			kind, val = "remote", r.Remote
		}
		item(2+i, fmt.Sprintf("%2d  %s %s %s", i+1, dim.Render(fit(kind, 6)), fit(val, max(10, w-30)), paint(whomColor(m.cfg.Rules, r.Whom), "→ "+r.Whom)))
	}
	lines = append(lines, "", "   "+bold.Render("Unassigned this week")+dim.Render("  folders no rule matches"))
	for i, r := range un {
		s := fitLeft(r.Project, max(10, w-30)) + " " + rjust(hdur(r.ActiveHours), 7)
		if m.assigning && 2+len(m.cfg.Rules)+i == cur {
			s += "  → " + paint(whomColor(m.cfg.Rules, m.input), m.input) + paint(accent, "▏")
		}
		item(2+len(m.cfg.Rules)+i, s)
	}
	if len(un) == 0 {
		lines = append(lines, dim.Render("   none: every folder this week has an owner 🌱"))
	}
	lo, hi := visible(len(lines), at, m.room(&b, 2))
	b.WriteString(strings.Join(lines[lo:hi], "\n") + "\n")
	switch {
	case m.assigning:
		return b.String() + m.keys("type a name or [←/→] pick one", "[⏎] save", "[esc] cancel")
	case cur >= 2 && cur < 2+len(m.cfg.Rules):
		return b.String() + m.keys("[x] delete", "[K/J] move rule", "[g] gaps", "[esc] back")
	case cur >= 2:
		return b.String() + m.keys("[⏎] assign to a whom", "[g] gaps", "[esc] back")
	}
	return b.String() + m.keys("[←/→] change", "[g] gaps", "[esc] back")
}
