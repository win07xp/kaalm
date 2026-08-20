/*
Copyright 2026 The Kaalm Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package console

import (
	"embed"
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"
	"time"
)

// The pages are the second face of the data layer: every template renders
// exactly the DTOs the corresponding /api/v1 endpoint serves, plus page
// chrome. The swap rule (docs/src/console/overview.md, The Swap Rule): a
// richer frontend replaces these templates, never the API. Zero JavaScript;
// the chat panel is an ordinary form POST.

//go:embed templates/*.html
var templateFS embed.FS

var pageTemplates = template.Must(template.New("").Funcs(template.FuncMap{
	"fmtTime": func(t *time.Time) string {
		if t == nil {
			return ""
		}
		return t.UTC().Format("2006-01-02 15:04:05 MST")
	},
}).ParseFS(templateFS, "templates/*.html"))

// uiRoutes wires the server-rendered pages onto the mux.
func (s *Server) uiRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /login", s.uiLoginForm)
	mux.HandleFunc("POST /login", s.uiLogin)
	mux.HandleFunc("POST /logout", s.uiLogout)
	mux.HandleFunc("GET /{$}", s.requireUI(s.uiHome))
	mux.HandleFunc("GET /ns/{ns}", s.requireUI(s.uiNamespace))
	mux.HandleFunc("GET /ns/{ns}/agents/{name}", s.requireUI(s.uiAgent))
	mux.HandleFunc("POST /ns/{ns}/agents/{name}/chat", s.requireUI(s.uiChat))
}

// requireUI resolves the session cookie and redirects to the login page when
// there is none. The pages authenticate by session only; per-request bearer
// tokens are the JSON API's mode.
func (s *Server) requireUI(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		id, ok := s.Sessions.Resolve(r.Context(), c.Value)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next(w, r.WithContext(withIdentity(r.Context(), id)))
	}
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTemplates.ExecuteTemplate(w, name, data); err != nil {
		slog.Error("template render failed", "template", name, "err", err)
	}
}

type loginPage struct {
	User  string // always empty: the header shows no logout before login
	Error string
}

func (s *Server) uiLoginForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, "login.html", loginPage{})
}

func (s *Server) uiLogin(w http.ResponseWriter, r *http.Request) {
	token := r.PostFormValue("token")
	if token == "" {
		s.render(w, "login.html", loginPage{Error: "paste a token"})
		return
	}
	value, id, err := s.Sessions.Create(r.Context(), token)
	if err != nil {
		slog.Info("login rejected", "err", err)
		s.render(w, "login.html", loginPage{Error: "the cluster did not accept that token"})
		return
	}
	slog.Info("login", "userId", id.Username)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: value, Path: "/",
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
		MaxAge: int(sessionMaxAge.Seconds()),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) uiLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.Sessions.Delete(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

type homePage struct {
	User       string
	Namespaces []string
}

func (s *Server) uiHome(w http.ResponseWriter, r *http.Request) {
	id := identityFrom(r.Context())
	all, err := s.Data.Namespaces(r.Context())
	if err != nil {
		http.Error(w, "listing namespaces failed", http.StatusServiceUnavailable)
		return
	}
	page := homePage{User: id.Username}
	for _, ns := range all {
		if ok, err := s.Gate.CanView(r.Context(), id, ns); err == nil && ok {
			page.Namespaces = append(page.Namespaces, ns)
		}
	}
	s.render(w, "namespaces.html", page)
}

type namespacePage struct {
	User      string
	Namespace string
	Fleet     []FleetRow
	Spend     []SpendRow
	Workloads []WorkloadSpend
	Tasks     []TaskRow
	Channels  []ChannelRow
}

// gateView authorizes one namespace view for a page, writing the denial.
func (s *Server) gateView(w http.ResponseWriter, r *http.Request, ns string) bool {
	id := identityFrom(r.Context())
	allowed, err := s.Gate.CanView(r.Context(), id, ns)
	if err != nil {
		http.Error(w, "authorization check failed", http.StatusServiceUnavailable)
		return false
	}
	if !allowed {
		http.Error(w, "you are not allowed to view this namespace", http.StatusForbidden)
		return false
	}
	return true
}

func (s *Server) uiNamespace(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("ns")
	if !s.gateView(w, r, ns) {
		return
	}
	page := namespacePage{User: identityFrom(r.Context()).Username, Namespace: ns}
	var err error
	if page.Fleet, err = s.Data.Fleet(r.Context(), ns); err != nil {
		http.Error(w, "reading the namespace failed", http.StatusServiceUnavailable)
		return
	}
	page.Spend, _ = s.Data.Spend(r.Context(), ns)
	page.Workloads = s.workloadSpend(r.Context(), ns)
	page.Tasks, _ = s.Data.Tasks(r.Context(), ns)
	page.Channels, _ = s.Data.Channels(r.Context(), ns)
	s.render(w, "namespace.html", page)
}

type agentPage struct {
	User      string
	Namespace string
	Agent     AgentDetail
	Reply     string
	ChatError string
}

func (s *Server) uiAgent(w http.ResponseWriter, r *http.Request) {
	s.renderAgent(w, r, "", "")
}

func (s *Server) renderAgent(w http.ResponseWriter, r *http.Request, reply, chatError string) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	if !s.gateView(w, r, ns) {
		return
	}
	detail, found, err := s.Data.Agent(r.Context(), ns, name)
	if err != nil {
		http.Error(w, "reading the agent failed", http.StatusServiceUnavailable)
		return
	}
	if !found {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	detail.Spend = agentSpendRows(s.workloadSpend(r.Context(), ns), detail.Name)
	s.render(w, "agent.html", agentPage{
		User: identityFrom(r.Context()).Username, Namespace: ns,
		Agent: detail, Reply: reply, ChatError: chatError,
	})
}

// uiChat runs one test chat from the form and re-renders the agent page with
// the reply or the error. Content is never logged.
func (s *Server) uiChat(w http.ResponseWriter, r *http.Request) {
	id := identityFrom(r.Context())
	ns, name := r.PathValue("ns"), r.PathValue("name")

	allowed, err := s.Gate.CanChat(r.Context(), id, ns)
	if err != nil {
		http.Error(w, "authorization check failed", http.StatusServiceUnavailable)
		return
	}
	if !allowed {
		s.renderAgent(w, r, "", "test-chat requires permission to create channels in this namespace")
		return
	}
	content := r.PostFormValue("content")
	if content == "" {
		s.renderAgent(w, r, "", "type a message first")
		return
	}

	start := time.Now()
	status, body, err := s.Gateway.Chat(r.Context(), ns, name, id.Username, content)
	if err != nil {
		slog.Error("test-chat gateway call failed", "namespace", ns, "agent", name, "userId", id.Username, "err", err)
		s.renderAgent(w, r, "", "the gateway could not be reached")
		return
	}
	slog.Info("test-chat", "namespace", ns, "agent", name, "userId", id.Username,
		"status", status, "durationMs", time.Since(start).Milliseconds())

	if status == http.StatusOK {
		var envelope struct {
			Content string `json:"content"`
		}
		if json.Unmarshal(body, &envelope) == nil && envelope.Content != "" {
			s.renderAgent(w, r, envelope.Content, "")
			return
		}
		s.renderAgent(w, r, "", "the agent returned an unreadable reply")
		return
	}
	var gwErr apiError
	if json.Unmarshal(body, &gwErr) == nil && gwErr.Error.Type != "" {
		s.renderAgent(w, r, "", "delivery failed: "+gwErr.Error.Type)
		return
	}
	s.renderAgent(w, r, "", "delivery failed")
}
