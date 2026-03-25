package auth

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	// ProtocolScheme is the custom URI scheme for OAuth callbacks.
	ProtocolScheme = "wmcp"

	// ProtocolCallbackURL is the redirect URI to register with Webex.
	ProtocolCallbackURL = ProtocolScheme + "://oauth-callback"
)

// RegisterProtocol registers the wmcp:// custom URI scheme with the OS
// so that OAuth callbacks open the webex-mcp binary.
func RegisterProtocol(binaryPath string) error {
	if binaryPath == "" {
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("resolving executable path: %w", err)
		}
		binaryPath, _ = filepath.Abs(exe)
	}

	// Validate the binary path does not contain characters that could
	// break shell quoting or registry values.
	if strings.ContainsAny(binaryPath, "\"'`$\\;|&<>!\n\r") && runtime.GOOS != "windows" {
		return fmt.Errorf("binary path contains unsafe characters: %s", binaryPath)
	}
	if strings.ContainsAny(binaryPath, "\"") && runtime.GOOS == "windows" {
		return fmt.Errorf("binary path contains double quotes: %s", binaryPath)
	}

	switch runtime.GOOS {
	case "windows":
		return registerProtocolWindows(binaryPath)
	case "darwin":
		return registerProtocolDarwin(binaryPath)
	case "linux":
		return registerProtocolLinux(binaryPath)
	default:
		return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
}

// HandleCallbackURL parses a wmcp:// URL from the OS and writes the callback file.
// Called by `webex-mcp --oauth-callback <url>`.
func HandleCallbackURL(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("parsing callback URL: %w", err)
	}

	// Validate scheme and host strictly.
	if parsed.Scheme != ProtocolScheme {
		return fmt.Errorf("unexpected scheme %q (expected %q)", parsed.Scheme, ProtocolScheme)
	}
	if parsed.Host != "oauth-callback" {
		return fmt.Errorf("unexpected host %q (expected oauth-callback)", parsed.Host)
	}

	code := parsed.Query().Get("code")
	state := parsed.Query().Get("state")
	errMsg := parsed.Query().Get("error")

	// Length validation to prevent abuse.
	const maxParamLen = 4096
	if len(code) > maxParamLen || len(state) > maxParamLen || len(errMsg) > maxParamLen {
		return fmt.Errorf("callback parameter exceeds maximum length")
	}

	if code == "" && errMsg == "" {
		return fmt.Errorf("no code or error in callback URL")
	}

	return WriteCallbackFile(code, state, errMsg)
}

func registerProtocolWindows(binaryPath string) error {
	// Create a VBScript wrapper that launches the handler without a visible
	// console window. This prevents the terminal flash on OAuth callback.
	vbsDir := filepath.Dir(binaryPath)
	vbsPath := filepath.Join(vbsDir, "wmcp-callback.vbs")
	vbs := fmt.Sprintf(
		"CreateObject(\"WScript.Shell\").Run \"\"\"%s\"\" --oauth-callback \"\"\" & WScript.Arguments(0) & \"\"\"\", 0, False\r\n",
		binaryPath,
	)
	if err := os.WriteFile(vbsPath, []byte(vbs), 0600); err != nil {
		return fmt.Errorf("writing VBScript wrapper: %w", err)
	}

	key := `HKCU\Software\Classes\` + ProtocolScheme
	commands := [][]string{
		{"reg", "add", key, "/ve", "/d", "URL:Webex MCP Protocol", "/f"},
		{"reg", "add", key, "/v", "URL Protocol", "/d", "", "/f"},
		{"reg", "add", key + `\shell\open\command`, "/ve", "/d",
			fmt.Sprintf(`wscript.exe "%s" "%%1"`, vbsPath), "/f"},
	}

	for _, args := range commands {
		cmd := exec.Command(args[0], args[1:]...) //nolint:gosec // args are constant registry commands
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("registry command failed: %s: %w", strings.TrimSpace(string(out)), err)
		}
	}
	return nil
}

func registerProtocolDarwin(binaryPath string) error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("getting home directory: %w", err)
	}

	appDir := filepath.Join(homeDir, "Applications", "WebexMCP.app")
	contentsDir := filepath.Join(appDir, "Contents")
	macosDir := filepath.Join(contentsDir, "MacOS")

	if err := os.MkdirAll(macosDir, 0750); err != nil {
		return fmt.Errorf("creating app bundle: %w", err)
	}

	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleIdentifier</key>
  <string>com.webex-mcp.oauth</string>
  <key>CFBundleName</key>
  <string>WebexMCP</string>
  <key>CFBundleExecutable</key>
  <string>wmcp-handler</string>
  <key>CFBundleURLTypes</key>
  <array>
    <dict>
      <key>CFBundleURLName</key>
      <string>Webex MCP OAuth</string>
      <key>CFBundleURLSchemes</key>
      <array>
        <string>%s</string>
      </array>
    </dict>
  </array>
</dict>
</plist>`, ProtocolScheme)

	if err := os.WriteFile(filepath.Join(contentsDir, "Info.plist"), []byte(plist), 0600); err != nil {
		return fmt.Errorf("writing Info.plist: %w", err)
	}

	script := fmt.Sprintf("#!/bin/sh\nexec \"%s\" --oauth-callback \"$@\"\n", binaryPath)
	handlerPath := filepath.Join(macosDir, "wmcp-handler")
	if err := os.WriteFile(handlerPath, []byte(script), 0750); err != nil { //nolint:gosec // must be executable for macOS app bundle
		return fmt.Errorf("writing handler script: %w", err)
	}

	cmd := exec.Command("/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister", //nolint:gosec // constant system path
		"-R", appDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("lsregister failed: %s: %w", strings.TrimSpace(string(out)), err)
	}

	return nil
}

func registerProtocolLinux(binaryPath string) error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("getting home directory: %w", err)
	}

	desktopDir := filepath.Join(homeDir, ".local", "share", "applications")
	if err := os.MkdirAll(desktopDir, 0750); err != nil {
		return fmt.Errorf("creating applications dir: %w", err)
	}

	desktop := fmt.Sprintf(`[Desktop Entry]
Name=Webex MCP OAuth Handler
Exec="%s" --oauth-callback %%u
Type=Application
NoDisplay=true
MimeType=x-scheme-handler/%s;
`, binaryPath, ProtocolScheme)

	desktopFile := filepath.Join(desktopDir, "wmcp-oauth.desktop")
	if err := os.WriteFile(desktopFile, []byte(desktop), 0600); err != nil {
		return fmt.Errorf("writing desktop file: %w", err)
	}

	cmd := exec.Command("xdg-mime", "default", "wmcp-oauth.desktop", //nolint:gosec // constant args
		fmt.Sprintf("x-scheme-handler/%s", ProtocolScheme))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("xdg-mime failed: %s: %w", strings.TrimSpace(string(out)), err)
	}

	return nil
}
