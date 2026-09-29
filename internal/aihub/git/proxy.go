package git

// The git-http-backend smart-protocol proxy: aihub mounts /git/{wi}.git/…
// and execs the real `git http-backend` CGI. Basic auth: username must be
// the work-item id and the secret must equal the current ACTIVE attempt's
// push credential — an ended generation's credential is rejected here
// (takeover fencing main layer; GIT-401).

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/silgrid/polyforge/internal/common/api"
)

// HandleGit is the http.Handler for the /git/ prefix.
func (m *Manager) HandleGit(w http.ResponseWriter, r *http.Request) {
	// Path shape after /git/: "<wi_id>.git[/…]".
	rest := strings.TrimPrefix(r.URL.Path, "/git/")
	rest = strings.TrimPrefix(rest, "/")
	wiID := rest
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		wiID = rest[:i]
	}
	wiID = strings.TrimSuffix(wiID, ".git")
	if !strings.HasPrefix(wiID, "wi_") {
		writeGitErr(w, api.ErrGitNotFound, "unknown git path %q", r.URL.Path)
		return
	}

	// Basic auth against the active attempt's credential.
	user, secret, ok := r.BasicAuth()
	if !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="polyforge"`)
		writeGitErr(w, api.ErrGitUnauthorized, "git access requires an attempt-scoped credential")
		return
	}
	if _, err := m.checker.CheckPushCredential(wiID, user, secret); err != nil {
		w.Header().Set("WWW-Authenticate", `Basic realm="polyforge"`)
		// The git client only understands the 401; the typed code rides in
		// the response headers for diagnostics.
		ge, isTyped := err.(*api.Error)
		code := api.ErrGitUnauthorized
		detail := err.Error()
		if isTyped {
			code, detail = ge.Code, ge.Detail
		}
		w.Header().Set("X-PF-Git-Error", code)
		writeGitErr(w, code, "%s", detail)
		return
	}

	// The repo must exist (created at claim time). A clone that races a
	// claim is served a clean 404 rather than an empty CGI 500.
	repoDir := m.RepoDir(wiID)
	if _, err := os.Stat(filepath.Join(repoDir, "HEAD")); err != nil {
		writeGitErr(w, api.ErrGitNotFound, "no repository for %s", wiID)
		return
	}

	// CGI environment (git http-backend contract). Base is the inherited
	// process environment: http-backend execs git-receive-pack/upload-pack
	// from PATH, and the pre-receive hook (a bash script exec'd by
	// receive-pack) inherits this environment too.
	env := os.Environ()
	env = append(env,
		"GIT_PROJECT_ROOT="+m.Root,
		"GIT_HTTP_EXPORT_ALL=1",
		"PATH_INFO=/"+rest,
		"PATH_TRANSLATED="+filepath.Join(m.Root, rest),
		"REQUEST_METHOD="+r.Method,
		"QUERY_STRING="+r.URL.RawQuery,
		"REMOTE_USER="+wiID,
		"REMOTE_ADDR="+remoteAddr(r),
		"CONTENT_TYPE="+r.Header.Get("Content-Type"),
		"GIT_PF_MAX_BLOB="+strconv.FormatInt(m.MaxBlobBytes, 10),
	)
	if n := r.Header.Get("Content-Length"); n != "" {
		env = append(env, "CONTENT_LENGTH="+n)
	}
	// Protocol v2 negotiation travels in the Git-Protocol header.
	if gp := r.Header.Get("Git-Protocol"); gp != "" {
		env = append(env, "GIT_PROTOCOL="+gp)
	}
	for k, v := range r.Header {
		// Pass HTTP_* through (http-backend reads HTTP_ACCEPT etc.).
		if strings.HasPrefix(k, "Git-") || k == "Content-Type" || k == "Content-Length" {
			continue
		}
		name := "HTTP_" + strings.ToUpper(strings.ReplaceAll(k, "-", "_"))
		for _, hv := range v {
			env = append(env, name+"="+hv)
		}
	}

	cmd := exec.Command("git", "http-backend")
	cmd.Env = env
	cmd.Dir = m.Root
	stdin, err := cmd.StdinPipe()
	if err != nil {
		writeGitErr(w, api.ErrGitInternal, "%v", err)
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		writeGitErr(w, api.ErrGitInternal, "%v", err)
		return
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		writeGitErr(w, api.ErrGitInternal, "start http-backend: %v", err)
		return
	}
	// Body → CGI stdin (bounded by the request itself).
	go func() {
		_, _ = io.Copy(stdin, r.Body)
		stdin.Close()
	}()

	// Parse the CGI response: headers, then body.
	br := bufio.NewReader(stdout)
	status := http.StatusOK
	hdr := http.Header{}
	for {
		line, rerr := br.ReadString('\n')
		if rerr != nil {
			writeGitErr(w, api.ErrGitInternal, "http-backend died before headers")
			_, _ = cmd.Process.Wait()
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		i := strings.IndexByte(line, ':')
		if i < 0 {
			continue
		}
		name, value := strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:])
		switch strings.ToLower(name) {
		case "status":
			if fields := strings.Fields(value); len(fields) > 0 {
				if code, cerr := strconv.Atoi(fields[0]); cerr == nil {
					status = code
				}
			}
		default:
			hdr.Add(name, value)
		}
	}
	for k, vs := range hdr {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(status)
	_, _ = io.Copy(w, br)
	_ = cmd.Wait()
}

func remoteAddr(r *http.Request) string {
	if i := strings.LastIndexByte(r.RemoteAddr, ':'); i > 0 {
		return r.RemoteAddr[:i]
	}
	return r.RemoteAddr
}

// writeGitErr renders the typed envelope for git-surface failures that
// never reach CGI (auth, unknown paths, backend startup). The git client
// itself only reacts to the HTTP status.
func writeGitErr(w http.ResponseWriter, code, format string, args ...any) {
	e := &api.Error{Code: code, Detail: fmt.Sprintf(format, args...)}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.HTTPStatus())
	// Deliberately small writes; git clients print this on failure.
	_, _ = fmt.Fprintf(w, `{"code":%q,"detail":%q}`, e.Code, e.Detail)
}
