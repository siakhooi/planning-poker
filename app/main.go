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
	"strings"
	"sync"
	"time"

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

// roomIdleEvictionDelay is how long a room with zero WebSocket connections may stay before it is removed.
const roomIdleEvictionDelay = 30 * time.Minute

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
	mu              sync.Mutex
	indexHub        *Hub // connections open on "/" (live session count on the index page)
	roomHubs        map[string]*Hub
	roomNames       map[string]string
	roomEvictTimers map[string]*time.Timer // pending idle-eviction per room
}

func newApp() *App {
	return &App{
		indexHub:        newHub(),
		roomHubs:        make(map[string]*Hub),
		roomNames:       make(map[string]string),
		roomEvictTimers: make(map[string]*time.Timer),
	}
}

// scheduleRoomEvictionLocked starts (or replaces) the idle timer for an empty room. Caller must hold a.mu.
func (a *App) scheduleRoomEvictionLocked(roomID string, h *Hub) {
	if t, ok := a.roomEvictTimers[roomID]; ok {
		t.Stop()
		delete(a.roomEvictTimers, roomID)
	}
	timer := time.AfterFunc(roomIdleEvictionDelay, func() {
		a.evictRoomIfStillEmpty(roomID, h)
	})
	a.roomEvictTimers[roomID] = timer
}

func (a *App) scheduleRoomEviction(roomID string, h *Hub) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.scheduleRoomEvictionLocked(roomID, h)
}

func (a *App) cancelRoomEviction(roomID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if t, ok := a.roomEvictTimers[roomID]; ok {
		t.Stop()
		delete(a.roomEvictTimers, roomID)
	}
}

func (a *App) evictRoomIfStillEmpty(roomID string, h *Hub) {
	a.mu.Lock()
	current, ok := a.roomHubs[roomID]
	if !ok || current != h {
		a.mu.Unlock()
		return
	}
	if h.count() != 0 {
		delete(a.roomEvictTimers, roomID)
		a.mu.Unlock()
		return
	}
	delete(a.roomHubs, roomID)
	delete(a.roomNames, roomID)
	delete(a.roomEvictTimers, roomID)
	a.mu.Unlock()

	log.Printf("removed room after %v idle: %s", roomIdleEvictionDelay, roomID)
	a.broadcastLobbyState()
}

func runRoomHubWebSocket(w http.ResponseWriter, r *http.Request, a *App, roomID string, h *Hub, displayName string) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("websocket upgrade: %v", err)
		return
	}

	if len(displayName) > 120 {
		displayName = displayName[:120]
	}
	h.add(conn, displayName)
	a.cancelRoomEviction(roomID)
	h.broadcastRoomState()
	a.broadcastLobbyState()

	go func() {
		defer func() {
			_ = conn.Close()
			remaining := h.remove(conn)
			h.broadcastRoomState()
			a.broadcastLobbyState()
			if remaining == 0 {
				a.scheduleRoomEviction(roomID, h)
			}
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

func (a *App) getHub(roomID string) (*Hub, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	h, ok := a.roomHubs[roomID]
	return h, ok
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
	a.scheduleRoomEvictionLocked(id, a.roomHubs[id])
	a.mu.Unlock()

	a.broadcastLobbyState()

	http.Redirect(w, r, "/"+id, http.StatusSeeOther)
}

func (a *App) roomPage(w http.ResponseWriter, r *http.Request) {
	roomID := chi.URLParam(r, "roomID")
	h, ok := a.getHub(roomID)
	if !ok {
		data := struct{ RoomID string }{RoomID: roomID}
		var buf bytes.Buffer
		if err := tmpl.ExecuteTemplate(&buf, "room_not_found.html", data); err != nil {
			http.Error(w, "template error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write(buf.Bytes())
		return
	}

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
	h, ok := a.getHub(roomID)
	if !ok {
		http.Error(w, "room not found", http.StatusNotFound)
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	runRoomHubWebSocket(w, r, a, roomID, h, name)
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
