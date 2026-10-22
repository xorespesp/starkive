// Messages of the commits that the server and the setup tool make in the DB repository (`CommitMessage`, `NewCommitMessage`),
// and the text they are made of (`formatCommitList`, `FormatCount`).
package core

import (
	"fmt"
	"strings"
)

// Maximum length of a commit message, in characters.
// `formatCommitList` already shortens lists in the body, so this is only a safety net for messages that are still too long
const maxCommitMessage = 4000

// Maximum number of items listed in a commit body.
// The rest is shortened to "… and N more"
const maxListedItems = 20

// Builds one list block for a commit body: a title line and `- item` lines.
//
// Args:
//   - `title`: Title of the list (e.g. "New star lists"). If empty, no title line is written
//   - `items`: Items to list
//
// Returns:
//   - The list text. An empty string if there are no items (`NewCommitMessage` skips it)
//
// NOTE: A sync or an Import can have thousands of items.
// So only up to `maxListedItems` items are listed, and the rest is given as a count.
// The diff shows the actual changes.
func formatCommitList(title string, items []string) string {
	if len(items) == 0 {
		return ""
	}
	lines := []string{}
	if title != "" {
		lines = append(lines, title+":")
	}
	for _, item := range items[:min(len(items), maxListedItems)] {
		lines = append(lines, "- "+item)
	}
	if len(items) > maxListedItems {
		lines = append(lines, fmt.Sprintf("… and %d more", len(items)-maxListedItems))
	}
	return strings.Join(lines, "\n")
}

// Message of a DB repository commit: one subject line and a body below it.
// Made with `NewCommitMessage`
type CommitMessage struct {
	subject string   // Subject in the form `<prefix>: <summary>` (`NewCommitMessage`)
	body    []string // Body blocks, added below the subject with blank lines between them
}

// Builds a commit message.
// The subject is `<prefix>: <summary>`, so commits can be filtered by who made them, like `git log --grep "^sync("`.
//
// Args:
//   - `prefix`: Who made the change and which file changed, written like `user(star-items)`.
//     Who is `user` (something a person did in the dashboard or the setup tool) or `sync` (a sync, `SyncArchiveWithGitHub`).
//     The file is `star-items`, `star-lists`, `repo-meta`, `config`, or `workflow`, or the folder `readmes` (`ReadmesDBDir`).
//     The one exception is the commit that adds the initial files of the DB repository, whose prefix is `feat` (`feat: Add initial files`)
//   - `summary`: What changed, starting with a verb (e.g. "add owner/repo")
//   - `body`: Body blocks (use `formatCommitList` for a list of many items). Empty blocks are skipped
//
// NOTE: The fields are private, so other packages can make a message only with this function, or as the zero value (which isn't valid).
// That way, every commit has the same subject and body format.
func NewCommitMessage(prefix string, summary string, body ...string) CommitMessage {
	m := CommitMessage{subject: prefix + ": " + summary}
	for _, section := range body {
		if section != "" {
			m.body = append(m.body, section)
		}
	}
	return m
}

// Reports whether the subject has the form `<prefix>: <summary>`, with neither part empty or only spaces.
// The zero value (`CommitMessage{}`) doesn't.
func (m CommitMessage) isValid() bool {
	prefix, summary, found := strings.Cut(m.subject, ": ")
	return found && strings.TrimSpace(prefix) != "" && strings.TrimSpace(summary) != ""
}

// Turns the message into the text for a commit (subject, blank line, body blocks separated by blank lines).
// If it is longer than `maxCommitMessage` characters, the end is cut off (the subject comes first, so it is never cut)
func (m CommitMessage) String() string {
	text := strings.Join(append([]string{m.subject}, m.body...), "\n\n")
	if runes := []rune(text); len(runes) > maxCommitMessage { // Cuts by character, not by byte, so multi-byte characters are not broken
		text = string(runes[:maxCommitMessage])
	}
	return text
}

// Joins a count and an English noun in singular or plural form (1 star, 3 stars).
//
// Args:
//   - `n`: The count
//   - `noun`: The noun in singular form. The plural adds `s` at the end
func FormatCount(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
