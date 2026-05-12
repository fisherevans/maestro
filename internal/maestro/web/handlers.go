package web

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fisherevans/maestro/internal/maestro"
)

// indexData is the view model for the project list page.
type indexData struct {
	Projects []indexProject
}

type indexProject struct {
	Name       string
	RepoPath   string
	SmokeGate  string
	ActiveN    int
	MergedN    int
	TotalN     int
	SessionsN  int
	LastChange time.Time
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	names, err := maestro.ListProjects()
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err)
		return
	}
	view := indexData{}
	for _, n := range names {
		store, err := maestro.NewStore(n)
		if err != nil {
			continue
		}
		st, err := store.Load()
		if err != nil {
			continue
		}
		row := indexProject{
			Name:       st.Project.Name,
			RepoPath:   st.Project.RepoPath,
			SmokeGate:  st.Project.SmokeGate,
			LastChange: st.Updated,
			SessionsN:  len(st.Sessions),
		}
		for _, t := range st.Tasks {
			row.TotalN++
			if t.Status.IsActive() {
				row.ActiveN++
			}
			if t.Status == maestro.StatusMerged {
				row.MergedN++
			}
		}
		view.Projects = append(view.Projects, row)
	}
	sort.Slice(view.Projects, func(i, j int) bool {
		return view.Projects[i].LastChange.After(view.Projects[j].LastChange)
	})
	s.render(w, "index.html", view)
}

// projectData is the view model for a single project page.
type projectData struct {
	Project       *maestro.Project
	State         *maestro.State
	ActiveGroups  []sessionGroup // active sessions with their active tasks nested inside
	OrphanActive  []*maestro.Task // active tasks not tied to any active session (legacy data)
	PastSessions  []sessionRow
	RecentMerged  []*maestro.Task
	Tags          []tagCount
}

// sessionGroup is one unit of the "Active work" view: a session header + the
// active tasks belonging to it. Sessions with no active tasks still render
// (with a "no active tasks" placeholder) so condensation status stays visible.
type sessionGroup struct {
	Session *maestro.Session
	Active  []*maestro.Task
}

type sessionRow struct {
	Session *maestro.Session
	TaskN   int
}

func (s *server) handleProject(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("project")
	store, err := maestro.NewStore(name)
	if err != nil {
		httpErr(w, http.StatusBadRequest, err)
		return
	}
	st, err := store.Load()
	if err != nil {
		httpErr(w, http.StatusNotFound, err)
		return
	}

	view := projectData{
		Project: &st.Project,
		State:   st,
		Tags:    sortedTagCounts(st.AllTags()),
	}

	// Build a map for quick "is this session active?" lookups; sessions that
	// don't exist or have been condensed both fall through to the orphan path.
	activeSessionMap := make(map[string]*maestro.Session)
	for _, sess := range st.Sessions {
		if sess.EndedAt.IsZero() {
			activeSessionMap[sess.ID] = sess
		}
	}

	// Bucket active tasks by their active session. Sessionless tasks (legacy)
	// and tasks whose session has been condensed land in OrphanActive.
	activeTasksBySession := make(map[string][]*maestro.Task)
	for _, t := range st.Tasks {
		if !t.Status.IsActive() {
			continue
		}
		if t.Session == "" {
			view.OrphanActive = append(view.OrphanActive, t)
			continue
		}
		if _, ok := activeSessionMap[t.Session]; !ok {
			view.OrphanActive = append(view.OrphanActive, t)
			continue
		}
		activeTasksBySession[t.Session] = append(activeTasksBySession[t.Session], t)
	}
	for sid := range activeTasksBySession {
		tasks := activeTasksBySession[sid]
		sort.Slice(tasks, func(i, j int) bool {
			return tasks[i].UpdatedAt.After(tasks[j].UpdatedAt)
		})
		activeTasksBySession[sid] = tasks
	}
	sort.Slice(view.OrphanActive, func(i, j int) bool {
		return view.OrphanActive[i].UpdatedAt.After(view.OrphanActive[j].UpdatedAt)
	})

	// Assemble active session groups, sorted by session start desc.
	for _, sess := range st.Sessions {
		if !sess.EndedAt.IsZero() {
			row := sessionRow{Session: sess, TaskN: len(st.TasksInSession(sess.ID))}
			view.PastSessions = append(view.PastSessions, row)
			continue
		}
		view.ActiveGroups = append(view.ActiveGroups, sessionGroup{
			Session: sess,
			Active:  activeTasksBySession[sess.ID],
		})
	}
	sort.Slice(view.ActiveGroups, func(i, j int) bool {
		return view.ActiveGroups[i].Session.StartedAt.After(view.ActiveGroups[j].Session.StartedAt)
	})
	sort.Slice(view.PastSessions, func(i, j int) bool {
		return view.PastSessions[i].Session.EndedAt.After(view.PastSessions[j].Session.EndedAt)
	})

	merged := []*maestro.Task{}
	for _, t := range st.Tasks {
		if t.Status == maestro.StatusMerged {
			merged = append(merged, t)
		}
	}
	sort.Slice(merged, func(i, j int) bool {
		return merged[i].UpdatedAt.After(merged[j].UpdatedAt)
	})
	if len(merged) > 10 {
		merged = merged[:10]
	}
	view.RecentMerged = merged

	s.render(w, "project.html", view)
}

// sessionData is the view model for a session detail page.
type sessionData struct {
	Project *maestro.Project
	Session *maestro.Session
	Tasks   []*maestro.Task
}

func (s *server) handleSession(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("project")
	sid := r.PathValue("session")

	store, err := maestro.NewStore(name)
	if err != nil {
		httpErr(w, http.StatusBadRequest, err)
		return
	}
	st, err := store.Load()
	if err != nil {
		httpErr(w, http.StatusNotFound, err)
		return
	}
	sess := st.FindSession(sid)
	if sess == nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	s.render(w, "session.html", sessionData{
		Project: &st.Project,
		Session: sess,
		Tasks:   st.TasksInSession(sid),
	})
}

// taskData is the view model for a task detail page.
type taskData struct {
	Project *maestro.Project
	Task    *maestro.Task
	Session *maestro.Session
}

func (s *server) handleTask(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("project")
	tid := r.PathValue("task")

	store, err := maestro.NewStore(name)
	if err != nil {
		httpErr(w, http.StatusBadRequest, err)
		return
	}
	st, err := store.Load()
	if err != nil {
		httpErr(w, http.StatusNotFound, err)
		return
	}
	t := st.FindTask(tid)
	if t == nil {
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}
	view := taskData{
		Project: &st.Project,
		Task:    t,
	}
	if t.Session != "" {
		view.Session = st.FindSession(t.Session)
	}
	s.render(w, "task.html", view)
}

// searchData is the view model for the search page.
type searchData struct {
	Project *maestro.Project
	Query   maestro.SearchQuery
	Form    searchForm
	Results []searchResult
	Tags    []tagCount
}

// searchResult pairs a matching task with the field-level match snippets that
// explain why it showed up in the results.
type searchResult struct {
	Task    *maestro.Task
	Matches []searchMatch
}

type searchForm struct {
	Text    string
	Tags    string
	Session string
	Status  string
	Since   string
	Until   string
	Limit   int
}

func (s *server) handleSearch(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("project")
	store, err := maestro.NewStore(name)
	if err != nil {
		httpErr(w, http.StatusBadRequest, err)
		return
	}
	st, err := store.Load()
	if err != nil {
		httpErr(w, http.StatusNotFound, err)
		return
	}

	form := searchForm{
		Text:    r.URL.Query().Get("text"),
		Tags:    r.URL.Query().Get("tags"),
		Session: r.URL.Query().Get("session"),
		Status:  r.URL.Query().Get("status"),
		Since:   r.URL.Query().Get("since"),
		Until:   r.URL.Query().Get("until"),
		Limit:   50,
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			form.Limit = n
		}
	}

	q := maestro.SearchQuery{
		Text:    form.Text,
		Session: form.Session,
		Limit:   form.Limit,
	}
	if form.Tags != "" {
		for _, t := range splitCsv(form.Tags) {
			q.Tags = append(q.Tags, t)
		}
	}
	if form.Status != "" {
		for _, st := range splitCsv(form.Status) {
			q.Statuses = append(q.Statuses, maestro.TaskStatus(st))
		}
	}
	if form.Since != "" {
		if t, err := parseDate(form.Since); err == nil {
			q.Since = t
		}
	}
	if form.Until != "" {
		if t, err := parseDate(form.Until); err == nil {
			q.Until = t
		}
	}

	view := searchData{
		Project: &st.Project,
		Query:   q,
		Form:    form,
		Tags:    sortedTagCounts(st.AllTags()),
	}
	if anyFilterSet(form) {
		tasks := st.SearchTasks(q)
		for _, t := range tasks {
			notes := make([]note, 0, len(t.Notes))
			for _, n := range t.Notes {
				notes = append(notes, note{Source: n.Source, Type: n.Type, Content: n.Content})
			}
			view.Results = append(view.Results, searchResult{
				Task:    t,
				Matches: matchesFor(t.Label, t.Description, t.Summary, t.Tags, notes, form.Text, splitCsv(form.Tags), 160),
			})
		}
	}
	s.render(w, "search.html", view)
}

func anyFilterSet(f searchForm) bool {
	return f.Text != "" || f.Tags != "" || f.Session != "" || f.Status != "" || f.Since != "" || f.Until != ""
}

func splitCsv(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseDate(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Parse("2006-01-02", s)
}

// render executes the named template with the given data, wrapped in layout.html.
func (s *server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	tmpl := s.tmpls.Lookup(name)
	if tmpl == nil {
		httpErr(w, http.StatusInternalServerError, fmt.Errorf("template %s not found", name))
		return
	}
	if err := tmpl.Execute(w, data); err != nil {
		// Best-effort: error may have flushed headers; just log.
		fmt.Fprintf(httpDevNull{}, "render %s: %v\n", name, err)
	}
}

func httpErr(w http.ResponseWriter, code int, err error) {
	http.Error(w, err.Error(), code)
}

// httpDevNull is a no-op io.Writer for swallowing render errors after headers
// have been committed; kept private so callers can't accidentally rely on it.
type httpDevNull struct{}

func (httpDevNull) Write(p []byte) (int, error) { return len(p), nil }
