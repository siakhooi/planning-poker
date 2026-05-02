package main

import (
	"bytes"
	"crypto/rand"
	"embed"
	"fmt"
	"html"
	"html/template"
	"log"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/gorilla/websocket"
)

//go:embed *.html
var tplFS embed.FS

var tmpl = template.Must(template.ParseFS(tplFS, "*.html"))

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true // tighten in production (same-origin, explicit origins)
	},
}

// Hub tracks active WebSocket connections (one browser tab/session) for one room.
type Hub struct {
	mu    sync.Mutex
	conns map[*websocket.Conn]string // display name per connection
}

func newHub() *Hub {
	return &Hub{conns: make(map[*websocket.Conn]string)}
}

func (h *Hub) add(c *websocket.Conn, displayName string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if strings.TrimSpace(displayName) == "" {
		displayName = "Guest"
	}
	h.conns[c] = displayName
	return len(h.conns)
}

func (h *Hub) remove(c *websocket.Conn) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.conns, c)
	return len(h.conns)
}

func (h *Hub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.conns)
}

func (h *Hub) writeTextToAll(payload []byte) {
	h.mu.Lock()
	conns := make([]*websocket.Conn, 0, len(h.conns))
	for c := range h.conns {
		conns = append(conns, c)
	}
	h.mu.Unlock()
	for _, c := range conns {
		if err := c.WriteMessage(websocket.TextMessage, payload); err != nil {
			log.Printf("websocket write: %v", err)
		}
	}
}

// broadcastRoomState sends session count and the sorted list of participant names (room page only).
func (h *Hub) broadcastRoomState() {
	h.mu.Lock()
	n := len(h.conns)
	names := make([]string, 0, len(h.conns))
	for _, name := range h.conns {
		names = append(names, name)
	}
	conns := make([]*websocket.Conn, 0, len(h.conns))
	for c := range h.conns {
		conns = append(conns, c)
	}
	h.mu.Unlock()

	sort.Strings(names)
	var listHTML strings.Builder
	for _, name := range names {
		listHTML.WriteString("<li>")
		listHTML.WriteString(html.EscapeString(name))
		listHTML.WriteString("</li>")
	}

	fragment := fmt.Sprintf(
		`<strong id="session-count" hx-swap-oob="true">%d</strong>`+
			`<ul id="user-list" class="user-list" hx-swap-oob="true">%s</ul>`,
		n, listHTML.String(),
	)

	for _, c := range conns {
		if err := c.WriteMessage(websocket.TextMessage, []byte(fragment)); err != nil {
			log.Printf("websocket write: %v", err)
		}
	}
}

// App holds the home-page hub, per-room hubs, and optional display names for rooms created via the form.
type App struct {
	mu        sync.Mutex
	indexHub  *Hub // connections open on "/" (live session count on the index page)
	roomHubs  map[string]*Hub
	roomNames map[string]string
}

func newApp() *App {
	return &App{
		indexHub:  newHub(),
		roomHubs:  make(map[string]*Hub),
		roomNames: make(map[string]string),
	}
}

// LobbyRoomRow is one row in the index lobby table (html/template requires exported fields).
type LobbyRoomRow struct {
	RoomID      string
	DisplayName string
	Count       int
}

func (a *App) lobbyOverviewOOBHTML() string {
	a.mu.Lock()
	lobbyCount := a.indexHub.count()
	var ids []string
	for id := range a.roomHubs {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var b strings.Builder
	b.WriteString(`<table id="lobby-overview" class="lobby-table" hx-swap-oob="true"><thead><tr><th scope="col">Name</th><th scope="col">Users</th></tr></thead><tbody>`)
	b.WriteString(`<tr><td>Lobby (this page)</td><td><strong id="session-count">`)
	b.WriteString(strconv.Itoa(lobbyCount))
	b.WriteString(`</strong></td></tr>`)
	for _, id := range ids {
		cnt := a.roomHubs[id].count()
		nm := a.roomNames[id]
		disp := nm
		if disp == "" {
			disp = "Room " + id
		} else {
			disp = nm + " · " + id
		}
		b.WriteString(`<tr><td><a href="/`)
		b.WriteString(id)
		b.WriteString(`">`)
		b.WriteString(html.EscapeString(disp))
		b.WriteString(`</a></td><td>`)
		b.WriteString(strconv.Itoa(cnt))
		b.WriteString(`</td></tr>`)
	}
	b.WriteString(`</tbody></table>`)
	a.mu.Unlock()
	return b.String()
}

// broadcastLobbyState pushes the lobby overview table to everyone on the index page WebSocket.
func (a *App) broadcastLobbyState() {
	fragment := a.lobbyOverviewOOBHTML()
	a.indexHub.writeTextToAll([]byte(fragment))
}

func runIndexHubWebSocket(w http.ResponseWriter, r *http.Request, a *App) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("websocket upgrade: %v", err)
		return
	}

	a.indexHub.add(conn, "")
	a.broadcastLobbyState()

	go func() {
		defer func() {
			_ = conn.Close()
			a.indexHub.remove(conn)
			a.broadcastLobbyState()
		}()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
}

func runRoomHubWebSocket(w http.ResponseWriter, r *http.Request, a *App, h *Hub, displayName string) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("websocket upgrade: %v", err)
		return
	}

	if len(displayName) > 120 {
		displayName = displayName[:120]
	}
	h.add(conn, displayName)
	h.broadcastRoomState()
	a.broadcastLobbyState()

	go func() {
		defer func() {
			_ = conn.Close()
			h.remove(conn)
			h.broadcastRoomState()
			a.broadcastLobbyState()
		}()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
}

func randomSixDigitRoomID() string {
	n, err := rand.Int(rand.Reader, big.NewInt(900000))
	if err != nil {
		return "100000"
	}
	return fmt.Sprintf("%06d", int(n.Int64())+100000)
}

func (a *App) getOrCreateHub(roomID string) *Hub {
	a.mu.Lock()
	defer a.mu.Unlock()
	if h, ok := a.roomHubs[roomID]; ok {
		return h
	}
	h := newHub()
	a.roomHubs[roomID] = h
	return h
}

func (a *App) home(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	lobbyCount := a.indexHub.count()
	var ids []string
	for id := range a.roomHubs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	rooms := make([]LobbyRoomRow, 0, len(ids))
	for _, id := range ids {
		cnt := a.roomHubs[id].count()
		nm := a.roomNames[id]
		var disp string
		if nm == "" {
			disp = "Room " + id
		} else {
			disp = nm + " · " + id
		}
		rooms = append(rooms, LobbyRoomRow{RoomID: id, DisplayName: disp, Count: cnt})
	}
	a.mu.Unlock()

	data := struct {
		LobbyCount int
		Rooms      []LobbyRoomRow
	}{LobbyCount: lobbyCount, Rooms: rooms}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "index.html", data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

func (a *App) indexWS(w http.ResponseWriter, r *http.Request) {
	runIndexHubWebSocket(w, r, a)
}

func (a *App) createRoom(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if len(name) > 120 {
		name = name[:120]
	}

	a.mu.Lock()
	var id string
	for range 64 {
		candidate := randomSixDigitRoomID()
		if _, exists := a.roomHubs[candidate]; !exists {
			id = candidate
			break
		}
	}
	if id == "" {
		a.mu.Unlock()
		http.Error(w, "could not allocate room", http.StatusServiceUnavailable)
		return
	}
	a.roomHubs[id] = newHub()
	if name != "" {
		a.roomNames[id] = name
	}
	a.mu.Unlock()

	a.broadcastLobbyState()

	http.Redirect(w, r, "/"+id, http.StatusSeeOther)
}

func (a *App) roomPage(w http.ResponseWriter, r *http.Request) {
	roomID := chi.URLParam(r, "roomID")
	h := a.getOrCreateHub(roomID)
	a.broadcastLobbyState()

	a.mu.Lock()
	name := a.roomNames[roomID]
	a.mu.Unlock()

	data := struct {
		RoomID   string
		RoomName string
		Count    int
	}{
		RoomID:   roomID,
		RoomName: name,
		Count:    h.count(),
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "room.html", data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

func (a *App) roomWS(w http.ResponseWriter, r *http.Request) {
	roomID := chi.URLParam(r, "roomID")
	h := a.getOrCreateHub(roomID)
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	runRoomHubWebSocket(w, r, a, h, name)
}

func main() {
	app := newApp()

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	r.Post("/rooms", app.createRoom)
	r.Get("/ws", app.indexWS)
	r.Get("/ws/{roomID:[0-9]{6}}", app.roomWS)
	r.Get("/{roomID:[0-9]{6}}", app.roomPage)
	r.Get("/", app.home)

	addr := ":8080"
	log.Printf("listening on http://localhost%s", addr)
	if err := http.ListenAndServe(addr, r); err != nil {
		log.Fatal(err)
	}
}
