package main

import (
	"bytes"
	"html"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

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
