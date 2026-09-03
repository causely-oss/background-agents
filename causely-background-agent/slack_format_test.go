package main

import (
	"strings"
	"testing"
)

func TestConvertBoldAsterisks(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"simple bold", "This is **bold** text", "This is *bold* text"},
		{"multiple bold spans", "**a** and **b**", "*a* and *b*"},
		{"already single-asterisk: unaffected", "This is *bold* text", "This is *bold* text"},
		{"no bold: unaffected", "plain text", "plain text"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := convertBoldAsterisks(tt.in); got != tt.want {
				t.Errorf("convertBoldAsterisks(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestConvertMarkdownTables(t *testing.T) {
	t.Run("real example from the orders-service demo", func(t *testing.T) {
		in := "**Root Cause Analysis:**\n\n" +
			"| Signal | Detail |\n" +
			"|---|---|\n" +
			"| **Exception** | KeyError on an environment variable key |\n" +
			"| **Crash location** | app/app.py, line 5 |\n\n" +
			"The fix is a one-character typo correction."
		out := convertMarkdownTables(in)
		if strings.Contains(out, "|") {
			t.Errorf("output still contains pipe characters, tables weren't converted:\n%s", out)
		}
		if !strings.Contains(out, "Signal:") {
			t.Errorf("expected header 'Signal' to label the first cell, got:\n%s", out)
		}
		if !strings.Contains(out, "KeyError on an environment variable key") {
			t.Errorf("expected cell content to survive conversion, got:\n%s", out)
		}
	})

	t.Run("no table present: text passes through unchanged", func(t *testing.T) {
		in := "Just a plain paragraph with no tables at all."
		if got := convertMarkdownTables(in); got != in {
			t.Errorf("convertMarkdownTables(%q) = %q, want unchanged", in, got)
		}
	})

	t.Run("text before and after a table is preserved", func(t *testing.T) {
		in := "Before.\n| A | B |\n|---|---|\n| 1 | 2 |\nAfter."
		out := convertMarkdownTables(in)
		if !strings.HasPrefix(out, "Before.\n") {
			t.Errorf("expected leading text preserved, got:\n%s", out)
		}
		if !strings.HasSuffix(out, "\nAfter.") {
			t.Errorf("expected trailing text preserved, got:\n%s", out)
		}
	})
}

func TestToSlackMrkdwn_FullPipeline(t *testing.T) {
	in := "**Root cause identified!**\n\n" +
		"| Signal | Detail |\n" +
		"|---|---|\n" +
		"| **Bug** | Typo in the env var name |\n\n" +
		"Fix PR: https://github.com/org/repo/pull/8"
	out := toSlackMrkdwn(in)
	if strings.Contains(out, "**") {
		t.Errorf("double asterisks survived: %q", out)
	}
	if strings.Contains(out, "|") {
		t.Errorf("pipe table syntax survived: %q", out)
	}
	if !strings.Contains(out, "*Root cause identified!*") {
		t.Errorf("expected single-asterisk bold heading, got: %q", out)
	}
	if !strings.Contains(out, "https://github.com/org/repo/pull/8") {
		t.Errorf("expected PR link preserved, got: %q", out)
	}
}
