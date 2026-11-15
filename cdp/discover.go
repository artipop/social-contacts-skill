package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// EnvURL names the environment variable that overrides discovery with an
// explicit address, the same as Options.URL.
const EnvURL = "SOCIAL_CONTACTS_CDP_URL"

// Channel is a Chrome release channel. Each keeps its own user data directory,
// and so its own DevToolsActivePort.
type Channel string

const (
	Stable Channel = "stable"
	Beta   Channel = "beta"
	Dev    Channel = "dev"
	Canary Channel = "canary"
)

// UserDataDir is where Chrome of the given channel keeps its profiles on this
// machine — the directory Chrome writes DevToolsActivePort into.
func UserDataDir(channel Channel) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return userDataDir(runtime.GOOS, channel, home, os.Getenv)
}

// userDataDir follows Chromium's chrome_paths_{mac,win,linux}: the default
// user data directory per OS and channel.
func userDataDir(goos string, channel Channel, home string, getenv func(string) string) (string, error) {
	if channel == "" {
		channel = Stable
	}
	var names map[Channel]string
	var base string
	switch goos {
	case "darwin":
		base = filepath.Join(home, "Library", "Application Support", "Google")
		names = map[Channel]string{Stable: "Chrome", Beta: "Chrome Beta", Dev: "Chrome Dev", Canary: "Chrome Canary"}
	case "windows":
		base = getenv("LOCALAPPDATA")
		if base == "" {
			base = filepath.Join(home, "AppData", "Local")
		}
		base = filepath.Join(base, "Google")
		names = map[Channel]string{
			Stable: filepath.Join("Chrome", "User Data"),
			Beta:   filepath.Join("Chrome Beta", "User Data"),
			Dev:    filepath.Join("Chrome Dev", "User Data"),
			Canary: filepath.Join("Chrome SxS", "User Data"),
		}
	case "linux":
		base = getenv("XDG_CONFIG_HOME")
		if base == "" {
			base = filepath.Join(home, ".config")
		}
		names = map[Channel]string{
			Stable: "google-chrome", Beta: "google-chrome-beta",
			Dev: "google-chrome-unstable", Canary: "google-chrome-canary",
		}
	default:
		return "", fmt.Errorf("Chrome на %s не поддерживается: укажите адрес отладки вручную (%s)", goos, EnvURL)
	}
	name, ok := names[channel]
	if !ok {
		return "", fmt.Errorf("неизвестный канал Chrome %q", channel)
	}
	return filepath.Join(base, name), nil
}

// ReadActivePort reads DevToolsActivePort from a user data directory and
// returns the browser's websocket address.
//
// Chrome writes the file when remote debugging is on: the port on the first
// line, the browser target's websocket path on the second. The path carries a
// per-launch id, so the address cannot be guessed — and with auto-connect
// Chrome serves no /json/version to look it up.
func ReadActivePort(userDataDir string) (string, error) {
	path := filepath.Join(userDataDir, "DevToolsActivePort")
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("%w (нет файла %s)", ErrNotEnabled, path)
	case errors.Is(err, fs.ErrPermission):
		return "", fmt.Errorf("%w (%s: %v)", ErrProfileUnreadable, path, err)
	case err != nil:
		return "", fmt.Errorf("не прочитать %s: %w", path, err)
	}

	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) < 2 {
		return "", fmt.Errorf("%w (в %s нет порта и пути)", ErrNotEnabled, path)
	}
	port, err := strconv.Atoi(lines[0])
	if err != nil || port <= 0 || port > 65535 {
		return "", fmt.Errorf("в %s неверный порт %q", path, lines[0])
	}
	if !strings.HasPrefix(lines[1], "/") {
		return "", fmt.Errorf("в %s неверный путь %q", path, lines[1])
	}
	return "ws://127.0.0.1:" + strconv.Itoa(port) + lines[1], nil
}

// endpoint decides where to connect: an explicit address first, then
// DevToolsActivePort of the chosen profile.
func endpoint(ctx context.Context, opts Options) (string, error) {
	explicit := strings.TrimSpace(opts.URL)
	if explicit == "" {
		explicit = strings.TrimSpace(os.Getenv(EnvURL))
	}
	if explicit != "" {
		return resolveURL(ctx, explicit)
	}
	dir := opts.UserDataDir
	if dir == "" {
		var err error
		if dir, err = UserDataDir(opts.Channel); err != nil {
			return "", err
		}
	}
	return ReadActivePort(dir)
}

// resolveURL accepts a browser websocket address as is, or the HTTP address of
// a Chrome started with --remote-debugging-port, whose /json/version names the
// websocket.
func resolveURL(ctx context.Context, raw string) (string, error) {
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("неверный адрес отладки %q: %w", raw, err)
	}
	switch u.Scheme {
	case "ws", "wss":
		return raw, nil
	case "http", "https":
	default:
		return "", fmt.Errorf("неверный адрес отладки %q: нужен ws:// или http://", raw)
	}

	u.Path = strings.TrimSuffix(u.Path, "/") + "/json/version"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		if isRefused(err) {
			return "", fmt.Errorf("%w (по адресу %s никто не слушает)", ErrNotEnabled, raw)
		}
		return "", fmt.Errorf("не связаться с Chrome по %s: %w", raw, err)
	}
	defer resp.Body.Close()
	var version struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s ответил %s: укажите адрес ws:// из DevToolsActivePort", u, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(&version); err != nil || version.WebSocketDebuggerURL == "" {
		return "", fmt.Errorf("%s не назвал адрес websocket", u)
	}
	return version.WebSocketDebuggerURL, nil
}

// httpClient never goes through a proxy: the debugging port is local, and a
// proxy from the environment would only get in the way.
var httpClient = &http.Client{Transport: &http.Transport{Proxy: nil}}
