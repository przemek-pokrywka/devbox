// Copyright 2022 Jetpack Technologies Inc and contributors. All rights reserved.
// Use of this source code is governed by the license in the LICENSE file.

package nix

import (
	"bytes"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/pkg/errors"
	"go.jetpack.io/devbox/debug"
)

//go:embed shellrc.tmpl
var shellrcText string
var shellrcTmpl = template.Must(template.New("shellrc").Parse(shellrcText))

type name string

const (
	shUnknown name = ""
	shBash    name = "bash"
	shZsh     name = "zsh"
	shKsh     name = "ksh"
	shPosix   name = "posix"
)

// Shell configures a user's shell to run in Devbox. Its zero value is a
// fallback shell that launches a regular Nix shell.
type Shell struct {
	name            name
	binPath         string
	userShellrcPath string
	planInitHook    string

	// UserInitHook contains commands that will run at shell startup.
	UserInitHook string
}

type ShellOption func(*Shell)

// DetectShell attempts to determine the user's default shell.
func DetectShell(opts ...ShellOption) (*Shell, error) {
	path := os.Getenv("SHELL")
	if path == "" {
		return nil, errors.New("unable to detect the current shell")
	}

	sh := &Shell{binPath: filepath.Clean(path)}
	base := filepath.Base(path)
	// Login shell
	if base[0] == '-' {
		base = base[1:]
	}
	switch base {
	case "bash":
		sh.name = shBash
		sh.userShellrcPath = rcfilePath(".bashrc")
	case "zsh":
		sh.name = shZsh
		sh.userShellrcPath = rcfilePath(".zshrc")
	case "ksh":
		sh.name = shKsh
		sh.userShellrcPath = rcfilePath(".kshrc")
	case "dash", "ash", "sh":
		sh.name = shPosix
		sh.userShellrcPath = os.Getenv("ENV")

		// Just make up a name if there isn't already an init file set
		// so we have somewhere to put a new one.
		if sh.userShellrcPath == "" {
			sh.userShellrcPath = ".shinit"
		}
	default:
		sh.name = shUnknown
	}

	for _, opt := range opts {
		opt(sh)
	}

	debug.Log("Detected shell: %s", sh.binPath)
	debug.Log("Recognized shell as: %s", sh.binPath)
	debug.Log("Looking for user's shell init file at: %s", sh.userShellrcPath)
	return sh, nil
}

func WithPlanInitHook(hook string) ShellOption {
	return func(s *Shell) {
		s.planInitHook = hook
	}
}

// rcfilePath returns the absolute path for an rcfile, which is usually in the
// user's home directory. It doesn't guarantee that the file exists.
func rcfilePath(basename string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, basename)
}

func (s *Shell) Run(nixPath string) error {
	// Just to be safe, we need to guarantee that the NIX_PROFILES paths
	// have been filepath.Clean'ed. The shellrc.tmpl has some commands that
	// assume they are.
	nixProfileDirs := splitNixList(os.Getenv("NIX_PROFILES"))

	// Copy the current PATH into nix-shell, but clean and remove some
	// directories that are incompatible.
	parentPath := cleanEnvPath(os.Getenv("PATH"), nixProfileDirs)
	env := append(os.Environ(),
		"PARENT_PATH="+parentPath,
		"NIX_PROFILES="+strings.Join(nixProfileDirs, " "),

		// Prevent the user's shellrc from re-sourcing nix-daemon.sh
		// inside the devbox shell.
		"__ETC_PROFILE_NIX_SOURCED=1",
	)

	// Launch a fallback shell if we couldn't find the path to the user's
	// default shell.
	if s.binPath == "" {
		cmd := exec.Command("nix-shell", "--pure")
		cmd.Args = append(cmd.Args, toKeepArgs(env)...)
		cmd.Args = append(cmd.Args, nixPath)
		cmd.Env = env
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		debug.Log("Unable to detect the user's shell, falling back to: %v", cmd.Args)
		return errors.WithStack(cmd.Run())
	}

	cmd := exec.Command("nix-shell", "--command", s.execCommand(), "--pure")
	cmd.Args = append(cmd.Args, toKeepArgs(env)...)
	cmd.Args = append(cmd.Args, nixPath)
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	debug.Log("Executing nix-shell command: %v", cmd.Args)
	return errors.WithStack(cmd.Run())
}

// execCommand is a command that replaces the current shell with s. This is what
// Run sets the nix-shell --command flag to.
func (s *Shell) execCommand() string {
	// We exec env, which will then exec the shell. This lets us set
	// additional environment variables before any of the shell's init
	// scripts run.
	args := []string{
		"exec",
		"env",

		// Correct SHELL to be the one we're about to exec.
		fmt.Sprintf(`"SHELL=%s"`, s.binPath),
	}

	// userShellrcPath is empty when we know the path to the user's shell,
	// but we don't recognize its name. In this case we don't know how to
	// override the shellrc file, so just launch the shell without any
	// additional args.
	if s.userShellrcPath == "" {
		return strings.Join(append(args, s.binPath), " ")
	}

	// Create a devbox shellrc file that runs the user's shellrc + the shell
	// hook in devbox.json.
	shellrc, err := s.writeDevboxShellrc()
	if err != nil {
		// Fall back to just launching the shell without a custom
		// shellrc.
		debug.Log("Failed to write devbox shellrc: %v", err)
		return strings.Join(append(args, s.binPath), " ")
	}

	// Shells have different ways of overriding the shellrc, so we need to
	// look at the name to know which env vars or args to set.
	var (
		extraEnv  []string
		extraArgs []string
	)
	switch s.name {
	case shBash:
		extraArgs = []string{"--rcfile", fmt.Sprintf(`"%s"`, shellrc)}
	case shZsh:
		extraEnv = []string{fmt.Sprintf(`"ZDOTDIR=%s"`, filepath.Dir(shellrc))}
	case shKsh, shPosix:
		extraEnv = []string{fmt.Sprintf(`"ENV=%s"`, shellrc)}
	}
	args = append(args, extraEnv...)
	args = append(args, s.binPath)
	args = append(args, extraArgs...)
	return strings.Join(args, " ")
}

func (s *Shell) writeDevboxShellrc() (path string, err error) {
	if s.userShellrcPath == "" {
		// If this happens, then there's a bug with how we detect shells
		// and their shellrc paths. If the shell is unknown or we can't
		// determine the shellrc path, then we should launch a fallback
		// shell instead.
		panic("writeDevboxShellrc called with an empty user shellrc path; use the fallback shell instead")
	}

	// We need a temp dir (as opposed to a temp file) because zsh uses
	// ZDOTDIR to point to a new directory containing the .zshrc.
	tmp, err := os.MkdirTemp("", "devbox")
	if err != nil {
		return "", fmt.Errorf("create temp dir for shell init file: %v", err)
	}

	// This is a best-effort to include the user's existing shellrc. If we
	// can't read it, then just omit it from the devbox shellrc.
	userShellrc, err := os.ReadFile(s.userShellrcPath)
	if err != nil {
		userShellrc = []byte{}
	}

	// If the user already has a shellrc file, then give the devbox shellrc
	// file the same name. Otherwise, use an arbitrary name of "shellrc".
	shellrcName := "shellrc"
	if s.userShellrcPath != "" {
		shellrcName = filepath.Base(s.userShellrcPath)
	}
	path = filepath.Join(tmp, shellrcName)
	shellrcf, err := os.Create(path)
	if err != nil {
		return "", fmt.Errorf("write to shell init file: %v", err)
	}
	defer func() {
		cerr := shellrcf.Close()
		if err == nil {
			err = cerr
		}
	}()

	err = shellrcTmpl.Execute(shellrcf, struct {
		OriginalInit     string
		OriginalInitPath string
		UserHook         string
		PlanInitHook     string
	}{
		OriginalInit:     string(bytes.TrimSpace(userShellrc)),
		OriginalInitPath: filepath.Clean(s.userShellrcPath),
		UserHook:         strings.TrimSpace(s.UserInitHook),
		PlanInitHook:     strings.TrimSpace(s.planInitHook),
	})
	if err != nil {
		return "", fmt.Errorf("execute shellrc template: %v", err)
	}

	debug.Log("Wrote devbox shellrc to: %s", path)
	return path, nil
}

// envToKeep is the set of environment variables that we want to copy verbatim
// to the new devbox shell.
var envToKeep = map[string]bool{
	// POSIX
	//
	// Variables that are part of the POSIX standard.
	"HOME":   true,
	"OLDPWD": true,
	"PWD":    true,
	"TERM":   true,
	"TZ":     true,
	"USER":   true,

	// POSIX Locale
	//
	// Variables that are part of the POSIX standard which define
	// the shell's locale.
	"LC_ALL":      true, // Sets and overrides all of the variables below.
	"LANG":        true, // Default to use for any of the variables below that are unset or null.
	"LC_COLLATE":  true, // Collation order.
	"LC_CTYPE":    true, // Character classification and case conversion.
	"LC_MESSAGES": true, // Formats of informative and diagnostic messages and interactive responses.
	"LC_MONETARY": true, // Monetary formatting.
	"LC_NUMERIC":  true, // Numeric, non-monetary formatting.
	"LC_TIME":     true, // Date and time formats.

	// Common
	//
	// Variables that most programs agree on, but aren't strictly
	// part of POSIX.
	"TERM_PROGRAM":         true, // Name of the terminal the shell is running in.
	"TERM_PROGRAM_VERSION": true, // The version of TERM_PROGRAM.
	"SHLVL":                true, // The number of nested shells.

	// Apple Terminal
	//
	// Special-cased variables that macOS's Terminal.app sets before
	// launching the shell. It's not clear what exactly all of these do,
	// but it seems like omitting them can cause problems.
	"TERM_SESSION_ID":        true,
	"SHELL_SESSIONS_DISABLE": true, // Respect session save/resume setting (see /etc/zshrc_Apple_Terminal).
	"SECURITYSESSIONID":      true,

	// Nix + Devbox
	//
	// Variables specific to running in a Nix shell and devbox shell.
	"PARENT_PATH":               true, // The PATH of the parent shell (where `devbox shell` was invoked).
	"__ETC_PROFILE_NIX_SOURCED": true, // Prevents Nix from being sourced again inside a devbox shell.
	"NIX_SSL_CERT_FILE":         true, // The path to Nix-installed SSL certificates (used by some Nix programs).
	"SSL_CERT_FILE":             true, // The path to non-Nix SSL certificates (used by some Nix and non-Nix programs).
}

// toKeepArgs takes a slice of environment variables in key=value format and
// builds a slice of "--keep" arguments that tell nix-shell which ones to
// keep.
//
// See envToKeep for the full set of kept environment variables.
func toKeepArgs(env []string) []string {
	args := make([]string, 0, len(envToKeep)*2)
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		if envToKeep[key] {
			args = append(args, "--keep", key)
		}
	}
	return args
}

// splitNixList splits and cleans a list of space-delimited paths. It is similar
// to filepath.SplitList for Nix environment variables, which do not use
// filepath.ListSeparator.
func splitNixList(s string) []string {
	split := strings.Fields(s)
	for i, dir := range split {
		split[i] = filepath.Clean(dir)
	}
	return split
}

// cleanEnvPath takes a string formatted as a shell PATH and cleans it for
// passing to nix-shell. It does the following rules for each entry:
//
//  1. Applies filepath.Clean.
//  2. Removes the path if it's relative (must begin with '/' and not be '.').
//  3. Removes the path if it's a descendant of a Nix profile directory.
func cleanEnvPath(pathEnv string, nixProfileDirs []string) string {
	split := filepath.SplitList(pathEnv)
	if len(split) == 0 {
		return ""
	}

	cleaned := make([]string, 0, len(split))
	for _, path := range split {
		path = filepath.Clean(path)
		if path == "." || path[0] != '/' {
			// Don't allow relative paths.
			continue
		}

		keep := true
		for _, profileDir := range nixProfileDirs {
			if strings.HasPrefix(path, profileDir) {
				keep = false
				break
			}
		}
		if keep {
			cleaned = append(cleaned, path)
		}
	}
	return strings.Join(cleaned, string(filepath.ListSeparator))
}