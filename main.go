// heroic-wrapper: read-only Heroic Games Launcher game discovery and launch-command generator.
// Linux, Go standard library only. This program NEVER creates/writes a file.
// Build: go build -o heroic-wrapper heroic-wrapper.go
// Usage: heroic-wrapper list | heroic-wrapper show 'Game Title' | heroic-wrapper show epic:APP_ID
// Override Heroic's config directory with HEROIC_CONFIG_DIR=/path/to/heroic.
// Override host runner paths with HEROIC_LEGENDARY_BIN, HEROIC_GOGDL_BIN, HEROIC_UMU_BIN.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
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
	return binary // shell will resolve from PATH when the user runs it
}
func pathExists(p string) bool { _, err := os.Stat(p); return err == nil }

func buildCommand(g game) (string, error) {
	cfg, err := configSettings(g)
	if err != nil {
		return "", err
	}
	warnings := []string{}
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
	env := map[string]string{}
	if g.directory != "" {
		env["STEAM_COMPAT_INSTALL_PATH"] = g.directory
	}
	switch g.store {
	case "epic":
		env["LEGENDARY_CONFIG_PATH"] = filepath.Join(g.root, "legendaryConfig", "legendary")
		env["STORE"], env["GAMEID"] = "egs", "umu-0"
	case "gog":
		env["GOGDL_CONFIG_PATH"] = filepath.Join(g.root, "gogdlConfig")
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
				for _, p := range []string{lib, lib32, os.Getenv("LD_LIBRARY_PATH")} {
					if p != "" {
						pieces = append(pieces, p)
					}
				}
				if len(pieces) > 0 {
					if _, ok := os.LookupEnv("ORIG_LD_LIBRARY_PATH"); !ok {
						env["ORIG_LD_LIBRARY_PATH"] = os.Getenv("LD_LIBRARY_PATH")
					}
					env["LD_LIBRARY_PATH"] = strings.Join(pieces, ":")
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
		args = []string{preferredBinary("HEROIC_LEGENDARY_BIN", "legendary"), "launch", id}
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
		args = []string{
			preferredBinary("HEROIC_GOGDL_BIN", "gogdl"),
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

	if g.store == "epic" || g.store == "gog" {
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
					env["PROTON_VERB"] = "waitforexitandrun"
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
	if g.store == "epic" && !toolExists(preferredBinary("HEROIC_LEGENDARY_BIN", "legendary")) && !pathExists(preferredBinary("HEROIC_LEGENDARY_BIN", "legendary")) {
		warnings = append(warnings, "legendary CLI not found on host; install it or set HEROIC_LEGENDARY_BIN")
	}
	if g.store == "gog" && !toolExists(preferredBinary("HEROIC_GOGDL_BIN", "gogdl")) && !pathExists(preferredBinary("HEROIC_GOGDL_BIN", "gogdl")) {
		warnings = append(warnings, "gogdl CLI not found on host; install it or set HEROIC_GOGDL_BIN")
	}
	if pathExists(filepath.Join(g.root, "tools", "runtimes", "umu", "umu_run.py")) && wineType == "proton" && !toolExists("umu-run") {
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
	parts := []string{"env"}
	for _, key := range keys {
		parts = append(parts, key+"="+env[key])
	}
	parts = append(parts, args...)
	var output strings.Builder
	fmt.Fprintf(&output, "# %s [%s:%s]\n", strings.ReplaceAll(g.title, "\n", " "), g.store, g.id)
	for _, warning := range warnings {
		fmt.Fprintln(&output, "# NOTE:", warning)
	}
	if workingDirectory != "" {
		fmt.Fprintf(&output, "( cd %s && %s )\n", shellQuote(workingDirectory), shellJoin(parts))
	} else {
		fmt.Fprintf(&output, "%s\n", shellJoin(parts))
	}
	return output.String(), nil
}
func toolExists(name string) bool { _, err := exec.LookPath(name); return err == nil }

func run() error {
	if len(os.Args) < 2 {
		return errors.New("usage: heroic-wrapper list | heroic-wrapper show 'Game Name' | heroic-wrapper show epic:APP_ID")
	}
	if os.Args[1] != "list" && os.Args[1] != "show" {
		return fmt.Errorf("unknown command %q (expected list or show)", os.Args[1])
	}
	if os.Args[1] == "show" && len(os.Args) != 3 {
		return errors.New("usage: heroic-wrapper show 'Game Name'")
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
	chosen, err := pick(games, os.Args[2])
	if err != nil {
		return err
	}
	result, err := buildCommand(chosen)
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
