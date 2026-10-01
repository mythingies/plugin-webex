package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/mythingies/plugin-webex/internal/webex"
)

// shareDirsEnv names extra directories, separated by os.PathListSeparator, that
// share_file may read from besides the working and temp directories.
const shareDirsEnv = "WEBEX_SHARE_DIRS"

// secretFileRe matches file names share_file refuses even inside an allowed directory:
// environment files, keys and keystores, SSH identities, and credential or token files.
var secretFileRe = regexp.MustCompile(`(?i)^(\.env.*|.*\.(pem|key|p12|pfx|jks|keystore|kdbx|ppk)|id_[a-z0-9_]+(\.pub)?|.*(credential|secret|token|password).*)$`)

func registerShareFile(s *mcpserver.MCPServer, client *webex.Client) {
	opts := append([]mcp.ToolOption{
		mcp.WithDescription("Upload a local file to a Webex space or thread, with an optional message. " +
			"Only regular files under the working directory, the OS temp directory, or " + shareDirsEnv +
			" are allowed, never hidden paths or key, credential, or .env files, up to 100 MB."),
		mcp.WithString("room_id",
			mcp.Required(),
			mcp.Description("The ID of the space to share the file in."),
		),
		mcp.WithString("file_path",
			mcp.Required(),
			mcp.Description("The local file path to upload."),
		),
		mcp.WithString("parent_id",
			mcp.Description("The ID of the parent message, to share the file in its thread."),
		),
	}, messageOptions("message sent with the file")...)
	tool := mcp.NewTool("share_file", opts...)

	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		roomID, err := req.RequireString("room_id")
		if err != nil {
			return mcp.NewToolResultError("room_id is required"), nil
		}
		filePath, err := req.RequireString("file_path")
		if err != nil {
			return mcp.NewToolResultError("file_path is required"), nil
		}
		text, markdown, errResult := messageBody(req, false)
		if errResult != nil {
			return errResult, nil
		}
		name, data, err := readShareable(filePath, shareRoots())
		if err != nil {
			auditLog("share_file", "refused", "room_id", roomID, "reason", err.Error())
			return mcp.NewToolResultError(err.Error()), nil
		}

		parentID := req.GetString("parent_id", "")
		msg, err := client.ShareFile(roomID, parentID, name, data, text, markdown)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to share file: %v", err)), nil
		}

		auditLog("share_file", "sent", "msg_id", msg.ID, "room_id", roomID, "parent_id", parentID, "bytes", len(data))
		return mcp.NewToolResultText(fmt.Sprintf("File shared (id: %s, created: %s)", msg.ID, msg.Created)), nil
	})
}

// shareRoots are the directories share_file may read from.
func shareRoots() []string {
	var roots []string
	if wd, err := os.Getwd(); err == nil {
		roots = append(roots, wd)
	}
	roots = append(roots, os.TempDir())
	for _, d := range filepath.SplitList(os.Getenv(shareDirsEnv)) {
		if d != "" {
			roots = append(roots, d)
		}
	}
	return roots
}

// readShareable returns the base name and bytes of the file at p, if it is a regular
// file, not a symlink, inside one of roots by real path, with no hidden component
// below that root, not named like a secret, and within webex.MaxUploadSize. A Webex
// message can try to talk the model into uploading a key, so these limits hold
// whatever the model is asked.
func readShareable(p string, roots []string) (string, []byte, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", nil, fmt.Errorf("resolving %s: %w", p, err)
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return "", nil, fmt.Errorf("reading %s: %w", p, err)
	}
	if !info.Mode().IsRegular() {
		return "", nil, fmt.Errorf("%s is not a regular file; directories, devices, and symlinks are refused", p)
	}
	name := filepath.Base(abs)
	if secretFileRe.MatchString(name) {
		return "", nil, fmt.Errorf("%s looks like a key, credential, or environment file and is refused", name)
	}
	if info.Size() > webex.MaxUploadSize {
		return "", nil, fmt.Errorf("%s is %d bytes, over the %d-byte limit", p, info.Size(), webex.MaxUploadSize)
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", nil, fmt.Errorf("resolving %s: %w", p, err)
	}
	if !insideRoot(dir, roots) {
		return "", nil, fmt.Errorf("%s is outside the working directory, the temp directory, and %s", p, shareDirsEnv)
	}

	f, err := os.Open(abs) // #nosec G304 -- path checked above: regular file inside an allowed root
	if err != nil {
		return "", nil, fmt.Errorf("opening %s: %w", p, err)
	}
	defer func() { _ = f.Close() }()
	// The file opened must be the one checked, not one swapped in since.
	if opened, err := f.Stat(); err != nil || !os.SameFile(info, opened) {
		return "", nil, errors.New(p + " changed while it was being checked")
	}
	data, err := io.ReadAll(io.LimitReader(f, webex.MaxUploadSize+1))
	if err != nil {
		return "", nil, fmt.Errorf("reading %s: %w", p, err)
	}
	if len(data) > webex.MaxUploadSize {
		return "", nil, fmt.Errorf("%s grew past the %d-byte limit", p, webex.MaxUploadSize)
	}
	return name, data, nil
}

// insideRoot reports whether dir is a root, or below one with no hidden component
// such as .ssh or .git between them.
func insideRoot(dir string, roots []string) bool {
	for _, r := range roots {
		real, err := filepath.EvalSymlinks(r)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(real, dir)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			continue
		}
		hidden := false
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			if part != "." && strings.HasPrefix(part, ".") {
				hidden = true
				break
			}
		}
		if !hidden {
			return true
		}
	}
	return false
}
