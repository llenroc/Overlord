// Copyright 2015 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package overlord

import (
	"code.google.com/p/go-uuid/uuid"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/googollee/go-socket.io"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

const (
	WEBSERVER_ADDR    = "localhost:9000"
	LD_INTERVAL       = 5
	KEEP_ALIVE_PERIOD = 1
)

type SpawnTerminalCmd struct {
	Sid string
}

type SpawnShellCmd struct {
	Sid     string
	Command string
}

type ConnectLogcatCmd struct {
	Conn *websocket.Conn
}

type TerminateCmd struct {
}

// WebSocketContext is used for maintaining the session information of
// WebSocket requests. When requests come from Web Server, we create a new
// WebSocketConext to store the session ID and WebSocket connection. ConnServer
// will request a new terminal connection with client ID equals the session ID.
// This way, the ConnServer can retreive the connresponding WebSocketContext
// with it's client (session) ID and get the WebSocket.
type WebSocketContext struct {
	Sid  string
	Conn *websocket.Conn
}

func NewWebsocketContext(conn *websocket.Conn) *WebSocketContext {
	return &WebSocketContext{
		Sid:  uuid.New(),
		Conn: conn,
	}
}

type Overlord struct {
	agents   map[string]*ConnServer            // Normal ghost agents
	logcats  map[string]map[string]*ConnServer // logcat clients
	wsctxs   map[string]*WebSocketContext      // (cid, WebSocketContext) mapping
	ioserver *socketio.Server
}

func NewOverlord() *Overlord {
	return &Overlord{
		agents:  make(map[string]*ConnServer),
		logcats: make(map[string]map[string]*ConnServer),
		wsctxs:  make(map[string]*WebSocketContext),
	}
}

// Register a client.
func (self *Overlord) Register(conn *ConnServer) (*websocket.Conn, error) {
	msg, err := json.Marshal(map[string]interface{}{
		"mid": conn.Mid,
		"cid": conn.Cid,
	})
	if err != nil {
		return nil, err
	}

	var wsconn *websocket.Conn

	switch conn.Mode {
	case AGENT:
		if _, ok := self.agents[conn.Mid]; ok {
			return nil, errors.New("Register: duplicate machine ID: " + conn.Mid)
		}

		self.agents[conn.Mid] = conn
		self.ioserver.BroadcastTo("monitor", "agent joined", string(msg))
	case TERMINAL, SHELL:
		if ctx, ok := self.wsctxs[conn.Cid]; !ok {
			return nil, errors.New("Register: client " + conn.Cid +
				" registered without context")
		} else {
			wsconn = ctx.Conn
		}
	case LOGCAT:
		if _, ok := self.logcats[conn.Mid]; !ok {
			self.logcats[conn.Mid] = make(map[string]*ConnServer)
		}
		if _, ok := self.logcats[conn.Mid][conn.Cid]; ok {
			return nil, errors.New("Register: duplicate client ID: " + conn.Cid)
		}
		self.logcats[conn.Mid][conn.Cid] = conn
		self.ioserver.BroadcastTo("monitor", "logcat joined", string(msg))
	default:
		return nil, errors.New("Register: Unknown client mode")
	}

	var id string
	if conn.Mode == AGENT {
		id = conn.Mid
	} else {
		id = conn.Cid
	}

	log.Printf("%s %s registered\n", ModeStr(conn.Mode), id)

	return wsconn, nil
}

// Unregister a client.
func (self *Overlord) Unregister(conn *ConnServer) {
	msg, err := json.Marshal(map[string]interface{}{
		"mid": conn.Mid,
		"cid": conn.Cid,
	})

	if err != nil {
		panic(err)
	}

	switch conn.Mode {
	case AGENT:
		self.ioserver.BroadcastTo("monitor", "agent left", string(msg))
		delete(self.agents, conn.Mid)
	case LOGCAT:
		if _, ok := self.logcats[conn.Mid]; ok {
			self.ioserver.BroadcastTo("monitor", "logcat left", string(msg))
			delete(self.logcats[conn.Mid], conn.Cid)
			if len(self.logcats[conn.Mid]) == 0 {
				delete(self.logcats, conn.Mid)
			}
		}
	default:
		if _, ok := self.wsctxs[conn.Cid]; ok {
			delete(self.wsctxs, conn.Cid)
		}
	}

	var id string
	if conn.Mode == AGENT {
		id = conn.Mid
	} else {
		id = conn.Cid
	}
	log.Printf("%s %s unregistered\n", ModeStr(conn.Mode), id)
}

func (self *Overlord) AddWebsocketContext(wc *WebSocketContext) {
	self.wsctxs[wc.Sid] = wc
}

// Handle TCP Connection.
func (self *Overlord) handleConnection(conn net.Conn) {
	handler := NewConnServer(self, conn)
	go handler.Listen()
}

// Socket server main routine.
func (self *Overlord) ServSocket(port int) {
	addrStr := fmt.Sprintf("0.0.0.0:%d", port)
	addr, err := net.ResolveTCPAddr("tcp", addrStr)
	if err != nil {
		panic(err)
	}
	ln, err := net.ListenTCP("tcp", addr)
	if err != nil {
		panic(err)
	}
	log.Printf("Overlord started, listening at %s", addr)
	for {
		conn, err := ln.AcceptTCP()
		if err != nil {
			panic(err)
		}
		log.Printf("Incomming connection from %s\n", conn.RemoteAddr())
		conn.SetKeepAlive(true)
		conn.SetKeepAlivePeriod(KEEP_ALIVE_PERIOD * time.Second)
		self.handleConnection(conn)
	}
}

// Initialize the Socket.io server.
func (self *Overlord) InitSocketIOServer() {
	server, err := socketio.NewServer(nil)
	if err != nil {
		log.Fatal(err)
	}

	server.On("connection", func(so socketio.Socket) {
		so.Join("monitor")
	})

	server.On("error", func(so socketio.Socket, err error) {
		log.Println("error:", err)
	})

	self.ioserver = server
}

// Web server main routine.
func (self *Overlord) ServHTTP(addr, app string) {
	var upgrader = websocket.Upgrader{
		ReadBufferSize:  BUFSIZ,
		WriteBufferSize: BUFSIZ,
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}

	// Helper function for writing error message to WebSocket
	WebSocketSendError := func(ws *websocket.Conn, err string) {
		log.Println(err)
		msg := websocket.FormatCloseMessage(websocket.CloseProtocolError, err)
		ws.WriteMessage(websocket.CloseMessage, msg)
		ws.Close()
	}

	// PTY stream request handler.
	// We first create a WebSocketContext to store the connection, then send a
	// command to Overlord to client to spawn a terminal connection.
	PtyHandler := func(w http.ResponseWriter, r *http.Request) {
		log.Printf("Terminal request from %s\n", r.RemoteAddr)

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			panic(err)
		}

		vars := mux.Vars(r)
		mid := vars["mid"]
		if agent, ok := self.agents[mid]; ok {
			wc := NewWebsocketContext(conn)
			self.AddWebsocketContext(wc)
			agent.Bridge <- SpawnTerminalCmd{wc.Sid}
		} else {
			WebSocketSendError(conn, "No client with mid "+mid)
		}
	}

	// Shell command request handler.
	// We first create a WebSocketContext to store the connection, then send a
	// command to ConnServer to client to spawn a shell connection.
	ShellHandler := func(w http.ResponseWriter, r *http.Request) {
		log.Printf("Shell request from %s\n", r.RemoteAddr)

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			panic(err)
		}

		vars := mux.Vars(r)
		mid := vars["mid"]
		if agent, ok := self.agents[mid]; ok {
			if command, ok := r.URL.Query()["command"]; ok {
				wc := NewWebsocketContext(conn)
				self.AddWebsocketContext(wc)
				agent.Bridge <- SpawnShellCmd{wc.Sid, command[0]}
			} else {
				WebSocketSendError(conn, "No command specified for shell request "+mid)
			}
		} else {
			WebSocketSendError(conn, "No client with mid "+mid)
		}
	}

	// Logcat request handler.
	// We directly send the WebSocket connection to ConnServer for forwarding
	// the log stream.
	LogcatHandler := func(w http.ResponseWriter, r *http.Request) {
		log.Printf("Logcat request from %s\n", r.RemoteAddr)

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			panic(err)
		}

		vars := mux.Vars(r)
		mid := vars["mid"]
		cid := vars["cid"]

		if logcats, ok := self.logcats[mid]; ok {
			if logcat, ok := logcats[cid]; ok {
				logcat.Bridge <- ConnectLogcatCmd{conn}
			} else {
				WebSocketSendError(conn, "No client with cid "+cid)
			}
		} else {
			WebSocketSendError(conn, "No client with mid "+mid)
		}
	}

	// Get agent properties as JSON
	AgentPropertiesHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		vars := mux.Vars(r)
		mid := vars["mid"]
		jsonResult, err := json.Marshal(self.agents[mid].Properties)
		if err != nil {
			w.Write([]byte(`{"error": "` + err.Error() + `"}`))
			return
		}
		w.Write(jsonResult)
	}

	// List all agents connected to the Overlord.
	AgentsListHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		data := make([]map[string]string, len(self.agents))
		idx := 0
		for _, agent := range self.agents {
			data[idx] = map[string]string{
				"mid": agent.Mid,
				"cid": agent.Cid,
			}
			idx++
		}

		result, err := json.Marshal(data)
		if err != nil {
			w.Write([]byte(fmt.Sprintf(`{"error", "%s"}`, err.Error())))
		} else {
			w.Write(result)
		}
	}

	// List all logcat clients connected to the Overlord.
	LogcatsListHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		data := make([]map[string]interface{}, len(self.logcats))
		idx := 0
		for mid, logcats := range self.logcats {
			var cids []string
			for cid, _ := range logcats {
				cids = append(cids, cid)
			}
			data[idx] = map[string]interface{}{
				"mid":  mid,
				"cids": cids,
			}
			idx++
		}

		result, err := json.Marshal(data)
		if err != nil {
			w.Write([]byte(fmt.Sprintf(`{"error", "%s"}`, err.Error())))
		} else {
			w.Write(result)
		}
	}

	appDir := filepath.Join(filepath.Dir(os.Args[0]), "app", app)
	if _, err := os.Stat(appDir); os.IsNotExist(err) {
		log.Fatalf("App `%s' does not exist\n", app)
	}

	self.InitSocketIOServer()

	// Register the request handlers and start the WebServer.
	r := mux.NewRouter()
	// Logcat
	r.HandleFunc("/api/log/{mid}/{cid}", LogcatHandler)

	// Agent methods
	r.HandleFunc("/api/agent/pty/{mid}", PtyHandler)
	r.HandleFunc("/api/agent/shell/{mid}", ShellHandler)
	r.HandleFunc("/api/agent/properties/{mid}", AgentPropertiesHandler)

	r.HandleFunc("/api/agents/list", AgentsListHandler)
	r.HandleFunc("/api/logcats/list", LogcatsListHandler)

	http.Handle("/api/", r)
	http.Handle("/api/socket.io/", self.ioserver)
	http.Handle("/", http.FileServer(http.Dir(appDir)))

	err := http.ListenAndServe(WEBSERVER_ADDR, nil)
	if err != nil {
		log.Fatalf("net.http could not listen on address '%s': %s\n",
			WEBSERVER_ADDR, err)
	}
}

// Broadcast LAN discovery message.
func (self *Overlord) StartUDPBroadcast(port int) {
	conn, err := net.Dial("udp", fmt.Sprintf("%s:%d", net.IPv4bcast, port))
	if err != nil {
		panic(err)
	}

	ticker := time.NewTicker(time.Duration(LD_INTERVAL * time.Second))

	for {
		select {
		case <-ticker.C:
			conn.Write([]byte(fmt.Sprintf("OVERLORD :%d", OVERLORD_PORT)))
		}
	}
}

func (self *Overlord) Serv(app string) {
	go self.ServSocket(OVERLORD_PORT)
	go self.ServHTTP(WEBSERVER_ADDR, app)
	go self.StartUDPBroadcast(OVERLORD_LD_PORT)

	ticker := time.NewTicker(time.Duration(60 * time.Second))

	for {
		select {
		case <-ticker.C:
			log.Printf("#Goroutines, #Ghostclient: %d, %d\n",
				runtime.NumGoroutine(), len(self.agents))
		}
	}
}

func StartOverlord(app string) {
	ovl := NewOverlord()
	ovl.Serv(app)
}