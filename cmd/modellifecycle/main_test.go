package main

import (
	"strings"
	"testing"
	"time"
)

const docFixture = `<html><body>
<table><tr><th>API model name</th><th>Tentative retirement date</th></tr>
<tr><td>claude-opus-5</td><td>Not sooner than September 1, 2027</td></tr></table>
<table><tr><th>Model</th><th>Release date</th><th>Shutdown date</th><th>Recommended replacement</th></tr>
<tr><td>gemini-2.0-flash</td><td>February 5, 2025</td><td>June 1, 2026</td><td>gemini-3.6-flash</td></tr>
<tr><td>gemini-2.5-pro</td><td>June 17, 2025</td><td>No shutdown date announced</td><td></td></tr></table>
<table><tr><th>Shutdown date</th><th>Model snapshot</th><th>Substitute model</th></tr>
<tr><td>2026‑10‑23</td><td>gpt-4.1-nano | gpt-4.1-nano-2025-04-14</td><td>gpt-5.6-luna</td></tr></table>
</body></html>`

func TestParseDocNotices(t *testing.T) {
	notices, err := parseDocNotices(strings.NewReader(docFixture))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, n := range notices {
		got[n.ID] = n.Retires.Format(time.DateOnly) + " " + n.Replacement
	}
	want := map[string]string{
		"gemini-2.0-flash":        "2026-06-01 gemini-3.6-flash",
		"gpt-4.1-nano":            "2026-10-23 gpt-5.6-luna",
		"gpt-4.1-nano-2025-04-14": "2026-10-23 gpt-5.6-luna",
	}
	if len(got) != len(want) {
		t.Fatalf("notices = %v, want %v", got, want)
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("notice %q = %q, want %q", id, got[id], w)
		}
	}
}

func TestParseDocNoticesRejectsPageWithoutRetirementTable(t *testing.T) {
	if _, err := parseDocNotices(strings.NewReader(`<table><tr><th>Model</th></tr></table>`)); err == nil {
		t.Fatal("expected an error so a reworked page surfaces as unreadable")
	}
}

func TestMatches(t *testing.T) {
	cases := []struct {
		id, upstream string
		want         bool
	}{
		{"claude-sonnet-4-5-20250929", "claude-sonnet-4-5", true},
		{"claude-opus-4-20250514", "claude-opus-4-0", true},
		{"gpt-5-2025-08-07", "gpt-5", true},
		{"models/gemini-2.5-pro", "gemini-2.5-pro", true},
		{"gpt-5-mini", "gpt-5", false},
		{"gpt-4o-mini-2024-07-18", "gpt-4o", false},
	}
	for _, c := range cases {
		if got := matches(c.id, c.upstream); got != c.want {
			t.Errorf("matches(%q, %q) = %v, want %v", c.id, c.upstream, got, c.want)
		}
	}
}

func TestReport(t *testing.T) {
	now := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	b := bindings{
		"makora": {"deepseek/deepseek-v4-flash": {"deepseek/deepseek-v4-flash"}},
		"openai": {"gpt-4.1-nano": {"gpt-4.1-nano"}, "gpt-5": {"gpt-5"}},
	}
	notices := []notice{
		{"gpt-4.1-nano", now.AddDate(0, 0, 7), "gpt-5.6-luna"},
		{"gpt-5-2025-08-07", now.AddDate(0, 0, 67), ""},
	}
	findings := merge(append(
		scheduled(b, "openai", notices, "https://example.com/deprecations"),
		unlisted(b, "makora", []string{"deepseek-ai/DeepSeek-V4.1-Flash"}, "makora /models")...,
	))

	daily := report(findings, []string{"together deprecations page: status 503"}, now, false)
	for _, want := range []string{
		"*makora* `deepseek/deepseek-v4-flash` is no longer listed by the provider",
		"*openai* `gpt-4.1-nano` retires 2026-10-12 (in 7 days) · catalog `gpt-4.1-nano` · replacement: gpt-5.6-luna · <https://example.com/deprecations|source>",
	} {
		if !strings.Contains(daily, want) {
			t.Errorf("daily report missing %q:\n%s", want, daily)
		}
	}
	if strings.Contains(daily, "gpt-5`") || strings.Contains(daily, "Unreadable") {
		t.Errorf("daily report should skip off-threshold findings and source failures:\n%s", daily)
	}

	weekly := report(findings, []string{"together deprecations page: status 503"}, now, true)
	for _, want := range []string{"(weekly digest)", "`gpt-5` retires 2026-12-11 (in 67 days) · catalog `gpt-5` · notice names `gpt-5-2025-08-07`", "_Unreadable sources:_ together deprecations page: status 503"} {
		if !strings.Contains(weekly, want) {
			t.Errorf("digest missing %q:\n%s", want, weekly)
		}
	}

	if got := report(nil, nil, now, false); got != "" {
		t.Errorf("empty daily report = %q, want empty", got)
	}
}
