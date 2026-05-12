// Package web serves a read-only browser UI over the maestro state directory.
// Useful for exploring projects, sessions, tasks, condensed summaries, and
// the full implementer prompt + exchange log that the CLI also reads.
//
// Wired up by `maestro web` in cmd/maestro. The package is intentionally
// stdlib-only (net/http + html/template + embed) so the binary stays small
// and there's no build pipeline.
package web

import (
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/fisherevans/maestro/internal/maestro"
)

//go:embed templates/*.html static/*
var assets embed.FS

// Serve binds to addr (e.g. "127.0.0.1:9876") and runs the maestro web UI
// until the process is interrupted. If openBrowser is true, the user's
// default browser is launched at the served URL once the listener is up.
//
// The server is read-only against ~/.maestro/. State is reloaded on each
// request, so CLI/orchestrator writes show up live.
func Serve(addr string, openBrowser bool) error {
	tmpls, err := loadTemplates()
	if err != nil {
		return fmt.Errorf("load templates: %w", err)
	}

	staticFS, err := fs.Sub(assets, "static")
	if err != nil {
		return fmt.Errorf("load static assets: %w", err)
	}

	s := &server{tmpls: tmpls}

	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /project/{project}", s.handleProject)
	mux.HandleFunc("GET /project/{project}/search", s.handleSearch)
	mux.HandleFunc("GET /project/{project}/session/{session}", s.handleSession)
	mux.HandleFunc("GET /project/{project}/task/{task}", s.handleTask)

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	actualAddr := listener.Addr().String()
	url := "http://" + actualAddr + "/"

	fmt.Printf("maestro web serving on %s\n", url)
	fmt.Println("press ctrl-c to stop")

	if openBrowser {
		go func() {
			// brief delay so the listener is ready before we open
			time.Sleep(150 * time.Millisecond)
			if err := openInBrowser(url); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not open browser: %v\n", err)
			}
		}()
	}

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return srv.Serve(listener)
}

type server struct {
	tmpls *template.Template
}

// loadTemplates parses every templates/*.html file with shared helpers
// available to all pages.
func loadTemplates() (*template.Template, error) {
	funcs := template.FuncMap{
		"shortTime": func(t time.Time) string {
			if t.IsZero() {
				return ""
			}
			return t.Format("2006-01-02 15:04")
		},
		"rfc3339": func(t time.Time) string {
			if t.IsZero() {
				return ""
			}
			return t.Format(time.RFC3339)
		},
		"ago": humanizeAgo,
		"nonzero": func(t time.Time) bool {
			return !t.IsZero()
		},
		"join": strings.Join,
		"add":  func(a, b int) int { return a + b },
		"sub":  func(a, b int) int { return a - b },
		"isActive": func(s maestro.TaskStatus) bool {
			return s.IsActive()
		},
		"statusClass": func(s maestro.TaskStatus) string {
			switch s {
			case maestro.StatusInProgress:
				return "status-in-progress"
			case maestro.StatusPending:
				return "status-pending"
			case maestro.StatusBlocked:
				return "status-blocked"
			case maestro.StatusAwaitingReview:
				return "status-awaiting"
			case maestro.StatusMerged:
				return "status-merged"
			case maestro.StatusAbandoned:
				return "status-abandoned"
			}
			return "status-other"
		},
		"noteClass": func(typ string) string {
			t := strings.ToLower(strings.TrimSpace(typ))
			if t == "" {
				return "note-untyped"
			}
			return "note-" + t
		},
		"markdown":    renderMarkdown,
		"parseReport": parseReportNote,
		"renderNote":  renderNoteContent,
		"reportFieldClass": func(key string) string {
			return "field-" + strings.ToLower(strings.ReplaceAll(key, "_", "-"))
		},
		"shortSha": func(s string) string {
			if len(s) > 12 {
				return s[:12]
			}
			return s
		},
	}

	pattern := "templates/*.html"
	t, err := template.New("").Funcs(funcs).ParseFS(assets, pattern)
	if err != nil {
		return nil, err
	}
	return t, nil
}

func humanizeAgo(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		if m == 0 {
			return fmt.Sprintf("%dh ago", h)
		}
		return fmt.Sprintf("%dh%dm ago", h, m)
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func openInBrowser(url string) error {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
	case "linux":
		cmd = "xdg-open"
	case "windows":
		cmd = "rundll32"
		args = []string{"url.dll,FileProtocolHandler"}
	default:
		return errors.New("unsupported OS")
	}
	args = append(args, url)
	return exec.Command(cmd, args...).Start()
}

func sortedTagCounts(counts map[string]int) []tagCount {
	out := make([]tagCount, 0, len(counts))
	for tag, n := range counts {
		out = append(out, tagCount{Tag: tag, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Tag < out[j].Tag
	})
	return out
}

type tagCount struct {
	Tag   string
	Count int
}
