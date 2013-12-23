// This file is part of Monsti, a web content management system.
// Copyright 2012-2013 Christian Neumann
//
// Monsti is free software: you can redistribute it and/or modify it under the
// terms of the GNU Affero General Public License as published by the Free
// Software Foundation, either version 3 of the License, or (at your option) any
// later version.
//
// Monsti is distributed in the hope that it will be useful, but WITHOUT ANY
// WARRANTY; without even the implied warranty of MERCHANTABILITY or FITNESS FOR
// A PARTICULAR PURPOSE.  See the GNU Affero General Public License for more
// details.
//
// You should have received a copy of the GNU Affero General Public License
// along with Monsti.  If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"runtime/debug"
	"strings"

	"github.com/gorilla/context"
	"github.com/gorilla/sessions"
	"pkg.monsti.org/gettext"
	"pkg.monsti.org/service"
	"pkg.monsti.org/util"
	"pkg.monsti.org/util/template"
)

// Context holds information about a request
type reqContext struct {
	Res         http.ResponseWriter
	Req         *http.Request
	Node        *service.NodeInfo
	Action      string
	Session     *sessions.Session
	UserSession *service.UserSession
	Site        *util.SiteSettings
	Serv        *service.Session
}

// nodeHandler is a net/http handler to process incoming HTTP requests.
type nodeHandler struct {
	Renderer template.Renderer
	Settings *settings
	// Hosts is a map from hosts to site names.
	Hosts map[string]string
	// Log is the logger used by the node handler.
	Log *log.Logger
	// Info is a connection to an INFO service.
	Info     *service.InfoClient
	Sessions *service.SessionPool
}

// splitAction splits and returns the path and @@action of the given URL.
func splitAction(path string) (string, string) {
	tokens := strings.Split(path, "/")
	last := tokens[len(tokens)-1]
	var action string
	if len(last) > 2 && last[:2] == "@@" {
		action = last[2:]
	}
	nodePath := path
	if len(action) > 0 {
		nodePath = path[:len(path)-(len(action)+3)]
		if len(nodePath) == 0 {
			nodePath = "/"
		}
	}
	return nodePath, action
}

// ServeHTTP handles incoming HTTP requests.
func (h *nodeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c := reqContext{Res: w, Req: r}
	defer func() {
		if err := recover(); err != nil {
			var buf bytes.Buffer
			fmt.Fprintf(&buf, "panic: %v\n", err)
			buf.Write(debug.Stack())
			h.Log.Println(buf.String())
			http.Error(c.Res, "Application error.",
				http.StatusInternalServerError)
		}
	}()
	var err error
	c.Serv, err = h.Sessions.New()
	if err != nil {
		panic(fmt.Errorf("Could not get session: %v", err))
	}
	defer h.Sessions.Free(c.Serv)
	var nodePath string
	nodePath, c.Action = splitAction(c.Req.URL.Path)
	if len(c.Action) == 0 && nodePath[len(nodePath)-1] != '/' {
		newPath, err := url.Parse(nodePath + "/")
		if err != nil {
			panic("Could not parse request URL:" + err.Error())
		}
		url := c.Req.URL.ResolveReference(newPath)
		http.Redirect(c.Res, c.Req, url.String(), http.StatusSeeOther)
		return
	}
	site_name, ok := h.Hosts[c.Req.Host]
	if !ok {
		panic("No site found for host " + c.Req.Host)
	}
	site := h.Settings.Monsti.Sites[site_name]
	c.Site = &site
	c.Site.Name = site_name
	c.Session = getSession(c.Req, *c.Site)
	defer context.Clear(c.Req)
	c.UserSession = getClientSession(c.Session,
		h.Settings.Monsti.GetSiteConfigPath(c.Site.Name))
	c.UserSession.Locale = c.Site.Locale
	c.Node, err = c.Serv.Data().GetNode(c.Site.Name, nodePath)
	if err != nil {
		h.Log.Printf("Node not found: %v", err)
		c.Node = &service.NodeInfo{Path: nodePath}
		h.DisplayError(http.StatusNotFound, &c)
		return
	}
	if !checkPermission(c.Action, c.UserSession) {
		http.Error(w, "Unauthorized.", http.StatusUnauthorized)
		return
	}
	switch c.Action {
	case "login":
		h.Login(&c)
	case "logout":
		h.Logout(&c)
	case "add":
		h.Add(&c)
	case "remove":
		h.Remove(&c)
	default:
		h.RequestNode(&c)
	}
}

// DisplayError shows an error page to the user.
func (h *nodeHandler) DisplayError(HTTPErr int, c *reqContext) {
	http.Error(c.Res, "Document not found", HTTPErr)
}

// RequestNode handles node requests.
func (h *nodeHandler) RequestNode(c *reqContext) {
	// Setup ticket and send to workers.
	h.Log.Print(c.Site.Name, c.Req.Method, c.Req.URL.Path)

	nodeServ, err := h.Info.FindNodeService(c.Node.Type)
	if err != nil {
		panic(fmt.Sprintf("Could not find node service for %q at %q: %v", c.Node.Type, err))
	}
	if err = c.Req.ParseMultipartForm(1024 * 1024); err != nil {
		panic(fmt.Sprintf("Could not parse form: %v", err))
	}
	req := service.Request{
		Site:     c.Site.Name,
		Method:   c.Req.Method,
		Node:     *c.Node,
		Query:    c.Req.URL.Query(),
		Session:  *c.UserSession,
		Action:   c.Action,
		FormData: c.Req.Form,
	}

	// Attach request files
	if c.Req.MultipartForm != nil {
		if len(c.Req.MultipartForm.File) > 0 {
			req.Files = make(map[string][]service.RequestFile)
		}
		for name, fileHeaders := range c.Req.MultipartForm.File {
			if _, ok := req.Files[name]; !ok {
				req.Files[name] = make([]service.RequestFile, 0)
			}
			for _, fileHeader := range fileHeaders {
				file, err := fileHeader.Open()
				if err != nil {
					panic("Could not open multipart file header: " + err.Error())
				}
				if osFile, ok := file.(*os.File); ok {
					req.Files[name] = append(req.Files[name], service.RequestFile{
						TmpFile: osFile.Name()})
				} else {
					content, err := ioutil.ReadAll(file)
					if err != nil {
						panic("Could not read multipart file: " + err.Error())
					}
					req.Files[name] = append(req.Files[name], service.RequestFile{
						Content: content})
				}
			}
		}
	}

	res, err := nodeServ.Request(&req)
	if err != nil {
		panic(fmt.Sprintf("Could not request node: %v", err))
	}

	G, _, _, _ := gettext.DefaultLocales.Use("monsti-httpd", c.UserSession.Locale)
	if len(res.Body) == 0 && len(res.Redirect) == 0 {
		panic("Got empty response.")
	}
	if res.Node != nil {
		oldPath := c.Node.Path
		c.Node = res.Node
		c.Node.Path = oldPath
	}
	if len(res.Redirect) > 0 {
		http.Redirect(c.Res, c.Req, res.Redirect, http.StatusSeeOther)
		return
	}
	env := masterTmplEnv{Node: c.Node, Session: c.UserSession}
	if c.Action == "edit" {
		env.Title = fmt.Sprintf(G("Edit \"%s\""), c.Node.Title)
		env.Flags = EDIT_VIEW
	}
	var content []byte
	if res.Raw {
		content = res.Body
	} else {
		content = []byte(renderInMaster(h.Renderer, res.Body, env, h.Settings,
			*c.Site, c.UserSession.Locale, c.Serv))
	}
	err = c.Session.Save(c.Req, c.Res)
	if err != nil {
		panic(err.Error())
	}
	c.Res.Write(content)
}
