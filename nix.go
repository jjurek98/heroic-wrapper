package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Nixpkgs' heroic is a buildFHSEnv launcher, not just an Electron wrapper.
// Its container-init builds the 32/64-bit ld cache, then execs /init with the
// original argv. Reuse that installed launcher and change ONLY its /init
// symlink to the FHS Bash. The replacement Bash sources the same /etc/profile
// before running the game (and Comet), rather than opening Electron.
//
// No nix evaluation, store scanning, generated files or launcher execution is
// needed by 'show'. The printed command performs the adaptation in memory.
// See nixpkgs/nixos-26.05:
//
//	pkgs/by-name/he/heroic/package.nix
//	pkgs/build-support/build-fhsenv-bubblewrap/{default.nix,container-init.cc}
type nixRuntime struct {
	launcher, shell, initBinding string
}

const maxNixScriptSize = 1024 * 1024

func readNixScript(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > maxNixScriptSize {
		return "", fmt.Errorf("not a small regular shell script: %s", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxNixScriptSize+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxNixScriptSize || !strings.HasPrefix(string(data), "#!") || strings.ContainsRune(string(data), '\x00') {
		return "", fmt.Errorf("not a supported shell script: %s", path)
	}
	return string(data), nil
}

// Also accepts a relocated .../nix/store for fixtures. Store item names cannot
// contain shell whitespace; no path obtained here is evaluated as shell code.
func nixStoreItem(path string) string {
	const marker = "/nix/store/"
	i := strings.Index(path, marker)
	if i < 0 {
		return ""
	}
	end := i + len(marker)
	name := strings.SplitN(path[end:], "/", 2)[0]
	if name == "" || name == "." || name == ".." {
		return ""
	}
	return path[:end] + name
}

// HEROIC_BIN chooses the *installed Heroic launcher*, not heroic-wrapper.
// Prefer PATH, with normal Nix profile locations as a fallback for GUI sessions.
func installedHeroic() (string, error) {
	var candidates []string
	if override := os.Getenv("HEROIC_BIN"); override != "" {
		p, ok := executablePath(override)
		if !ok {
			return "", fmt.Errorf("HEROIC_BIN points to %q, which is not an executable", override)
		}
		candidates = []string{p}
	} else if p, err := exec.LookPath("heroic"); err == nil {
		candidates = []string{p}
	} else {
		home, _ := os.UserHomeDir()
		state := nonempty(os.Getenv("XDG_STATE_HOME"), filepath.Join(home, ".local", "state"))
		candidates = []string{
			filepath.Join(home, ".nix-profile", "bin", "heroic"),
			filepath.Join(state, "nix", "profiles", "profile", "bin", "heroic"),
			"/run/current-system/sw/bin/heroic",
		}
	}
	for _, candidate := range candidates {
		if p, ok := executablePath(candidate); ok {
			resolved, err := filepath.EvalSymlinks(p)
			if err != nil {
				return "", fmt.Errorf("resolve Heroic launcher: %w", err)
			}
			return resolved, nil
		}
	}
	return "", nil
}

var nixInitBinding = regexp.MustCompile(`(?m)^[\t ]*(--symlink[\t ]+(/[^\s"'\\]+)[\t ]+/init)[\t ]*\\?[\t ]*$`)

func inspectNixRuntime(path string) (*nixRuntime, error) {
	script, err := readNixScript(path)
	if err != nil {
		return nil, err
	}
	// Fail closed on an unwrapped Electron launcher or a changed wrapper format.
	// Never execute a script to find out whether it is an FHS environment.
	for _, marker := range []string{"/bin/bwrap", "--bind /nix /nix", "container-init", `exec "${cmd[@]}"`} {
		if !strings.Contains(script, marker) {
			return nil, fmt.Errorf("%s is not a supported nixpkgs Heroic FHS launcher (missing %q)", path, marker)
		}
	}
	bindings := nixInitBinding.FindAllStringSubmatch(script, -1)
	if len(bindings) != 1 || nixStoreItem(bindings[0][2]) == "" {
		return nil, fmt.Errorf("%s must contain exactly one Nix store /init symlink", path)
	}
	initScript, err := readNixScript(bindings[0][2])
	if err != nil {
		return nil, fmt.Errorf("read Heroic FHS init: %w", err)
	}
	// This is the runScript used by pkgs.heroic, including Heroic package
	// overrides for extraPkgs/extraLibraries/extraEnv. Custom runScripts are
	// deliberately rejected instead of silently dropping their behavior.
	var initLines []string
	for _, line := range strings.Split(initScript, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			initLines = append(initLines, line)
		}
	}
	if strings.Join(initLines, "\n") != "source /etc/profile\nexec heroic \"$@\"" {
		return nil, errors.New("unsupported Heroic FHS init; expected nixpkgs runScript = \"heroic\"")
	}
	shebang := strings.TrimSpace(strings.SplitN(script, "\n", 2)[0][2:])
	if filepath.Base(shebang) != "bash" || !filepath.IsAbs(shebang) {
		return nil, fmt.Errorf("unsupported Heroic FHS shell: %q", shebang)
	}
	shell, ok := executablePath(shebang)
	if !ok {
		return nil, fmt.Errorf("Heroic FHS Bash is not executable: %s", shebang)
	}
	return &nixRuntime{launcher: path, shell: shell, initBinding: bindings[0][1]}, nil
}

func resolveNixRuntime() (*nixRuntime, error) {
	mode := nonempty(os.Getenv("HEROIC_NIX_RUNTIME"), "auto")
	if mode == "off" {
		return nil, nil
	}
	if mode != "auto" && mode != "required" {
		return nil, fmt.Errorf("invalid HEROIC_NIX_RUNTIME=%q (expected auto, required or off)", mode)
	}
	path, err := installedHeroic()
	if err != nil {
		return nil, err
	}
	if path == "" || nixStoreItem(path) == "" {
		if mode == "required" {
			return nil, errors.New("no Nix Heroic launcher found; set HEROIC_BIN to the installed pkgs.heroic executable")
		}
		return nil, nil
	}
	env, err := inspectNixRuntime(path)
	if err != nil {
		return nil, fmt.Errorf("Nix Heroic runtime: %w; select the wrapped pkgs.heroic with HEROIC_BIN (not heroic-unwrapped), or set HEROIC_NIX_RUNTIME=off to manage the runtime yourself", err)
	}
	return env, nil
}

func (r *nixRuntime) wrap(command string) string {
	// Keep the original immutable store path instead of following a profile
	// symlink again when the user eventually executes the printed command.
	// Check the binding again to fail clearly if that store path disappears or
	// no longer has the format inspected by 'show'. Quoting is Bash-safe at both
	// levels, even when game paths/arguments contain substitutions or newlines.
	adapter := fmt.Sprintf(`set -e
heroic_nix_script=$(<%s)
heroic_nix_old=%s
heroic_nix_new='--symlink /bin/bash /init'
if [[ $heroic_nix_script != *"$heroic_nix_old"* ]]; then
  printf 'heroic-wrapper: installed Nix launcher changed; regenerate the command with show\n' >&2
  exit 1
fi
heroic_nix_script=${heroic_nix_script/"$heroic_nix_old"/"$heroic_nix_new"}
exec %s -c "$heroic_nix_script" heroic-wrapper -c "$1"
`, shellQuote(r.launcher), shellQuote(r.initBinding), shellQuote(r.shell))
	// Do not use a login shell: use exactly the profile that nixpkgs' original
	// /init sources. In particular this sets up the graphics driver environment.
	payload := "source /etc/profile || exit $?\n" + command
	return shellJoin([]string{r.shell, "-c", adapter, "heroic-wrapper", payload})
}

var nixAbsolutePath = regexp.MustCompile("/[^\\s\"'\\\\$`;(){}<>]+")

// Follow only static store references in the selected launcher's scripts and
// the known bin/heroic entries in its FHS roots. This handles profile symlinks,
// ...-heroic-*-bwrap -> ...-fhs -> heroic-unwrapped, and the unwrapped wrapper's
// --suffix PATH reference to umu-launcher, without querying Nix or starting GUI
// processes. Bounded traversal intentionally avoids scanning /nix/store.
func nixHeroicRoots() []string {
	launcher, err := installedHeroic()
	if err != nil || nixStoreItem(launcher) == "" {
		return nil
	}
	queue := []string{launcher}
	seenScripts, seenRoots := map[string]bool{}, map[string]bool{}
	var roots []string
	for len(queue) > 0 && len(seenScripts) < 16 && len(roots) < 256 {
		path := queue[0]
		queue = queue[1:]
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil || nixStoreItem(resolved) == "" || seenScripts[resolved] {
			continue
		}
		seenScripts[resolved] = true
		paths := []string{resolved}
		if data, err := readNixScript(resolved); err == nil {
			paths = append(paths, nixAbsolutePath.FindAllString(data, -1)...)
		}
		for _, path := range paths {
			root := nixStoreItem(path)
			if root == "" || seenRoots[root] {
				continue
			}
			seenRoots[root] = true
			roots = append(roots, root)
			for _, bin := range []string{"bin/heroic", "usr/bin/heroic"} {
				candidate := filepath.Join(root, filepath.FromSlash(bin))
				if _, ok := executablePath(candidate); ok {
					queue = append(queue, candidate)
				}
			}
			if len(roots) == 256 {
				break
			}
		}
	}
	return roots
}

func nixHeroicAppDirs() []string {
	var dirs []string
	for _, root := range nixHeroicRoots() {
		for _, rel := range []string{"opt/heroic", "usr/opt/heroic", "share/heroic", "usr/share/heroic", "lib/heroic", "usr/lib/heroic"} {
			dir := filepath.Join(root, filepath.FromSlash(rel))
			if info, err := os.Stat(dir); err == nil && info.IsDir() {
				dirs = append(dirs, dir)
			}
		}
	}
	return dirs
}

func nixHeroicUMU() string {
	for _, root := range nixHeroicRoots() {
		for _, rel := range []string{"bin/umu-run", "usr/bin/umu-run"} {
			if path, ok := executablePath(filepath.Join(root, filepath.FromSlash(rel))); ok {
				return path
			}
		}
	}
	return ""
}
