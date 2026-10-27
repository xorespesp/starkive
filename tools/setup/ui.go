// How the setup talks with the person (`ui`), and its terminal implementation (`terminalUI`, `-headless`).
// The step logic (`run`) shows and asks things only through this interface.
// The browser setup page implementation is in `webui.go`.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"golang.org/x/term"
)

// How the setup shows things to the person and asks them questions.
// A Go interface is a list of methods.
// Any type that has these methods can be used as this type, without declaring it.
// Backticks (`...`) in messages (`say`, `note`, prompts) mark code or names.
// The setup page shows them as code, and the terminal drops them.
type ui interface {
	step(n int)                       // Starts step `n` (from 1) of `setupSteps`. The steps before it are done
	say(line string)                  // One line of guidance
	note(line string)                 // One line the person must not miss. The setup page shows it in a box that stands out
	value(name string, value string)  // A value the person copies to another place (an environment variable, etc.) and its name. The setup page adds a button that copies only the value
	link(label string, target string) // A link the person can open if they want, and its label. The setup goes on even if they don't open it
	open(label string, target string) // A link the person must open and act on before the setup can go on, and its label. The person opens it when ready: the setup page shows a button, and the terminal prints the link
	// Tells that the setup finished with success.
	// `text` is the last message, `target` is the next link to open, and `label` is its label.
	// The setup page shows a result card, and the terminal prints the message and the link
	done(text string, label string, target string)
	// Asks for one line and returns the answer with leading and trailing spaces removed.
	// `hint` is an example of what to enter (empty if none).
	// The setup page shows it faintly inside the input box
	ask(prompt string, hint string) (string, error)
	askSecret(prompt string) (string, error) // Same as `ask`, but doesn't show the typed characters and keeps the answer nowhere (secrets)
	// Waits for a value that is checked in the background to arrive on `received`.
	// The person can also enter it by hand.
	// `waitResult` tells which way it ended.
	// The setup page shows the `waiting` sentence, a progress indicator and a link labeled `manualEntryLabel` to enter it by hand, instead of an input box.
	// When that link is clicked, it returns `switchedToTyping`.
	// The terminal prints the `waiting` sentence and `prompt`, and the person can type a line while waiting (`typedAnswer`)
	waitForValue(waiting string, prompt string, manualEntryLabel string, received <-chan string) (string, waitResult, error)
}

// Why a wait (`ui.waitForValue`) ended
type waitResult int

const (
	receivedValue    waitResult = iota // The value checked in the background arrived. The returned string is that value
	typedAnswer                        // The person typed an answer while waiting (terminal). The returned string is that answer
	switchedToTyping                   // The person chose to enter it by hand (link on the setup page). The caller stops the background check and asks with `ask`
)

// A `ui` that runs in the terminal.
// It doesn't open a browser.
// It only prints the links to open
type terminalUI struct {
	in      *bufio.Reader     // Buffer that reads standard input line by line
	lines   chan terminalLine // Channel that receives the result of the line being read (`readLine`)
	reading bool              // Whether the goroutine that reads standard input is running. Becomes `false` when its result is taken
}

// The result of reading one line from standard input
type terminalLine struct {
	text string // The line, with leading and trailing spaces removed
	err  error  // An error if the input ended and nothing more can be read
}

// Creates a `terminalUI` that uses standard input and output
func newTerminalUI() *terminalUI {
	return &terminalUI{in: bufio.NewReader(os.Stdin), lines: make(chan terminalLine, 1)}
}

// Removes backticks from the message `text`.
// The terminal has no code style, so it shows only the characters
func stripBackticks(text string) string { return strings.ReplaceAll(text, "`", "") }

// Prints the number and name of step `n` after an empty line
func (t *terminalUI) step(n int) {
	fmt.Println()
	fmt.Printf("%d. %s\n", n, setupSteps[n-1])
}

// Prints one line of guidance `line`, indented
func (t *terminalUI) say(line string) { fmt.Println("   " + stripBackticks(line)) }

// Prints one line `line` that must not be missed, indented and with `Note:` in front
func (t *terminalUI) note(line string) { fmt.Println("   Note: " + stripBackticks(line)) }

// Prints the last message `text` and the next link `target` to open (label `label`)
func (t *terminalUI) done(text string, label string, target string) {
	fmt.Println()
	t.say(text)
	t.link(label, target)
}

// Prints the name `name` and the value `value` between empty lines.
// The value is on its own line, so the person can copy only the value without the name
func (t *terminalUI) value(name string, value string) {
	fmt.Println()
	fmt.Println("   " + name + ":")
	fmt.Println("   " + value)
	fmt.Println()
}

// Prints the label `label` and the link `target`
func (t *terminalUI) link(label string, target string) { fmt.Println("   " + label + ": " + target) }

// Prints the label `label` and the link `target`, and asks the person to open it in a browser.
// If it is a localhost address of this computer, it also explains how to open it from a browser on another computer
func (t *terminalUI) open(label string, target string) {
	t.link(label, target)
	fmt.Println("   Open the link above in your browser.")
	if hint := buildPortForwardHint(target); hint != "" {
		fmt.Println("   " + hint)
	}
}

// Returns the channel that receives the next line of standard input.
// It starts a new reading goroutine only when none is running.
//
// NOTE: A read from standard input can't be cancelled halfway.
// When `waitForValue` ends because the checked value arrived first, the read keeps running.
// So the next prompt doesn't start a new read, and it takes the result of that read.
// Two reading goroutines would read the same buffer together and mix up the lines.
func (t *terminalUI) readLine() <-chan terminalLine {
	if !t.reading {
		t.reading = true
		go func() { // `go`: reads in a goroutine (a lightweight thread), so other values can arrive while waiting
			line, err := t.in.ReadString('\n')
			if err != nil && line == "" {
				t.lines <- terminalLine{err: errors.New("no more input")}
				return
			}
			t.lines <- terminalLine{text: strings.TrimSpace(line)}
		}()
	}
	return t.lines
}

// Prints the prompt `prompt`, indented.
// The terminal has no hint inside an input box, so if there is a `hint`, it is added in parentheses before the `:` at the end of the prompt
func printPrompt(prompt string, hint string) {
	if hint != "" {
		prompt = strings.TrimSuffix(prompt, ":") + " (" + hint + "):"
	}
	fmt.Print("   " + stripBackticks(prompt) + " ")
}

// Prints the prompt `prompt` (hint `hint`), reads one line from standard input and returns it with leading and trailing spaces removed.
// It is an error if the input ended (EOF) with nothing read.
// This keeps callers that repeat the same prompt from looping forever
func (t *terminalUI) ask(prompt string, hint string) (string, error) {
	printPrompt(prompt, hint)
	line := <-t.readLine()
	t.reading = false
	return line.text, line.err
}

// Prints the waiting sentence `waiting` and the prompt `prompt`.
// Returns whichever comes first: a value from `received` or a line from standard input.
// A received value is printed so the person can see it.
// The prompt ends with a note that typing is optional (the person can type at any time while waiting).
// The reason it ended is `receivedValue` or `typedAnswer`.
// The terminal has no link, so `manualEntryLabel` isn't used
func (t *terminalUI) waitForValue(waiting string, prompt string, manualEntryLabel string, received <-chan string) (string, waitResult, error) {
	t.say(waiting)
	printPrompt(prompt, "optional, it continues on its own")
	select { // Takes whichever comes first
	case line := <-t.readLine():
		t.reading = false
		return line.text, typedAnswer, line.err
	case value := <-received:
		fmt.Println(value)
		return value, receivedValue, nil
	}
}

// Same as `ask`, but the typed characters don't show on the screen.
// If standard input isn't a terminal (input passed through a pipe), it can't hide them, so it just reads like `ask`
func (t *terminalUI) askSecret(prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) || t.reading { // If an earlier read is still running, a hidden read can't be done separately, so it uses that read
		return t.ask(prompt, "")
	}
	printPrompt(prompt, "")
	value, err := term.ReadPassword(fd)
	fmt.Println() // A hidden read doesn't print the newline the person typed either, so it prints one instead
	return strings.TrimSpace(string(value)), err
}

// Builds a sentence that explains the port forwarding needed to open the localhost address `target` of this computer from a browser on another computer.
// Returns an empty string if it isn't a localhost address.
//
// NOTE: The setup page and the GitHub App step need the browser to reach this tool's localhost server.
// If the tool runs on a computer you connected to over SSH, the browser on your own computer can't reach that computer's localhost.
// So the same port must be forwarded over SSH.
func buildPortForwardHint(target string) string {
	u, err := url.Parse(target)
	if err != nil || (u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1") {
		return ""
	}
	return fmt.Sprintf("If your browser is on another computer, connect to this one with ssh -L %s:127.0.0.1:%s and open it there.", u.Port(), u.Port())
}
