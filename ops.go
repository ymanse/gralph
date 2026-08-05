package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	lua "github.com/yuin/gopher-lua"
)

// ---------------------------------------------------------------------------
// Operational subcommands: `gralph status`, `gralph reset`, `gralph validate`.
// They never launch agent sessions; they inspect or repair the state
// directory and lint profiles without executing any gate code.
// ---------------------------------------------------------------------------

// statusReport is the machine-readable shape of `gralph status --json`.
type statusReport struct {
	Profile     string         `json:"profile"`
	Instance    string         `json:"instance"`
	StateDir    string         `json:"state_dir"`
	Cursor      string         `json:"cursor"`
	SessionID   string         `json:"session_id"`
	Failures    map[string]int `json:"failures"`
	Subcommands []subStatus    `json:"subcommands,omitempty"`
}

// subStatus is the quota progress of one subcommand of the cursor node.
type subStatus struct {
	Name  string   `json:"name"`
	Done  int      `json:"done"`
	Count int      `json:"count"`
	Keys  []string `json:"keys"`
}

// buildStatus assembles the report from state.json, plus progress.json when
// the (effective) cursor node has subcommand quotas.
func buildStatus(p *Profile) (*statusReport, error) {
	st, err := LoadState(p.StateDir)
	if err != nil {
		return nil, err
	}
	r := &statusReport{
		Profile:   p.Path,
		Instance:  p.Name,
		StateDir:  p.StateDir,
		Cursor:    st.Cursor,
		SessionID: st.SessionID,
		Failures:  st.Failures,
	}
	cursor := st.Cursor
	if cursor == "" {
		cursor = p.FirstCommand().Name // implicit entry on a fresh run
	}
	if cmd := p.Command(cursor); cmd != nil && len(cmd.Subcommands) > 0 {
		pr, err := LoadProgress(p.StateDir, cmd.Name)
		if err != nil {
			return nil, err
		}
		for i := range cmd.Subcommands {
			s := &cmd.Subcommands[i]
			r.Subcommands = append(r.Subcommands, subStatus{
				Name:  s.Name,
				Done:  pr.CountDone(s.Name),
				Count: s.Count,
				Keys:  pr.DoneKeys(s.Name),
			})
		}
	}
	return r, nil
}

// printStatus renders the report for humans, or as JSON with --json.
func printStatus(p *Profile, asJSON bool) error {
	r, err := buildStatus(p)
	if err != nil {
		return err
	}
	if asJSON {
		data, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	}
	fmt.Printf("profile:   %s\n", r.Profile)
	fmt.Printf("instance:  %s\n", r.Instance)
	fmt.Printf("state dir: %s\n", r.StateDir)
	if r.Cursor == "" {
		fmt.Printf("cursor:    (not started; entry is %q)\n", p.FirstCommand().Name)
	} else {
		fmt.Printf("cursor:    %s\n", r.Cursor)
	}
	if r.SessionID == "" {
		fmt.Println("session:   (none)")
	} else {
		fmt.Printf("session:   %s\n", r.SessionID)
	}
	if len(r.Failures) == 0 {
		fmt.Println("failures:  (none)")
	} else {
		fmt.Println("failures:")
		keys := make([]string, 0, len(r.Failures))
		for k := range r.Failures {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("  %s: %d\n", k, r.Failures[k])
		}
	}
	if len(r.Subcommands) > 0 {
		fmt.Println("subcommand progress:")
		for _, s := range r.Subcommands {
			fmt.Printf("  %s: %d/%d done", s.Name, s.Done, s.Count)
			if len(s.Keys) > 0 {
				fmt.Printf(" (%s)", strings.Join(s.Keys, ", "))
			}
			fmt.Println()
		}
	}
	return nil
}

// resetStateDir clears gralph's on-disk state under the state lock.
// failuresOnly zeroes just the failure counters, keeping the cursor, session
// id, user store and subcommand progress -- the escape hatch for manual
// sessions, where counters accumulate without an orchestrator to rotate them.
func resetStateDir(stateDir string, failuresOnly bool) error {
	return withStateLock(stateDir, func() error {
		if failuresOnly {
			st, err := LoadState(stateDir)
			if err != nil {
				return err
			}
			st.Failures = map[string]int{}
			return st.Save(stateDir)
		}
		// blocked.json goes with them: a reset that left it behind would hand
		// back a fresh flow that exits 3 before its first iteration.
		for _, path := range []string{statePath(stateDir), storePath(stateDir), progressPath(stateDir), blockPath(stateDir)} {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		return nil
	})
}

// confirmReset asks y/N on the terminal. A non-TTY stdin cannot confirm, so
// scripted callers must pass --force.
func confirmReset(prompt string) (bool, error) {
	fi, err := os.Stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false, fmt.Errorf("stdin is not a terminal; pass --force to skip confirmation")
	}
	fmt.Fprintf(os.Stderr, "%s [y/N]: ", prompt)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes", nil
}

// lintProfile checks a profile without running anything: the loader's full
// validation rules, lua file existence and syntax (compiled, never executed),
// plus graph warnings (nodes unreachable from the entry command, no reachable
// terminal node so DONE can never be reached).
func lintProfile(path string) (errs, warns []string) {
	p, err := LoadProfile(path)
	if err != nil {
		return []string{err.Error()}, nil
	}

	L := lua.NewState()
	defer L.Close()
	checkLua := func(owner, rel string) {
		if rel == "" {
			return
		}
		abs := p.resolvePath(rel)
		if _, err := os.Stat(abs); err != nil {
			errs = append(errs, fmt.Sprintf("%s: lua script: %v", owner, err))
			return
		}
		// LoadFile compiles without executing, so linting never runs gate code.
		if _, err := L.LoadFile(abs); err != nil {
			errs = append(errs, fmt.Sprintf("%s: lua syntax: %v", owner, err))
		}
	}
	for i := range p.Commands {
		c := &p.Commands[i]
		checkLua(fmt.Sprintf("command %q", c.Name), c.Lua)
		for j := range c.Subcommands {
			s := &c.Subcommands[j]
			checkLua(fmt.Sprintf("subcommand %q of %q", s.Name, c.Name), s.Lua)
		}
	}

	// Reachability from the entry command over `next` edges.
	entry := p.FirstCommand().Name
	reached := map[string]bool{entry: true}
	queue := []string{entry}
	for len(queue) > 0 {
		cmd := p.Command(queue[0])
		queue = queue[1:]
		for _, n := range cmd.Next {
			if !reached[n] {
				reached[n] = true
				queue = append(queue, n)
			}
		}
	}
	doneReachable := false
	for i := range p.Commands {
		c := &p.Commands[i]
		if !reached[c.Name] {
			warns = append(warns, fmt.Sprintf("command %q is unreachable from the entry command %q", c.Name, entry))
			continue
		}
		if len(c.Next) == 0 {
			doneReachable = true
		}
	}
	if !doneReachable {
		warns = append(warns, fmt.Sprintf(
			"no terminal command (one without `next`) is reachable from %q; the cursor can never become %s and the loop will not finish",
			entry, DoneCursor))
	}
	warns = append(warns, flatInvocationWarnings(p)...)
	return errs, warns
}

// flatInvocationWarnings flags hand-written guidance that tells the agent to
// run `gralph <name>` for a name defined in the profile. The flat form does
// not dispatch (custom commands run as `gralph do <name>`), so such guidance
// would send the agent into an unknown-command error.
func flatInvocationWarnings(p *Profile) (warns []string) {
	names := map[string]bool{}
	for i := range p.Commands {
		c := &p.Commands[i]
		names[c.Name] = true
		for j := range c.Subcommands {
			names[c.Subcommands[j].Name] = true
		}
	}
	for i := range p.Commands {
		c := &p.Commands[i]
		for _, n := range flatInvocations(c.Guidance, names) {
			warns = append(warns, fmt.Sprintf(
				"command %q: guidance invokes `gralph %s`, which does not dispatch; write `gralph do %s`",
				c.Name, n, n))
		}
	}
	return warns
}

// flatInvocations returns each profile-defined name that text invokes as
// `gralph <name>`, once per name, in first-appearance order.
func flatInvocations(text string, names map[string]bool) []string {
	const prefix = "gralph "
	isNameRune := func(r rune) bool {
		return r == '-' || r == '_' ||
			r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
	}
	seen := map[string]bool{}
	var out []string
	for rest := text; ; {
		i := strings.Index(rest, prefix)
		if i < 0 {
			break
		}
		rest = rest[i+len(prefix):]
		tok := rest
		if j := strings.IndexFunc(rest, func(r rune) bool { return !isNameRune(r) }); j >= 0 {
			tok = rest[:j]
		}
		if names[tok] && !seen[tok] {
			seen[tok] = true
			out = append(out, tok)
		}
	}
	return out
}

// runValidate is the CLI body of `gralph validate`: errors exit 1, warnings
// alone exit 0.
func runValidate(path string) int {
	errs, warns := lintProfile(path)
	for _, w := range warns {
		fmt.Printf("WARNING: %s\n", w)
	}
	for _, e := range errs {
		fmt.Printf("ERROR: %s\n", e)
	}
	if len(errs) > 0 {
		fmt.Printf("%s: %d error(s), %d warning(s)\n", path, len(errs), len(warns))
		return 1
	}
	fmt.Printf("%s: OK (%d warning(s))\n", path, len(warns))
	return 0
}
