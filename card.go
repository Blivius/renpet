package main

import (
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
)

// The card: a range on one box made to be screenshotted. Your hours against your agents', who
// the time was for, the top projects, and the line that says the most. With hide, every name is
// blanked out (the colours stay, so the split still shows).

const cardW = 58 // inside the border and padding

type cardData struct {
	Range       string      `json:"range"`
	From        time.Time   `json:"from"`
	To          time.Time   `json:"to"`
	YouHours    float64     `json:"you_hours"`
	AgentHours  float64     `json:"agent_run_hours"`
	ActiveHours float64     `json:"with_agents_hours"`
	WaitedHours float64     `json:"waited_on_you_hours"`
	Peak        int         `json:"peak_parallel"`
	For         []cardShare `json:"for"`
	Top         []cardShare `json:"top_projects"`
	Line        string      `json:"line,omitempty"`
}

type cardShare struct {
	Name  string  `json:"name,omitempty"` // empty when names are hidden
	Hours float64 `json:"hours"`
	color string
}

func cardFrom(d rangeData, label string, rules []Rule, hide bool) cardData {
	tot := total(d.total)
	c := cardData{Range: label, From: d.from, To: d.to, YouHours: tot.YouHours, AgentHours: tot.AgentHours,
		ActiveHours: tot.ActiveHours, WaitedHours: tot.ThinkHours, Peak: tot.Peak, Line: voice(d)}
	var names []string
	for _, row := range d.total.Rows {
		switch {
		case row.Whom == "TOTAL" || row.ActiveHours < 0.05:
		case row.Project == "*" && row.Whom != "automation":
			c.For = append(c.For, cardShare{title(row.Whom), row.ActiveHours, whomColor(rules, row.Whom)})
			names = append(names, title(row.Whom))
		case row.Project != "*" && row.Whom != "automation" && row.Project != "(no workspace)" && len(c.Top) < 3:
			c.Top = append(c.Top, cardShare{strings.TrimRight(fit(filepath.Base(row.Project), 14), " "), row.ActiveHours, whomColor(rules, row.Whom)})
			names = append(names, filepath.Base(row.Project))
		}
	}
	if hide {
		for i := range c.For {
			c.For[i].Name = ""
		}
		for i := range c.Top {
			c.Top[i].Name = ""
		}
		for _, n := range names { // the line can name who or what: then it goes too
			if strings.Contains(c.Line, n) {
				c.Line = ""
			}
		}
	}
	return c
}

// shares writes "Blivius 12h · Office 9h", each name in its colour, or ▒▒▒ blocks when hidden.
func shares(ss []cardShare, w int) string {
	var parts []string
	for _, s := range ss {
		name := s.Name
		if name == "" {
			name = "▒▒▒▒▒"
		}
		parts = append(parts, paint(s.color, name)+" "+bold.Render(roundH(s.Hours)))
	}
	return fit(strings.Join(parts, dim.Render(" · ")), w)
}

// roundH is hours as a card says them: 45m, 8.1h, 22h.
func roundH(x float64) string {
	switch {
	case x < 1:
		return hdur(x)
	case x < 10:
		return strconv.FormatFloat(math.Round(x*10)/10, 'f', -1, 64) + "h"
	}
	return fmt.Sprintf("%.0fh", x)
}

func (c cardData) render() string {
	var b strings.Builder
	b.WriteString(bold.Render(paint(accent, "renpet")) + dim.Render(" · "+c.Range) + "\n\n")
	most := max(c.YouHours, c.AgentHours)
	line := func(label string, x float64, hex, tail string) {
		f := 0.0
		if most > 0 {
			f = x / most
		}
		fmt.Fprintf(&b, "%s %s  %s%s\n", bold.Render(fmt.Sprintf("%-7s", label)), bold.Render(rjust(hdur(x), 8)), paint(hex, fit(bar(f, 24), 24)), tail)
	}
	line("you", c.YouHours, voiceHex, "")
	mult := ""
	if c.YouHours > 0.01 {
		mult = "  " + bold.Render(paint(accent, fmt.Sprintf("%.1f×", c.AgentHours/c.YouHours)))
	}
	line("agents", c.AgentHours, accent, mult)
	fmt.Fprintf(&b, "%s %s %s\n", dim.Render("agents waited on you"), bold.Render(hdur(c.WaitedHours)), dim.Render(fmt.Sprintf("· peak %d at once", c.Peak)))
	if len(c.For) > 0 {
		b.WriteString("\n" + dim.Render("for  ") + shares(c.For, cardW-5) + "\n")
	}
	if len(c.Top) > 0 {
		b.WriteString(dim.Render("top  ") + shares(c.Top, cardW-5) + "\n")
	}
	if c.Line != "" {
		b.WriteString("\n" + paint(voiceHex, fit(c.Line, cardW)) + "\n")
	}
	b.WriteString("\n" + dim.Render(rjust("github.com/blivius/renpet", cardW)))
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color(accent)).Padding(1, 3).
		Render(b.String())
}

// cardBox is the card over the TUI, with its keys under it.
func (m model) cardBox() string {
	keys := "[n] show names  any key closes"
	if !m.cardHide {
		keys = "[n] hide names  any key closes"
	}
	return lipgloss.JoinVertical(lipgloss.Center, cardFrom(*m.d, m.rangeLabel(), m.rules, m.cardHide).render(), dim.Render(keys))
}
