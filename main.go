// heroic-wrapper: Heroic game discovery, launch-command generation and terminal UI.
// Linux. Discovery is read-only; the interactive UI launches installed games.
// Build: go build -o heroic-wrapper .
// Usage: heroic-wrapper | heroic-wrapper list | heroic-wrapper show [--direct] [--no-comet] 'Game Title' | heroic-wrapper where gogdl|legendary|comet|nix
// Override Heroic's config directory with HEROIC_CONFIG_DIR=/path/to/heroic.
// Override runner paths with HEROIC_LEGENDARY_BIN, HEROIC_GOGDL_BIN, HEROIC_COMET_BIN, HEROIC_UMU_BIN.
// Native packaged Heroic includes Legendary and gogdl; those are preferred to $PATH.
// Nix: reuse the installed pkgs.heroic FHS runtime (HEROIC_BIN selects its launcher).
// HEROIC_NIX_RUNTIME=auto|required|off controls Nix runtime wrapping.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

type object = map[string]any

type game struct {
	title, id, store, root, directory, executable, platform, browserURL, folder string
	install                                                                     object
}

func obj(v any) object {
	m, _ := v.(map[string]any)
	if m == nil {
		return object{}
	}
	return m
}
func str(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	case float64:
		return fmt.Sprintf("%.0f", x)
	default:
		return ""
	}
}
func boolean(m object, key string, fallback bool) bool {
	if v, ok := m[key].(bool); ok {
		return v
	}
	return fallback
}
func nonempty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
func arr(v any) []any {
	a, _ := v.([]any)
	return a
}
func expand(s string) string {
	home, _ := os.UserHomeDir()
	if s == "~" {
		return home
	}
	if strings.HasPrefix(s, "~/") {
		return filepath.Join(home, s[2:])
	}
	return s
}
func readJSON(path string) (any, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return value, nil
}
func jsonObject(path string) (object, error) {
	v, err := readJSON(path)
	return obj(v), err
}
func configRoots() []string {
	if v := os.Getenv("HEROIC_CONFIG_DIR"); v != "" {
		return []string{expand(v)}
	}
	home, _ := os.UserHomeDir()
	xdg := os.Getenv("XDG_CONFIG_HOME")
	if xdg == "" {
		xdg = filepath.Join(home, ".config")
	}
	candidates := []string{
		filepath.Join(xdg, "heroic"),
		filepath.Join(home, ".var", "app", "com.heroicgameslauncher.hgl", "config", "heroic"),
	}
	var out []string
	for _, root := range candidates {
		if info, err := os.Stat(root); err == nil && info.IsDir() {
			out = append(out, root)
		}
	}
	return out
}
func epicMetadata(root, id string) (string, string, string) {
	metadata := filepath.Join(root, "legendaryConfig", "legendary", "metadata", id+".json")
	info, _ := jsonObject(metadata)
	m := obj(info["metadata"])
	attributes := obj(m["customAttributes"])
	folder := str(obj(attributes["FolderName"])["value"])
	managed := strings.ToLower(nonempty(str(obj(attributes["ThirdPartyManagedApp"])["value"]), str(obj(attributes["ThirdPartyManagedProvider"])["value"])))
	return nonempty(str(m["title"]), str(info["title"])), folder, managed
}
func gogInfoTitle(dir, id string) string {
	info, _ := jsonObject(filepath.Join(dir, "goggame-"+id+".info"))
	return nonempty(str(info["name"]), str(info["gameName"]))
}
func discover(root string) ([]game, error) {
	var out []game
	epic, err := jsonObject(filepath.Join(root, "legendaryConfig", "legendary", "installed.json"))
	if err != nil {
		return nil, err
	}
	for id, value := range epic {
		v := obj(value)
		if boolean(v, "is_dlc", false) {
			continue
		}
		dir := expand(str(v["install_path"]))
		if dir == "" {
			continue
		}
		title, folder, managed := epicMetadata(root, id)
		if managed == "origin" || managed == "the ea app" {
			v["isEAManaged"] = true
		}
		if managed == "ubisoftconnect" {
			v["isUbisoftManaged"] = true
		}
		out = append(out, game{
			title: nonempty(title, str(v["title"]), filepath.Base(dir), id), folder: nonempty(folder, id),
			id: id, store: "epic", root: root, directory: dir,
			executable: str(v["executable"]), platform: nonempty(str(v["platform"]), "Windows"), install: v,
		})
	}

	gogCache, err := jsonObject(filepath.Join(root, "store_cache", "gog_library.json"))
	if err != nil {
		return nil, err
	}
	gogTitles := map[string]string{}
	gogFolders := map[string]string{}
	for _, value := range arr(gogCache["games"]) {
		m := obj(value)
		key := nonempty(str(m["app_name"]), str(m["appName"]))
		gogTitles[key] = str(m["title"])
		gogFolders[key] = str(m["folder_name"])
	}
	installed, err := readJSON(filepath.Join(root, "gog_store", "installed.json"))
	if err != nil {
		return nil, err
	}
	// Electron-store uses {"installed": [...]}; also accept older plain array form.
	installs := arr(obj(installed)["installed"])
	if _, ok := installed.([]any); ok {
		installs = arr(installed)
	}
	for _, value := range installs {
		v := obj(value)
		if boolean(v, "is_dlc", false) {
			continue
		}
		id := nonempty(str(v["appName"]), str(v["app_name"]))
		dir := expand(str(v["install_path"]))
		if id == "" || dir == "" {
			continue
		}
		out = append(out, game{
			title: nonempty(gogTitles[id], str(v["title"]), gogInfoTitle(dir, id), filepath.Base(dir), id), folder: nonempty(gogFolders[id], filepath.Base(dir)),
			id: id, store: "gog", root: root, directory: dir,
			executable: str(v["executable"]), platform: nonempty(str(v["platform"]), "windows"), install: v,
		})
	}

	library, err := jsonObject(filepath.Join(root, "sideload_apps", "library.json"))
	if err != nil {
		return nil, err
	}
	for _, value := range arr(library["games"]) {
		v := obj(value)
		inst := obj(v["install"])
		id := nonempty(str(v["app_name"]), str(v["appName"]))
		exe := nonempty(str(inst["executable"]), str(v["executable"]))
		dir := nonempty(str(inst["install_path"]), str(v["folder_name"]))
		exe, dir = expand(exe), expand(dir)
		if dir == "" && exe != "" {
			dir = filepath.Dir(exe)
		}
		if id == "" {
			continue
		}
		out = append(out, game{
			title: nonempty(str(v["title"]), str(v["name"]), id),
			id:    id, store: "sideload", root: root, directory: dir,
			executable: exe, platform: nonempty(str(inst["platform"]), "Windows"),
			browserURL: str(v["browserUrl"]), install: inst,
		})
	}
	return out, nil
}
func allGames() ([]game, error) {
	roots := configRoots()
	if len(roots) == 0 {
		return nil, errors.New("no Heroic config directory found; set HEROIC_CONFIG_DIR")
	}
	var games []game
	seen := map[string]bool{}
	for _, root := range roots {
		found, err := discover(root)
		if err != nil {
			return nil, err
		}
		for _, g := range found {
			key := g.store + "\x00" + g.id + "\x00" + g.directory
			if !seen[key] {
				games = append(games, g)
				seen[key] = true
			}
		}
	}
	sort.Slice(games, func(i, j int) bool {
		if strings.ToLower(games[i].title) != strings.ToLower(games[j].title) {
			return strings.ToLower(games[i].title) < strings.ToLower(games[j].title)
		}
		if games[i].store != games[j].store {
			return games[i].store < games[j].store
		}
		return games[i].id < games[j].id
	})
	return games, nil
}
func pick(games []game, selector string) (game, error) {
	var matches []game
	for _, g := range games {
		if strings.EqualFold(selector, g.title) ||
			strings.EqualFold(selector, g.store+":"+g.id) ||
			(strings.EqualFold(selector, g.id) && g.id != "") {
			matches = append(matches, g)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) == 0 {
		return game{}, fmt.Errorf("game %q not found (run 'list')", selector)
	}
	var choices []string
	for _, g := range matches {
		choices = append(choices, fmt.Sprintf("%s [%s:%s] at %s", g.title, g.store, g.id, g.root))
	}
	return game{}, fmt.Errorf("ambiguous game name; select by store:id (or set HEROIC_CONFIG_DIR):\n  %s", strings.Join(choices, "\n  "))
}
func configSettings(g game) (object, error) {
	global, err := jsonObject(filepath.Join(g.root, "config.json"))
	if err != nil {
		return nil, err
	}
	// Heroic config.json uses {"defaultSettings": {...}, "version": "v0"}.
	defaults := obj(global["defaultSettings"])
	if len(defaults) == 0 {
		defaults = obj(global["settings"])
	}
	settings := object{}
	for k, v := range defaults {
		settings[k] = v
	}
	cfg, err := jsonObject(filepath.Join(g.root, "GamesConfig", g.id+".json"))
	if err != nil {
		return nil, err
	}
	override := obj(cfg[g.id])
	for k, v := range override {
		settings[k] = v
	}
	return settings, nil
}

// shellQuote keeps paths, environment values, args and even embedded apostrophes
// safe when the user copies the printed string into Bash.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
func shellJoin(args []string) string {
	a := make([]string, 0, len(args))
	for _, v := range args {
		a = append(a, shellQuote(v))
	}
	return strings.Join(a, " ")
}

// Small POSIX-like argv parser for Heroic's shlex-formatted launch arguments.
// It performs NO shell substitutions, globbing, evaluation or filesystem access.
func splitArgs(input string) ([]string, error) {
	var result []string
	var b strings.Builder
	quote := rune(0)
	started := false
	chars := []rune(input)
	for i := 0; i < len(chars); i++ {
		c := chars[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			} else {
				b.WriteRune(c)
			}
		case quote == '"':
			if c == '"' {
				quote = 0
				continue
			}
			if c == '\\' && i+1 < len(chars) {
				next := chars[i+1]
				if strings.ContainsRune("\\\"$`", next) {
					i++
					b.WriteRune(next)
					continue
				}
			}
			b.WriteRune(c)
		case c == '\'' || c == '"':
			quote, started = c, true
		case c == '\\':
			if i+1 >= len(chars) {
				return nil, errors.New("trailing backslash in launch arguments")
			}
			i++
			b.WriteRune(chars[i])
			started = true
		case c == ' ' || c == '\t' || c == '\n':
			if started {
				result = append(result, b.String())
				b.Reset()
				started = false
			}
		default:
			b.WriteRune(c)
			started = true
		}
	}
	if quote != 0 {
		return nil, errors.New("unclosed quote in launch arguments")
	}
	if started {
		result = append(result, b.String())
	}
	return result, nil
}
func appendSplit(args []string, input string) ([]string, error) {
	more, err := splitArgs(input)
	return append(args, more...), err
}
func validEnvName(s string) bool {
	for i, r := range s {
		if !((r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || r == '_' || (i != 0 && r >= '0' && r <= '9')) {
			return false
		}
	}
	return len(s) != 0
}
func preferredBinary(envName, binary string) string {
	if v := os.Getenv(envName); v != "" {
		return expand(v)
	}
	if v, err := exec.LookPath(binary); err == nil {
		return v
	}
	if envName == "HEROIC_UMU_BIN" {
		if v := nixHeroicUMU(); v != "" {
			return v
		}
	}
	return binary // shell will resolve from PATH when the user runs it
}
func pathExists(p string) bool { _, err := os.Stat(p); return err == nil }

// runnerLocation is a host-visible executable path, not the path inside a
// Flatpak sandbox. The source is only used by the 'where' diagnostics.
type runnerLocation struct {
	path, source, note string
}

func executablePath(path string) (string, bool) {
	path = expand(strings.TrimSpace(path))
	if path == "" {
		return "", false
	}
	if !strings.ContainsRune(path, os.PathSeparator) {
		resolved, err := exec.LookPath(path)
		if err != nil {
			return "", false
		}
		path = resolved
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	info, err := os.Stat(abs)
	return abs, err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0
}

func runnerArchitectures() []string {
	arch := runtime.GOARCH
	switch arch {
	case "amd64":
		arch = "x64" // Heroic uses Electron's process.arch, not Go's GOARCH
	case "386":
		arch = "ia32"
	}
	if arch == "x64" {
		return []string{arch}
	}
	return []string{arch, "x64"} // Same fallback as Heroic
}

// Find within known Heroic application layouts; do not recurse over the whole disk.
func runnerUnderAppDir(root, binary string) string {
	bases := []string{
		filepath.Join(root, "resources", "app.asar.unpacked", "build", "bin"),
		filepath.Join(root, "app.asar.unpacked", "build", "bin"),
		filepath.Join(root, "build", "bin"),
		filepath.Join(root, "usr", "share", "heroic", "resources", "app.asar.unpacked", "build", "bin"),
		filepath.Join(root, "usr", "lib", "heroic", "resources", "app.asar.unpacked", "build", "bin"),
	}
	for _, base := range bases {
		for _, arch := range runnerArchitectures() {
			if p, ok := executablePath(filepath.Join(base, arch, "linux", binary)); ok {
				return p
			}
		}
		// Older builds lack the architecture directory.
		if p, ok := executablePath(filepath.Join(base, "linux", binary)); ok {
			return p
		}
	}
	return ""
}

func flatpakInstallLocations() []string {
	home, _ := os.UserHomeDir()
	locations := []string{}
	if flatpak, err := exec.LookPath("flatpak"); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		output, err := exec.CommandContext(ctx, flatpak, "info", "--show-location", "com.heroicgameslauncher.hgl").Output()
		if err == nil {
			location := strings.TrimSpace(string(output))
			if filepath.IsAbs(location) {
				locations = append(locations, location)
			}
		}
	}
	// Flatpak may be installed without its CLI in this execution environment.
	// These are normal symlinks to the active per-user/system installation.
	for _, base := range []string{
		filepath.Join(home, ".local", "share", "flatpak"),
		"/var/lib/flatpak",
	} {
		for _, arch := range []string{runtime.GOARCH, "x86_64", "aarch64"} {
			if arch == "amd64" {
				arch = "x86_64"
			}
			if arch == "arm64" {
				arch = "aarch64"
			}
			locations = append(locations,
				filepath.Join(base, "app", "com.heroicgameslauncher.hgl", arch, "stable", "active"))
		}
	}
	return locations
}

// /app/... is only meaningful in the sandbox. Translate to the Flatpak's
// *host-side* installation path without copying/extracting any files.
func flatpakHostPath(sandboxPath string) string {
	if !strings.HasPrefix(sandboxPath, "/app/") {
		return ""
	}
	relative := strings.TrimPrefix(sandboxPath, "/app/")
	for _, location := range flatpakInstallLocations() {
		candidate := filepath.Join(location, "files", relative)
		if p, ok := executablePath(candidate); ok {
			return p
		}
	}
	return ""
}

func flatpakBundledRunner(binary string) string {
	for _, location := range flatpakInstallLocations() {
		for _, appDir := range []string{
			filepath.Join(location, "files", "bin", "heroic"),
			filepath.Join(location, "files", "share", "heroic"),
			filepath.Join(location, "files", "lib", "heroic"),
		} {
			if p := runnerUnderAppDir(appDir, binary); p != "" {
				return p
			}
		}
	}
	return ""
}

func isFlatpakExecutable(path string) bool {
	return strings.Contains(path, "/flatpak/app/com.heroicgameslauncher.hgl/")
}

func asRunnerLocation(path, source string) runnerLocation {
	location := runnerLocation{path: path, source: source}
	if isFlatpakExecutable(path) {
		location.note = "Using Heroic's Flatpak-bundled runner by its host path; it may need Flatpak runtime libraries not available on the host. If execution fails, install a host-compatible gogdl/legendary and set HEROIC_GOGDL_BIN/HEROIC_LEGENDARY_BIN."
	}
	if strings.HasPrefix(path, "/tmp/.mount_") {
		location.note = "Using a running AppImage's temporary mount; path only works while its AppImage remains mounted."
	}
	return location
}

// An explicit setting with /app/... may come from the Flatpak configuration;
// map it to the host's readable path if possible.
func specifiedRunnerPath(path, source string) (runnerLocation, error) {
	path = expand(path)
	if p, ok := executablePath(path); ok {
		return asRunnerLocation(p, source), nil
	}
	if p := flatpakHostPath(path); p != "" {
		return asRunnerLocation(p, source+" (Flatpak path translated to host)"), nil
	}
	return runnerLocation{}, fmt.Errorf("%s points to %q, which is not an executable on the host", source, path)
}

func candidateAppDirs() []string {
	home, _ := os.UserHomeDir()
	dirs := []string{}
	if custom := os.Getenv("HEROIC_INSTALL_DIR"); custom != "" {
		dirs = append(dirs, expand(custom))
	}
	dirs = append(dirs, nixHeroicAppDirs()...)
	dirs = append(dirs,
		"/opt/Heroic", "/opt/heroic",
		"/usr/lib/heroic-games-launcher-bin", "/usr/lib64/heroic-games-launcher-bin",
		"/usr/lib/heroic-games-launcher", "/usr/lib64/heroic-games-launcher",
		"/usr/lib/heroic", "/usr/lib64/heroic", "/usr/share/heroic",
		"/usr/share/heroic-games-launcher",
		filepath.Join(home, ".local", "opt", "Heroic"),
		filepath.Join(home, "Applications", "Heroic"),
	)
	if mounts, _ := filepath.Glob("/tmp/.mount_*"); len(mounts) > 0 {
		for _, dir := range mounts {
			if strings.Contains(strings.ToLower(filepath.Base(dir)), "heroic") {
				dirs = append(dirs, dir)
			}
		}
	}
	// Inspect *running* Heroic executables so portable/native installations
	// at arbitrary paths work without scanning the entire filesystem.
	if processes, err := os.ReadDir("/proc"); err == nil {
		for _, process := range processes {
			if !process.IsDir() || process.Name() == "" || process.Name()[0] < '0' || process.Name()[0] > '9' {
				continue
			}
			path, err := os.Readlink(filepath.Join("/proc", process.Name(), "exe"))
			if err != nil {
				continue
			}
			name := strings.ToLower(filepath.Base(path))
			if strings.Contains(name, "heroic") && !strings.Contains(name, "heroic-wrapper") {
				dirs = append(dirs, filepath.Dir(path))
			}
		}
	}
	return dirs
}

func logLocations(root string) []string {
	home, _ := os.UserHomeDir()
	nativeState := os.Getenv("XDG_STATE_HOME")
	if nativeState == "" {
		nativeState = filepath.Join(home, ".local", "state")
	}
	basePaths := []string{
		filepath.Join(nativeState, "Heroic", "logs"),
		filepath.Join(home, ".local", "state", "Heroic", "logs"),
		filepath.Join(home, ".var", "app", "com.heroicgameslauncher.hgl", ".local", "state", "Heroic", "logs"),
		filepath.Join(root, "logs"), // older releases
	}
	var result []string
	for _, base := range basePaths {
		result = append(result,
			filepath.Join(base, "heroic.log"), filepath.Join(base, "heroic.log.old"),
			filepath.Join(base, "runners", "gog.log"), filepath.Join(base, "runners", "legendary.log"))
	}
	return result
}

// Heroic logs the exact runner path on startup. This also handles custom
// installation prefixes (Nix, portable folders, etc.) without deep scanning.
func pathsFromHeroicLog(path, binary string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	if stat, err := f.Stat(); err == nil && stat.Size() > 4*1024*1024 {
		_, _ = f.Seek(-4*1024*1024, io.SeekEnd)
	}
	data, _ := io.ReadAll(io.LimitReader(f, 4*1024*1024))
	lines := strings.Split(string(data), "\n")
	var results []string
	for i := len(lines) - 1; i >= 0; i-- {
		line := lines[i]
		for _, marker := range []string{binary + " location:", strings.ToUpper(binary) + " location:"} {
			if pos := strings.Index(strings.ToLower(line), strings.ToLower(marker)); pos >= 0 {
				value := strings.Trim(strings.TrimSpace(line[pos+len(marker):]), "\"'")
				if filepath.IsAbs(value) {
					results = append(results, value)
				}
			}
		}
		if !strings.Contains(line, "/"+binary) {
			continue
		}
		// Heroic's command logs generally contain a space-delimited executable
		// right before ' --auth-config-path', '--version', 'launch', etc.
		if pos := strings.Index(line, "Running command:"); pos >= 0 {
			line = line[pos+len("Running command:"):]
		}
		if tokens, err := splitArgs(line); err == nil {
			for _, token := range tokens {
				if strings.HasSuffix(token, "/"+binary) && filepath.IsAbs(token) {
					results = append(results, token)
				}
			}
		}
	}
	return results
}

// Heroic's actual preference is override -> configured binary -> bundled.
// A host-side CLI cannot access Electron's runtime __dirname, so we additionally
// inspect Heroic's logs, running processes, and Flatpak's on-disk installation.
// Never return a bare gogdl/legendary: 'show' must be pasteable and meaningful.
func resolveRunner(g game, envName, configKey, binary string) (runnerLocation, error) {
	if override := os.Getenv(envName); override != "" {
		return specifiedRunnerPath(override, envName)
	}
	if cfg, err := jsonObject(filepath.Join(g.root, "config.json")); err == nil {
		defaults := obj(cfg["defaultSettings"])
		if len(defaults) == 0 {
			defaults = obj(cfg["settings"])
		}
		if path := str(defaults[configKey]); path != "" {
			return specifiedRunnerPath(path, "Heroic config.json "+configKey)
		}
	}
	for _, log := range logLocations(g.root) {
		for _, candidate := range pathsFromHeroicLog(log, binary) {
			if loc, err := specifiedRunnerPath(candidate, "Heroic log "+log); err == nil {
				return loc, nil
			}
		}
	}
	for _, dir := range candidateAppDirs() {
		if p := runnerUnderAppDir(dir, binary); p != "" {
			return asRunnerLocation(p, "Heroic bundled runner"), nil
		}
	}
	if p, ok := executablePath(binary); ok {
		return asRunnerLocation(p, "host $PATH"), nil
	}
	if p := flatpakBundledRunner(binary); p != "" {
		return asRunnerLocation(p, "Flatpak installation (host path)"), nil
	}
	return runnerLocation{}, fmt.Errorf(
		"cannot find Heroic's %s binary. Use %s=/absolute/path/to/%s; if Heroic is a Flatpak, inspect: flatpak info --show-location com.heroicgameslauncher.hgl",
		binary, envName, binary)
}

// gogDirectTask selects the same primary playTask that gogdl launch selects,
// without invoking gogdl or its cloud-save APIs. The .info file is installed
// alongside the game itself (goggame-<productid>.info).
func gogDirectTask(g game, exeOverride string) (executable, workingDirectory string, taskArgs []string, err error) {
	infoPath := filepath.Join(g.directory, "goggame-"+g.id+".info")
	info, err := jsonObject(infoPath)
	if err != nil {
		return "", "", nil, err
	}
	tasks := arr(info["playTasks"])
	if len(tasks) == 0 {
		return "", "", nil, fmt.Errorf("no playTasks in %s; gogdl would be unable to select a primary executable", infoPath)
	}
	var primary object
	for _, raw := range tasks {
		task := obj(raw)
		if boolean(task, "isPrimary", false) {
			primary = task
			break
		}
	}
	if primary == nil {
		return "", "", nil, fmt.Errorf("no primary playTask in %s", infoPath)
	}

	workingDirectory = g.directory
	if cwd := str(primary["workingDir"]); cwd != "" {
		workingDirectory, err = resolveGogPath(g.directory, cwd)
		if err != nil {
			return "", "", nil, fmt.Errorf("resolve GOG workingDir: %w", err)
		}
	}
	executable, err = resolveGogPath(g.directory, str(primary["path"]))
	if err != nil {
		return "", "", nil, fmt.Errorf("resolve GOG executable: %w", err)
	}
	if override := strings.TrimSpace(exeOverride); override != "" {
		if filepath.IsAbs(override) {
			executable = expand(override)
		} else {
			executable, err = resolveGogPath(g.directory, override)
			if err != nil {
				return "", "", nil, fmt.Errorf("resolve executable override: %w", err)
			}
		}
		workingDirectory = filepath.Dir(executable) // matches gogdl --override-exe
	}
	if !pathExists(executable) {
		return "", "", nil, fmt.Errorf("resolved executable %q does not exist", executable)
	}
	if !pathExists(workingDirectory) {
		return "", "", nil, fmt.Errorf("resolved working directory %q does not exist", workingDirectory)
	}

	switch args := primary["arguments"].(type) {
	case nil:
	case string:
		// gogdl converts Windows backslashes before shlex.split().
		taskArgs, err = splitArgs(strings.ReplaceAll(args, `\`, `/`))
		if err != nil {
			return "", "", nil, fmt.Errorf("parse GOG playTask arguments: %w", err)
		}
	case []any:
		for _, arg := range args {
			word, ok := arg.(string)
			if !ok {
				return "", "", nil, fmt.Errorf("GOG playTask arguments contains non-string value")
			}
			taskArgs = append(taskArgs, word)
		}
	default:
		return "", "", nil, fmt.Errorf("unsupported GOG playTask arguments type %T", primary["arguments"])
	}
	return executable, workingDirectory, taskArgs, nil
}

// Resolve paths encoded with Windows separators and tolerate Linux filename case
// differences the same way gogdl's get_case_insensitive_name helper does.
func resolveGogPath(root, relative string) (string, error) {
	if strings.TrimSpace(relative) == "" {
		return "", errors.New("empty path")
	}
	path := filepath.Join(root, filepath.FromSlash(strings.ReplaceAll(relative, `\`, `/`)))
	if pathExists(path) {
		return path, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	parts := strings.Split(strings.TrimPrefix(filepath.Clean(abs), string(os.PathSeparator)), string(os.PathSeparator))
	current := string(os.PathSeparator)
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		entries, err := os.ReadDir(current)
		if err != nil {
			return "", fmt.Errorf("cannot resolve %q: %w", path, err)
		}
		exact, folded := "", ""
		for _, item := range entries {
			if item.Name() == part {
				exact = item.Name()
				break
			}
			if strings.EqualFold(item.Name(), part) {
				folded = item.Name()
			}
		}
		if exact == "" {
			exact = folded
		}
		if exact == "" {
			return "", fmt.Errorf("%q not found (component %q)", path, part)
		}
		current = filepath.Join(current, exact)
	}
	return current, nil
}

// cometInfo returns Heroic's per-GOG-account username and whether Comet
// integration is enabled in Heroic's global settings. Cloud-save auto-sync
// is entirely separate and deliberately not used here.
func cometInfo(g game) (string, bool, error) {
	if g.store != "gog" {
		return "", false, nil
	}
	global, err := jsonObject(filepath.Join(g.root, "config.json"))
	if err != nil {
		return "", false, err
	}
	defaults := obj(global["defaultSettings"])
	if len(defaults) == 0 {
		defaults = obj(global["settings"])
	}
	if !boolean(obj(defaults["experimentalFeatures"]), "cometSupport", true) {
		return "", false, nil
	}
	gogConfig, err := jsonObject(filepath.Join(g.root, "gog_store", "config.json"))
	if err != nil {
		return "", false, err
	}
	username := str(obj(gogConfig["userData"])["username"])
	return username, username != "", nil
}

func buildCommand(g game, direct, disableComet bool) (string, error) {
	var nixEnv *nixRuntime
	var err error
	if g.browserURL == "" && !strings.EqualFold(g.platform, "browser") {
		nixEnv, err = resolveNixRuntime()
		if err != nil {
			return "", err
		}
	}
	cfg, err := configSettings(g)
	if err != nil {
		return "", err
	}
	warnings := []string{}
	var comet runnerLocation
	var cometUsername string
	if !disableComet && g.store == "gog" {
		username, enabled, err := cometInfo(g)
		if err != nil {
			return "", fmt.Errorf("read Heroic's Comet settings: %w", err)
		}
		if enabled {
			cometUsername = username
			comet, err = resolveRunner(g, "HEROIC_COMET_BIN", "altCometBin", "comet")
			if err != nil {
				return "", fmt.Errorf("Heroic starts Comet for this GOG account, but %w", err)
			}
			if comet.note != "" {
				warnings = append(warnings, comet.note)
			}
		}
	}
	if direct && g.store != "gog" {
		return "", errors.New("--direct currently supports GOG games only")
	}
	var runner runnerLocation
	if g.store == "epic" || (g.store == "gog" && !direct) {
		if g.store == "epic" {
			runner, err = resolveRunner(g, "HEROIC_LEGENDARY_BIN", "altLegendaryBin", "legendary")
		} else {
			runner, err = resolveRunner(g, "HEROIC_GOGDL_BIN", "altGogdlBin", "gogdl")
		}
		if err != nil {
			return "", err
		}
		if runner.note != "" {
			warnings = append(warnings, runner.note)
		}
	}
	gameArgs, err := splitArgs(str(cfg["launcherArgs"]))
	if err != nil {
		return "", fmt.Errorf("launcherArgs: %w", err)
	}
	launchOpt := obj(cfg["lastUsedLaunchOption"])
	if t := str(launchOpt["type"]); t == "" || t == "basic" {
		gameArgs, err = appendSplit(nil, str(launchOpt["parameters"]))
		if err != nil {
			return "", fmt.Errorf("lastUsedLaunchOption: %w", err)
		}
		trailing, _ := splitArgs(str(cfg["launcherArgs"]))
		gameArgs = append(gameArgs, trailing...)
	}
	exe := nonempty(str(cfg["targetExe"]), g.executable)
	if str(launchOpt["type"]) == "altExe" && str(launchOpt["executable"]) != "" {
		exe = str(launchOpt["executable"])
	}
	if g.store == "sideload" && exe != "" && !filepath.IsAbs(exe) {
		exe = filepath.Join(g.directory, exe)
	}
	workingDirectory := g.directory
	if g.store == "sideload" && exe != "" && g.browserURL == "" {
		workingDirectory = filepath.Dir(exe) // Heroic sideload launch uses executable's directory.
	}
	native := strings.EqualFold(g.platform, "linux") || strings.EqualFold(g.platform, "browser")
	var directExe string
	var directTaskArgs []string
	if direct {
		if native {
			return "", errors.New("--direct currently supports Windows GOG games only")
		}
		// gogdl uses --override-exe only when Heroic requested an override.
		override := ""
		if exe != "" && exe != g.executable {
			override = exe
		}
		directExe, workingDirectory, directTaskArgs, err = gogDirectTask(g, override)
		if err != nil {
			return "", err
		}
	}
	env := map[string]string{}
	inheritNixLibraryPath, captureNixLibraryPath := false, false
	if direct {
		// These are set by Heroic's actual launch command, but omitted by
		// the legacy heroic-wrapper show. Only add them in the new mode.
		env["HEROIC_APP_NAME"] = g.id
		env["HEROIC_APP_RUNNER"] = "gog"
		env["HEROIC_APP_SOURCE"] = "gog"
		env["LD_PRELOAD"] = ""
	}
	if g.directory != "" {
		env["STEAM_COMPAT_INSTALL_PATH"] = g.directory
	}
	switch g.store {
	case "epic":
		env["LEGENDARY_CONFIG_PATH"] = filepath.Join(g.root, "legendaryConfig", "legendary")
		env["STORE"], env["GAMEID"] = "egs", "umu-0"
	case "gog":
		if !direct {
			env["GOGDL_CONFIG_PATH"] = filepath.Join(g.root, "gogdlConfig")
		}
		env["STORE"], env["GAMEID"] = "gog", "umu-0"
	case "sideload":
		env["GAMEID"] = "umu-0"
	}
	if boolean(cfg, "nvidiaPrime", false) {
		env["DRI_PRIME"], env["__NV_PRIME_RENDER_OFFLOAD"], env["__GLX_VENDOR_LIBRARY_NAME"] = "1", "1", "nvidia"
	}
	if _, ok := os.LookupEnv("LD_PRELOAD"); !ok {
		env["LD_PRELOAD"] = ""
	}
	// Heroic's intentionally misspelled key is "enviromentOptions".
	for _, entry := range arr(cfg["enviromentOptions"]) {
		kv := obj(entry)
		k, v := str(kv["key"]), str(kv["value"])
		if k == "" {
			continue
		}
		if !validEnvName(k) {
			return "", fmt.Errorf("invalid environment variable name %q", k)
		}
		// Heroic strips a matched pair of outer quotes from custom env values.
		if len(v) > 1 && ((v[0] == '\'' && v[len(v)-1] == '\'') || (v[0] == '"' && v[len(v)-1] == '"')) {
			v = v[1 : len(v)-1]
		}
		env[k] = v
	}

	wineInfo := obj(cfg["wineVersion"])
	wineType := strings.ToLower(str(wineInfo["type"]))
	wineBin := expand(str(wineInfo["bin"]))
	if wineType == "" && !native {
		// If no global/per-game runtime was saved, this is only a fallback.
		wineType, wineBin = "wine", preferredBinary("HEROIC_WINE_BIN", "wine")
		warnings = append(warnings, "No saved Wine version found: falling back to host wine; verify Heroic's Wine selection")
	}
	if wineType == "wine" && wineBin == "" {
		wineBin = preferredBinary("HEROIC_WINE_BIN", "wine")
	}
	if wineType == "proton" && wineBin == "" {
		return "", errors.New("selected Proton runtime has no 'bin' path")
	}
	prefix := expand(nonempty(str(cfg["winePrefix"]), filepath.Join(os.Getenv("HOME"), "Games", "Heroic", "Prefixes", "shared")))
	if !native {
		env["WINEPREFIX"] = prefix
		if boolean(cfg, "showFps", false) {
			env["DXVK_HUD"] = "fps"
		}
		if boolean(cfg, "enableDXVKFpsLimit", false) {
			if cap := str(cfg["DXVKFpsCap"]); cap != "" {
				env["DXVK_FRAME_RATE"] = cap
			}
		}
		if boolean(cfg, "enableFSR", false) {
			env["WINE_FULLSCREEN_FSR"] = "1"
			sharpness := str(cfg["maxSharpness"])
			if sharpness == "" {
				sharpness = "2"
			}
			env["WINE_FULLSCREEN_FSR_STRENGTH"] = sharpness
		} else {
			env["WINE_FULLSCREEN_FSR"] = "0"
		}
		if boolean(cfg, "eacRuntime", false) {
			env["PROTON_EAC_RUNTIME"] = filepath.Join(g.root, "tools", "runtimes", "eac_runtime")
		}
		if boolean(cfg, "battlEyeRuntime", false) {
			env["PROTON_BATTLEYE_RUNTIME"] = filepath.Join(g.root, "tools", "runtimes", "battleye_runtime")
		}
		if wineType == "proton" {
			env["STEAM_COMPAT_DATA_PATH"] = prefix
			env["STEAM_COMPAT_CLIENT_INSTALL_PATH"] = filepath.Join(os.Getenv("HOME"), ".steam", "steam")
			env["PROTONPATH"] = filepath.Dir(wineBin)
			env["STEAM_COMPAT_APP_ID"], env["SteamAppId"] = "0", "0"
			env["SteamGameId"] = "heroic-" + nonempty(g.folder, g.id)
			env["PROTON_LOG_DIR"] = os.Getenv("HOME")
			if !boolean(cfg, "enableEsync", true) {
				env["PROTON_NO_ESYNC"] = "1"
			}
			if !boolean(cfg, "enableFsync", true) {
				env["PROTON_NO_FSYNC"] = "1"
			}
			if boolean(cfg, "enableWineWayland", false) {
				env["PROTON_ENABLE_WAYLAND"] = "1"
			}
			if boolean(cfg, "enableWoW64", false) {
				env["PROTON_USE_WOW64"] = "1"
			}
			if boolean(cfg, "enableHDR", false) && boolean(cfg, "enableWineWayland", false) {
				env["PROTON_ENABLE_HDR"] = "1"
			}
			if boolean(cfg, "autoInstallDxvkNvapi", false) {
				env["PROTON_ENABLE_NVAPI"] = "1"
				env["DXVK_NVAPI_ALLOW_OTHER_DRIVERS"] = "1"
			} else {
				env["PROTON_DISABLE_NVAPI"] = "1"
			}
			if boolean(cfg, "verboseLogs", false) {
				for k, v := range map[string]string{"WINEDEBUG": "+fixme", "DXVK_LOG_LEVEL": "info", "VKD3D_DEBUG": "fixme"} {
					if _, ok := env[k]; !ok {
						env[k] = v
					}
				}
			}
		} else {
			if boolean(cfg, "enableEsync", true) {
				env["WINEESYNC"] = "1"
			}
			if boolean(cfg, "enableFsync", true) {
				env["WINEFSYNC"] = "1"
			}
			if boolean(cfg, "enableWineWayland", false) {
				env["DISPLAY"] = ""
			}
			if boolean(cfg, "enableHDR", false) && boolean(cfg, "enableWineWayland", false) {
				env["DXVK_HDR"] = "1"
			}
			if boolean(cfg, "enableWoW64", false) {
				env["WINEARCH"] = "wow64"
			}
			if boolean(cfg, "autoInstallDxvkNvapi", false) {
				env["DXVK_ENABLE_NVAPI"] = "1"
				env["DXVK_NVAPI_ALLOW_OTHER_DRIVERS"] = "1"
			}
			disableMenuBuilder := "winemenubuilder.exe=d"
			dllKey := "WINEDLLOVERRIDES"
			for k := range env {
				if strings.EqualFold(k, dllKey) {
					dllKey = k
					break
				}
			}
			if old := env[dllKey]; old != "" {
				disableMenuBuilder = old + ";" + disableMenuBuilder
			}
			env[dllKey] = disableMenuBuilder
			if !boolean(cfg, "preferSystemLibs", false) {
				lib, lib32 := str(wineInfo["lib"]), str(wineInfo["lib32"])
				var pieces []string
				for _, p := range []string{lib, lib32} {
					if p != "" {
						pieces = append(pieces, p)
					}
				}
				if nixEnv == nil && os.Getenv("LD_LIBRARY_PATH") != "" {
					pieces = append(pieces, os.Getenv("LD_LIBRARY_PATH"))
				}
				if len(pieces) > 0 {
					// Explicit per-game values take precedence. Otherwise append
					// the *launch-time* FHS path, not a host snapshot from 'show'.
					if _, custom := env["LD_LIBRARY_PATH"]; !custom {
						env["LD_LIBRARY_PATH"] = strings.Join(pieces, ":")
						inheritNixLibraryPath = nixEnv != nil
						if _, custom := env["ORIG_LD_LIBRARY_PATH"]; !custom {
							if _, inherited := os.LookupEnv("ORIG_LD_LIBRARY_PATH"); !inherited {
								env["ORIG_LD_LIBRARY_PATH"] = os.Getenv("LD_LIBRARY_PATH")
								captureNixLibraryPath = nixEnv != nil
							}
						}
					}
					if pathExists(filepath.Join(lib, "gstreamer-1.0")) && pathExists(filepath.Join(lib32, "gstreamer-1.0")) {
						env["GST_PLUGIN_SYSTEM_PATH_1_0"] = filepath.Join(lib, "gstreamer-1.0") + ":" + filepath.Join(lib32, "gstreamer-1.0")
					}
					if pathExists(filepath.Join(lib, "wine")) && pathExists(filepath.Join(lib32, "wine")) {
						env["WINEDLLPATH"] = filepath.Join(lib, "wine") + ":" + filepath.Join(lib32, "wine")
					}
				}
			}
		}
	}

	var wrappers []string
	gamescopeActive := false
	if gs := obj(cfg["gamescope"]); boolean(gs, "enableUpscaling", false) || boolean(gs, "enableLimiter", false) {
		gamescopeBin := preferredBinary("HEROIC_GAMESCOPE_BIN", "gamescope")
		if pathExists(gamescopeBin) || toolExists(gamescopeBin) {
			gamescopeActive = true
			help, _ := exec.Command(gamescopeBin, "--help").CombinedOutput()
			oldVersion := !strings.Contains(string(help), "-F, --filter")
			wrappers = append(wrappers, gamescopeBin)
			if boolean(gs, "enableUpscaling", false) {
				for _, flag := range []struct{ key, opt string }{
					{"gameWidth", "-w"}, {"gameHeight", "-h"},
					{"upscaleWidth", "-W"}, {"upscaleHeight", "-H"},
				} {
					if value := str(gs[flag.key]); value != "" {
						wrappers = append(wrappers, flag.opt, value)
					}
				}
				switch str(gs["upscaleMethod"]) {
				case "fsr":
					if oldVersion {
						wrappers = append(wrappers, "-U")
					} else {
						wrappers = append(wrappers, "-F", "fsr")
					}
				case "nis":
					if oldVersion {
						wrappers = append(wrappers, "-Y")
					} else {
						wrappers = append(wrappers, "-F", "nis")
					}
				case "integer":
					if oldVersion {
						wrappers = append(wrappers, "-i")
					} else {
						wrappers = append(wrappers, "-S", "integer")
					}
				case "stretch":
					if !oldVersion {
						wrappers = append(wrappers, "-S", "stretch")
					}
				}
				switch str(gs["windowType"]) {
				case "fullscreen":
					wrappers = append(wrappers, "-f")
				case "borderless":
					wrappers = append(wrappers, "-b")
				}
			}
			if boolean(gs, "enableLimiter", false) {
				if v := str(gs["fpsLimiter"]); v != "" {
					wrappers = append(wrappers, "-r", v)
				}
				if v := str(gs["fpsLimiterNoFocus"]); v != "" {
					wrappers = append(wrappers, "-o", v)
				}
			}
			if boolean(gs, "enableForceGrabCursor", false) {
				wrappers = append(wrappers, "--force-grab-cursor")
			}
			if boolean(cfg, "showMangohud", false) {
				wrappers = append(wrappers, "--mangoapp")
			}
			more, err := splitArgs(str(gs["additionalOptions"]))
			if err != nil {
				return "", fmt.Errorf("gamescope additionalOptions: %w", err)
			}
			wrappers = append(wrappers, more...)
			wrappers = append(wrappers, "--")
		} else {
			warnings = append(warnings, "Gamescope was enabled but is not installed on the host; wrapper omitted")
		}
	}
	for _, entry := range arr(cfg["wrapperOptions"]) {
		v := obj(entry)
		if exe := str(v["exe"]); exe != "" {
			wrappers = append(wrappers, exe)
			a, err := splitArgs(str(v["args"]))
			if err != nil {
				return "", fmt.Errorf("wrapperOptions: %w", err)
			}
			wrappers = append(wrappers, a...)
		}
	}
	if boolean(cfg, "showMangohud", false) && !gamescopeActive {
		wrappers = append(wrappers, "mangohud", "--dlsym")
	}
	if boolean(cfg, "useGameMode", false) {
		wrappers = append(wrappers, "gamemoderun")
	}
	if rt := str(cfg["steamRuntime"]); rt != "" {
		env["PROTONPATH"] = expand(rt)
	}
	if native && cfg["steamRuntime"] != nil && cfg["steamRuntime"] != false && cfg["steamRuntime"] != "" {
		umu := preferredBinary("HEROIC_UMU_BIN", "umu-run")
		wrappers = append(wrappers, umu)
		if !(pathExists(umu) || toolExists(umu)) {
			warnings = append(warnings, "Steam Runtime requested for a native game but host umu-run is missing")
		}
	}

	var args []string
	switch g.store {
	case "epic":
		id := g.id
		if str(launchOpt["type"]) == "dlc" {
			id = nonempty(str(launchOpt["dlcAppName"]), id)
		}
		args = []string{runner.path, "launch", id}
		args = append(args, gameArgs...) // Heroic Legendary places extra arguments before launcher flags.
		if exe != "" && exe != g.executable {
			args = append(args, "--override-exe", exe)
		}
		if lang := str(cfg["language"]); lang != "" {
			args = append(args, "--language", lang)
		}
		if boolean(cfg, "offlineMode", false) {
			args = append(args, "--offline")
		}
		if boolean(g.install, "isEAManaged", false) {
			args = append(args, "--origin")
		}
		if boolean(g.install, "isUbisoftManaged", false) {
			args = append(args, "--ubisoft")
		}
	case "gog":
		if direct {
			args = append([]string{directExe}, directTaskArgs...)
		} else {
			args = []string{
				runner.path,
				"--auth-config-path", filepath.Join(g.root, "gog_store", "auth.json"),
				"launch", g.directory,
			}
			if exe != "" && exe != g.executable {
				args = append(args, "--override-exe", exe)
			}
			id := g.id
			if id == "1423049311" && boolean(obj(g.install["cyberpunk"]), "modsEnabled", false) {
				id = "1597316373"
				warnings = append(warnings, "Cyberpunk REDmod deployment is not reproduced")
			}
			args = append(args, id)
		}
	case "sideload":
		if g.browserURL != "" {
			args = []string{preferredBinary("HEROIC_BROWSER_BIN", "xdg-open"), g.browserURL}
			warnings = append(warnings, "Browser game: opens in an external browser instead of Heroic's embedded browser")
		} else {
			if exe == "" {
				return "", errors.New("sideload game has no executable")
			}
			args = []string{exe}
		}
	}

	if g.store == "epic" || (g.store == "gog" && !direct) {
		if !native {
			if wineType == "proton" {
				usingUMU := !boolean(cfg, "disableUMU", false)
				umu := preferredBinary("HEROIC_UMU_BIN", "umu-run")
				if _, err := exec.LookPath(umu); err != nil && !pathExists(umu) {
					usingUMU = false
				}
				if usingUMU {
					wrappers = append(wrappers, umu)
				} else {
					wrappers = append(wrappers, wineBin, "waitforexitandrun")
				}
				args = append(args, "--no-wine", "--wrapper", shellJoin(wrappers))
			} else {
				if wineBin == "" {
					return "", errors.New("Wine executable missing from Heroic settings")
				}
				args = append(args, "--wine", wineBin)
				if len(wrappers) > 0 {
					args = append(args, "--wrapper", shellJoin(wrappers))
				}
			}
		} else if len(wrappers) > 0 {
			args = append(args, "--wrapper", shellJoin(wrappers))
		}
		if g.store == "gog" {
			args = append(args, "--platform", strings.ToLower(g.platform))
		}
		if g.store == "gog" {
			args = append(args, gameArgs...)
		}
	} else {
		if g.browserURL == "" {
			if !native {
				if wineType == "proton" {
					if !direct {
						env["PROTON_VERB"] = "waitforexitandrun"
					}
					umu := preferredBinary("HEROIC_UMU_BIN", "umu-run")
					if !boolean(cfg, "disableUMU", false) && (pathExists(umu) || toolExists(umu)) {
						args = append(append(wrappers, umu), args...)
					} else {
						args = append(append(wrappers, wineBin, "waitforexitandrun"), args...)
					}
				} else {
					args = append(append(wrappers, wineBin), args...)
				}
			} else {
				args = append(wrappers, args...)
			}
			args = append(args, gameArgs...)
		}
	}
	if len(args) == 0 {
		return "", errors.New("empty command")
	}
	if !native && wineType != "wine" && wineType != "toolkit" && wineType != "proton" {
		return "", fmt.Errorf("unsupported Wine runtime type %q", wineType)
	}
	umu := preferredBinary("HEROIC_UMU_BIN", "umu-run")
	if pathExists(filepath.Join(g.root, "tools", "runtimes", "umu", "umu_run.py")) && wineType == "proton" && !pathExists(umu) && !toolExists(umu) {
		warnings = append(warnings, "Heroic has a bundled UMU inside its app; install host 'umu-run' for equivalent host execution")
	}
	if g.root == filepath.Join(os.Getenv("HOME"), ".var", "app", "com.heroicgameslauncher.hgl", "config", "heroic") {
		warnings = append(warnings, "Flatpak Heroic: ensure host Wine/Proton and store runners are installed and accessible")
	}
	if str(cfg["beforeLaunchScriptPath"]) != "" || str(cfg["afterLaunchScriptPath"]) != "" {
		warnings = append(warnings, "Heroic before/after-launch scripts are not executed by this command")
	}
	if boolean(cfg, "autoSyncSaves", false) {
		warnings = append(warnings, "Heroic cloud-save syncing is not reproduced")
	}
	// Sort for deterministic command output; only emit environment keys with valid names.
	keys := make([]string, 0, len(env))
	for key := range env {
		if !validEnvName(key) {
			return "", fmt.Errorf("invalid environment variable %q", key)
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := []string{shellQuote("env")}
	for _, key := range keys {
		value := shellQuote(key + "=" + env[key])
		if key == "LD_LIBRARY_PATH" && inheritNixLibraryPath {
			value += `"${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"`
		} else if key == "ORIG_LD_LIBRARY_PATH" && captureNixLibraryPath {
			value = `"ORIG_LD_LIBRARY_PATH=${LD_LIBRARY_PATH-}"`
		}
		parts = append(parts, value)
	}
	parts = append(parts, shellJoin(args))
	invocation := strings.Join(parts, " ")
	var output strings.Builder
	fmt.Fprintf(&output, "# %s [%s:%s]\n", strings.ReplaceAll(g.title, "\n", " "), g.store, g.id)
	if nixEnv != nil {
		fmt.Fprintln(&output, "# Nix: reuse Heroic's installed FHS runtime (graphics libraries, drivers and package overrides)")
		fmt.Fprintln(&output, "# Nix launcher:", nixEnv.launcher)
	}
	if direct {
		fmt.Fprintln(&output, "# Direct GOG launch: primary playTask read from goggame-<id>.info; no gogdl or cloud-save synchronization")
	}
	if comet.path != "" {
		fmt.Fprintln(&output, "# Heroic's GOG Galaxy identity service (Comet) is started independently of cloud-save syncing")
	}
	for _, warning := range warnings {
		fmt.Fprintln(&output, "# NOTE:", warning)
	}
	var command string
	if workingDirectory != "" {
		command = fmt.Sprintf("( cd %s && %s )", shellQuote(workingDirectory), invocation)
	} else {
		command = invocation
	}
	var launch strings.Builder
	if comet.path != "" {
		// Unlike Heroic we emit a standalone shell snippet rather than
		// creating a supervising background daemon. The subshell traps EXIT
		// and kills only the Comet instance that it started.
		fmt.Fprintln(&launch, "(")
		fmt.Fprintf(&launch, "  %s --from-heroic --username %s --quit &\n", shellQuote(comet.path), shellQuote(cometUsername))
		fmt.Fprintln(&launch, "  heroic_cmd_comet_pid=$!")
		fmt.Fprintln(&launch, `  trap 'kill "$heroic_cmd_comet_pid" 2>/dev/null || true; wait "$heroic_cmd_comet_pid" 2>/dev/null || true' EXIT`)
		fmt.Fprintf(&launch, "  %s\n", command)
		fmt.Fprintln(&launch, ")")
	} else {
		fmt.Fprintln(&launch, command)
	}
	if nixEnv != nil {
		fmt.Fprintln(&output, nixEnv.wrap(launch.String()))
	} else {
		output.WriteString(launch.String())
	}
	return output.String(), nil
}
func toolExists(name string) bool { _, err := exec.LookPath(name); return err == nil }

func run() error {
	if len(os.Args) == 1 {
		return runGUI()
	}
	if os.Args[1] == "where" {
		if len(os.Args) == 3 && os.Args[2] == "nix" {
			runtime, err := resolveNixRuntime()
			if err != nil {
				return err
			}
			if runtime == nil {
				return errors.New("Nix runtime is disabled or no Nix Heroic launcher was found; set HEROIC_BIN to the installed pkgs.heroic executable")
			}
			fmt.Println(runtime.launcher)
			fmt.Fprintln(os.Stderr, "Source: installed Heroic FHS launcher; /init is adapted in memory when the printed command runs")
			return nil
		}
		if len(os.Args) != 3 || (os.Args[2] != "gogdl" && os.Args[2] != "legendary" && os.Args[2] != "comet") {
			return errors.New("usage: heroic-wrapper where gogdl|legendary|comet|nix")
		}
		roots := configRoots()
		if len(roots) == 0 {
			return errors.New("no Heroic config directory found; set HEROIC_CONFIG_DIR")
		}
		var lastErr error
		for _, root := range roots {
			binary := os.Args[2]
			envName, configKey := "HEROIC_GOGDL_BIN", "altGogdlBin"
			if binary == "legendary" {
				envName, configKey = "HEROIC_LEGENDARY_BIN", "altLegendaryBin"
			} else if binary == "comet" {
				envName, configKey = "HEROIC_COMET_BIN", "altCometBin"
			}
			location, err := resolveRunner(game{root: root}, envName, configKey, binary)
			if err == nil {
				fmt.Println(location.path)
				fmt.Fprintln(os.Stderr, "Source:", location.source)
				if location.note != "" {
					fmt.Fprintln(os.Stderr, "NOTE:", location.note)
				}
				return nil
			}
			lastErr = err
		}
		return lastErr
	}
	if os.Args[1] != "list" && os.Args[1] != "show" {
		return fmt.Errorf("unknown command %q (expected list, show, or where)", os.Args[1])
	}
	direct := false
	disableComet := false
	selector := ""
	if os.Args[1] == "show" {
		for _, arg := range os.Args[2:] {
			switch arg {
			case "--direct":
				direct = true
			case "--no-comet":
				disableComet = true
			default:
				if strings.HasPrefix(arg, "--") || selector != "" {
					return errors.New("usage: heroic-wrapper show [--direct] [--no-comet] 'Game Name'")
				}
				selector = arg
			}
		}
		if selector == "" {
			return errors.New("usage: heroic-wrapper show [--direct] [--no-comet] 'Game Name'")
		}
	}
	if os.Args[1] == "list" && len(os.Args) != 2 {
		return errors.New("usage: heroic-wrapper list")
	}
	games, err := allGames()
	if err != nil {
		return err
	}
	if os.Args[1] == "list" {
		for _, game := range games {
			fmt.Printf("%s\t[%s:%s]\n", game.title, game.store, game.id)
		}
		return nil
	}
	chosen, err := pick(games, selector)
	if err != nil {
		return err
	}
	result, err := buildCommand(chosen, direct, disableComet)
	if err != nil {
		return err
	}
	fmt.Print(result)
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "heroic-wrapper:", err)
		os.Exit(1)
	}
}
