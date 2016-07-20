package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/conas/tno2/util/async"
	"github.com/conas/tno2/util/sec"
	"github.com/conas/tno2/util/str"
	"github.com/conas/tno2/wot/model"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
)

type WSSubscribers struct {
	rwmut        *sync.RWMutex
	subscription map[string]*async.FanOut
}

func NewWSSubscribers() *WSSubscribers {
	return &WSSubscribers{
		rwmut:        &sync.RWMutex{},
		subscription: make(map[string]*async.FanOut),
	}
}

func (wss *WSSubscribers) CreateSubscription(subscriptionID string, clients *async.FanOut) {
	wss.rwmut.Lock()
	defer wss.rwmut.Unlock()

	wss.subscription[subscriptionID] = clients
}

func (wss *WSSubscribers) CancelSubscription(subscriptionID string) {
	wss.rwmut.Lock()
	defer wss.rwmut.Unlock()

	wss.subscription[subscriptionID].RemoveAllSubscribes()
	delete(wss.subscription, subscriptionID)
}

func (wss *WSSubscribers) AddClient(subscriptionID string, client chan<- interface{}) int {
	wss.rwmut.RLock()
	defer wss.rwmut.RUnlock()

	return wss.subscription[subscriptionID].AddSubscriber(client)
}

func (wss *WSSubscribers) RemoveClient(subscriptionID string, clientID int) {
	wss.rwmut.RLock()
	defer wss.rwmut.RUnlock()

	wss.subscription[subscriptionID].RemoveSubscriber(clientID)
}

type Http struct {
	port           int
	router         *mux.Router
	hrefs          []string
	wotServers     map[string]*WotServer
	wssSubscribers *WSSubscribers
	actionResults  *ActionResults
}

// ----- Server API methods

func NewHttp(port int) *Http {
	// r.PathPrefix("/model").HandlerFunc(Model)
	return &Http{
		port:           port,
		router:         mux.NewRouter().StrictSlash(true),
		hrefs:          make([]string, 0),
		wotServers:     make(map[string]*WotServer),
		wssSubscribers: NewWSSubscribers(),
		actionResults:  NewActionResults(),
	}
}

func (p *Http) Bind(ctxPath string, s *WotServer) {
	td := s.GetDescription()
	p.wotServers[ctxPath] = s
	p.createRoutes(ctxPath, td)
	//Update TD uris by created protocol bind
	td.Uris = append(td.Uris, str.Concat("http://localhost:8080", ctxPath))
}

func (p *Http) Start() {
	port := str.Concat(":", strconv.Itoa(p.port))
	log.Fatal(http.ListenAndServe(port, p.router))
}

// ----- ThingDescription parser methods

func (p *Http) createRoutes(ctxPath string, td *model.ThingDescription) {
	p.registerDeviceRoot(ctxPath, td)
	p.registerDeviceDescriptor(ctxPath, td)
	p.registerProperties(ctxPath, td)
	p.registerActions(ctxPath, td)
	p.registerEvents(ctxPath, td)
}

func (p *Http) registerDeviceRoot(ctxPath string, td *model.ThingDescription) {
	p.addRoute(&route{
		name:    "Index",
		method:  "GET",
		pattern: contextPath(ctxPath, ""),
		handlerFunc: func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "Device information for -> %s", td.Name)
		},
	})
}

func (p *Http) registerDeviceDescriptor(ctxPath string, td *model.ThingDescription) {
	p.addRoute(&route{
		name:    "model",
		method:  "GET",
		pattern: contextPath(ctxPath, "description"),
		handlerFunc: func(w http.ResponseWriter, r *http.Request) {
			sendOK(w, td)
		},
	})
}

func (p *Http) registerProperties(ctxPath string, td *model.ThingDescription) {
	for _, prop := range td.Properties {
		p.addRoute(&route{
			name:        prop.Name,
			method:      "GET",
			pattern:     contextPath(ctxPath, prop.Hrefs[0]),
			handlerFunc: p.propertyGetHandler(ctxPath, &prop),
		})

		if prop.Writable {
			p.addRoute(&route{
				name:        prop.Hrefs[0],
				method:      "PUT",
				pattern:     contextPath(ctxPath, prop.Hrefs[0]),
				handlerFunc: p.propertySetHandler(ctxPath, &prop),
			})
		}
	}
}

func (p *Http) registerActions(ctxPath string, td *model.ThingDescription) {
	for _, action := range td.Actions {
		p.addRoute(&route{
			name:        action.Hrefs[0],
			method:      "POST",
			pattern:     contextPath(ctxPath, action.Hrefs[0]),
			handlerFunc: p.actionStartHandler(p.wotServers[ctxPath], action.Name),
		})

		p.addRoute(&route{
			name:        str.Concat(action.Hrefs[0], "Task"),
			method:      "GET",
			pattern:     contextPath(ctxPath, str.Concat(action.Hrefs[0], "/{taskid}")),
			handlerFunc: p.actionTaskHandler,
		})
	}
}

func (p *Http) registerEvents(ctxPath string, td *model.ThingDescription) {
	for _, event := range td.Events {
		p.addRoute(&route{
			name:        event.Hrefs[0],
			method:      "POST",
			pattern:     contextPath(ctxPath, event.Hrefs[0]),
			handlerFunc: p.eventSubscribeHandler(p.wotServers[ctxPath], event.Name),
		})

		p.addRoute(&route{
			name:        str.Concat(event.Hrefs[0], "WebSocket"),
			method:      "GET",
			pattern:     contextPath(ctxPath, str.Concat(event.Hrefs[0], "/ws/{subscriptionID}")),
			handlerFunc: p.eventClientHandler,
		})
	}
}

func (p *Http) propertyGetHandler(ctxPath string, prop *model.Property) func(w http.ResponseWriter, r *http.Request) {
	e := Encoder(prop)

	return func(w http.ResponseWriter, r *http.Request) {
		promise, rc := p.wotServers[ctxPath].GetProperty(prop.Name)

		if rc == OK {
			value := promise.Wait()
			sendOK(w, e(value))
		} else {
			sendERR(w, rc)
		}
	}
}

func (p *Http) propertySetHandler(ctxPath string, prop *model.Property) func(w http.ResponseWriter, r *http.Request) {
	d := Decoder(prop)

	return func(w http.ResponseWriter, r *http.Request) {
		name := prop.Name
		value := d(r.Body)
		promise, rc := p.wotServers[ctxPath].SetProperty(name, value)

		if rc == OK {
			promise.Wait()
		} else {
			sendERR(w, rc)
		}
	}
}

func (p *Http) actionStartHandler(wotServer *WotServer, actionName string) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		//FIXME: fix action request deserialization
		value := WotObject{}
		json.NewDecoder(r.Body).Decode(&value)

		actionID, slot := p.actionResults.CreateSlot()
		ash := NewActionStatusHandler(slot)

		_, rc := wotServer.InvokeAction(actionName, value, ash)

		if rc == OK {
			sendOK(w, httpSubUrl(r, actionID))
		} else {
			sendERR(w, rc)
		}
	}
}

func (p *Http) actionTaskHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	taskid := vars["taskid"]
	slot, rc := p.actionResults.GetSlot(taskid)

	if rc {
		sendOK(w, slot.Load())
	} else {
		sendERR(w, rc)
	}
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

func (p *Http) eventSubscribeHandler(wotServer *WotServer, eventName string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		subscriptionID, _ := sec.UUID4()
		clients := async.NewFanOut()

		p.wssSubscribers.CreateSubscription(subscriptionID, clients)
		wotServer.AddListener(eventName, p.eventHandler(subscriptionID, clients))

		//TODO: check for event existence
		sendOK(w, websocketSubUrl(r, subscriptionID))
	}
}

func (p *Http) eventClientHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)

	if err != nil {
		log.Println("Error creating WebSocket at: ", err)
		return
	}

	vars := mux.Vars(r)
	subscriptionID := vars["subscriptionID"]
	client := make(chan interface{})
	clientID := p.wssSubscribers.AddClient(subscriptionID, client)

	log.Println("Created internal subscriber subscriptionID: ", subscriptionID, " clientID: ", clientID)

	wsOpened := true
	for event := range client {
		if err = conn.WriteJSON(event); err != nil && wsOpened {
			p.wssSubscribers.RemoveClient(subscriptionID, clientID)
			log.Println("Removed internal subscriber subscriptionID: ", subscriptionID, " clientID: ", clientID)
			wsOpened = false
		}
	}
}

type Event struct {
	Timestamp time.Time   `json:"timestamp,omitempty"`
	Event     interface{} `json:"event,omitempty"`
}

func (p *Http) eventHandler(uuid string, clients *async.FanOut) *EventListener {
	el := &EventListener{
		ID: uuid,
		CB: func(event interface{}) {
			clients.Publish(event)
		},
	}

	return el
}

func httpSubUrl(r *http.Request, subresource string) *Links {
	linkString := str.Concat("http://", r.Host, r.URL, "/", subresource)

	link := Link{
		Rel:  "taskid",
		Href: linkString,
	}

	return &Links{
		Links: append(make([]Link, 0), link),
	}
}

func websocketSubUrl(r *http.Request, subresource string) *Links {
	linkString := str.Concat("ws://", r.Host, r.URL, "/ws/", subresource)

	link := Link{
		Rel:  "taskid",
		Href: linkString,
	}

	return &Links{
		Links: append(make([]Link, 0), link),
	}
}

func contextPath(ctxPath, element string) string {
	return str.Concat(ctxPath, "/", element)
}

func sendOK(w http.ResponseWriter, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(payload)
}

func sendERR(w http.ResponseWriter, payload interface{}) {
	w.Header().Set("Content-Type", "text/plain")
	json.NewEncoder(w).Encode(payload)
}

type Links struct {
	Links []Link `json:"links"`
}

type Link struct {
	Rel  string `json:"rel"`
	Href string `json:"href"`
}

type route struct {
	name        string
	method      string
	pattern     string
	handlerFunc http.HandlerFunc
}

func (p *Http) addRoute(route *route) {
	p.router.
		Methods(route.method).
		Path(route.pattern).
		Name(route.name).
		Handler(route.handlerFunc)
}
