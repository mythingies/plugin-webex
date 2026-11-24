package replydesk

import (
	"errors"
	"os/exec"
	"runtime"
	"strings"
)

// openBrowser opens url in the user's default browser. It only accepts the
// loopback URLs this package builds. Failure is not fatal for the caller: the
// URL has already been printed.
func openBrowser(url string) error {
	if !strings.HasPrefix(url, "http://127.0.0.1:") || strings.ContainsAny(url, " \t\r\n\"'&|<>^%") {
		return errors.New("replydesk: refusing to open a non-loopback URL")
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url) // #nosec G204 -- fixed binary, URL built by us
	case "darwin":
		cmd = exec.Command("open", url) // #nosec G204 -- fixed binary, URL built by us
	default:
		cmd = exec.Command("xdg-open", url) // #nosec G204 -- fixed binary, URL built by us
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	// Reap the launcher; its exit status says nothing useful.
	go func() { _ = cmd.Wait() }()
	return nil
}
