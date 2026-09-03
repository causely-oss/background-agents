package main

import (
	"fmt"
	"regexp"
	"strings"
)

// toSlackMrkdwn converts Claude's free-form GitHub-flavored Markdown into
// Slack's mrkdwn dialect, which differs in two ways that matter here:
//   - bold is *single* asterisks, not **double** — GitHub's "**bold**" renders
//     as literal asterisks in Slack instead of bold text.
//   - Slack has no table syntax at all — a GitHub-style "| a | b |" table
//     renders as literal pipe characters, not a table.
//
// This is a defensive safety net: buildSystemPrompt also instructs Claude to
// write Slack-safe text directly, but Claude doesn't always follow formatting
// instructions exactly, so both layers exist.
func toSlackMrkdwn(text string) string {
	text = convertBoldAsterisks(text)
	text = convertMarkdownTables(text)
	return text
}

// convertBoldAsterisks turns "**bold**" into "*bold*". A blanket "**" -> "*"
// replacement is safe here because the only legitimate use of "**" in this
// text is GitHub-style bold — there's no math or other context where a
// literal "**" would appear in an SRE investigation summary.
func convertBoldAsterisks(text string) string {
	return strings.ReplaceAll(text, "**", "*")
}

// tableSeparatorRe matches a markdown table's header/body separator row, e.g.
// "|---|---|" or "| :-- | --: |".
var tableSeparatorRe = regexp.MustCompile(`^\|?[\s:|-]+\|?$`)

// convertMarkdownTables rewrites GitHub-style pipe tables into "*header:*
// value" bullet lines, since Slack mrkdwn cannot render tables at all. The
// header row becomes the label for each cell in later rows rather than being
// printed itself.
func convertMarkdownTables(text string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	var headers []string
	inTable := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		isRow := strings.HasPrefix(trimmed, "|") && strings.Count(trimmed, "|") >= 2

		if !isRow {
			inTable = false
			headers = nil
			out = append(out, line)
			continue
		}

		if tableSeparatorRe.MatchString(trimmed) {
			// The "|---|---|" row marks the table as started; skip printing it.
			inTable = true
			continue
		}

		cells := splitTableRow(trimmed)

		if !inTable {
			// First pipe-row seen without a preceding separator is the header.
			headers = cells
			inTable = true
			continue
		}

		var parts []string
		for i, c := range cells {
			c = strings.TrimSpace(c)
			if c == "" {
				continue
			}
			if i < len(headers) && strings.TrimSpace(headers[i]) != "" {
				parts = append(parts, fmt.Sprintf("*%s:* %s", strings.TrimSpace(headers[i]), c))
			} else {
				parts = append(parts, c)
			}
		}
		out = append(out, "• "+strings.Join(parts, " — "))
	}

	return strings.Join(out, "\n")
}

// splitTableRow splits "| a | b | c |" into ["a", "b", "c"], tolerating
// missing leading/trailing pipes.
func splitTableRow(row string) []string {
	row = strings.TrimPrefix(row, "|")
	row = strings.TrimSuffix(row, "|")
	return strings.Split(row, "|")
}
