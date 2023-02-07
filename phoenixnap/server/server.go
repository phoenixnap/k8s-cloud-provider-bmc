package server

import (
	"encoding/json"
	"net/http"

	"github.com/gorilla/mux"
	"github.com/phoenixnap/go-sdk-bmc/billingapi"
	"github.com/phoenixnap/go-sdk-bmc/bmcapi"
	"github.com/phoenixnap/k8s-cloud-provider-bmc/phoenixnap/server/store"
)

// ErrorHandler a handler for errors that can choose to exit or not
// if it wants, it can exit entirely
type ErrorHandler interface {
	Error(error)
}

// Server a handler creator for an http server
type Server struct {
	Store store.DataStore
	ErrorHandler
}

type ErrorResponse struct {
	Response *http.Response
	Code     int    `json:"code"`
	Message  string `json:"message"`
}

// CreateHandler create an http.Handler
func (c *Server) CreateHandler() http.Handler {
	r := mux.NewRouter()
	bmc := r.PathPrefix("/bmc/v1").Subrouter()
	// list all servers
	bmc.HandleFunc("/servers", c.listServersHandler).Methods("GET")
	// get a single server
	bmc.HandleFunc("/servers/{serverID}", c.getServerHandler).Methods("GET")
	// create a server
	bmc.HandleFunc("/servers", c.createServerHandler).Methods("POST")
	// update a server
	bmc.HandleFunc("/servers/{serverID}", c.updateServerHandler).Methods("PATCH")

	billing := r.PathPrefix("/billing/v1").Subrouter()
	// list all products, including server types
	billing.HandleFunc("/products", c.listProductsHandler).Methods("GET")
	// list all locations
	billing.HandleFunc("/locations", c.listLocationsHandler).Methods("GET")
	return r
}

// list all locations
func (c *Server) listLocationsHandler(w http.ResponseWriter, r *http.Request) {
	locations, err := c.Store.ListLocations()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(ErrorResponse{Code: http.StatusInternalServerError, Message: "unable to list locations"})
		return
	}
	var resp = struct {
		locations []string
	}{
		locations: locations,
	}
	if err := writeJSON(w, &resp); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(ErrorResponse{Code: http.StatusInternalServerError, Message: "unable to write json"})
		return
	}
}

// list all plans
func (c *Server) listProductsHandler(w http.ResponseWriter, r *http.Request) {
	products, err := c.Store.ListProducts()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(ErrorResponse{Code: http.StatusInternalServerError, Message: "unable to list products"})
		return
	}
	var resp = struct {
		products []*billingapi.Product
	}{
		products: products,
	}
	if err := writeJSON(w, &resp); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(ErrorResponse{Code: http.StatusInternalServerError, Message: "unable to write json"})
		return
	}
}

// list all servers
func (c *Server) listServersHandler(w http.ResponseWriter, r *http.Request) {
	servers, err := c.Store.ListServers()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(ErrorResponse{Code: http.StatusInternalServerError, Message: "error retrieving servers"})
		return
	}
	var resp = struct {
		Servers []*bmcapi.Server `json:"servers"`
	}{
		Servers: servers,
	}
	if err := writeJSON(w, &resp.Servers); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(ErrorResponse{Code: http.StatusInternalServerError, Message: "unable to write json"})
		return
	}
}

// get information about a specific server
func (c *Server) getServerHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	ID := vars["serverID"]
	server, err := c.Store.GetServer(ID)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(ErrorResponse{Code: http.StatusNotFound, Message: "server not found"})
		return
	}
	if server != nil {
		if err := writeJSON(w, &server); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(ErrorResponse{Code: http.StatusInternalServerError, Message: "unable to write json"})
			return
		}
		return
	}
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(ErrorResponse{Code: http.StatusNotFound, Message: "server not found"})
}

// create a server
func (c *Server) createServerHandler(w http.ResponseWriter, r *http.Request) {
	// read the body of the request
	var req bmcapi.ServerCreate
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(ErrorResponse{Code: http.StatusBadRequest, Message: "cannot parse body of request"})
		return
	}
	server, err := c.Store.CreateServer(req.Hostname, req.Type, req.Location)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(ErrorResponse{Code: http.StatusInternalServerError, Message: "error creating server"})
		return
	}

	if server != nil {
		if err := writeJSON(w, &server); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(ErrorResponse{Code: http.StatusInternalServerError, Message: "unable to write json"})
		}
		return
	}
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(ErrorResponse{Code: http.StatusNotFound, Message: "not found"})
}

// update a server
func (c *Server) updateServerHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	serverID := vars["serverID"]
	// read the body of the request
	var req bmcapi.ServerPatch
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(ErrorResponse{Code: http.StatusBadRequest, Message: "unable to parse body of request"})
		return
	}

	server, err := c.Store.GetServer(serverID)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(ErrorResponse{Code: http.StatusBadRequest, Message: "unknown server ID"})
		return
	}
	if server != nil {
		server.Hostname = *req.Hostname
		server.Description = req.Description
		server.Id = serverID
		if err := c.Store.UpdateServer(server); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(ErrorResponse{Code: http.StatusInternalServerError, Message: "unable to update server"})
			return
		}
		if err := writeJSON(w, &server); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(ErrorResponse{Code: http.StatusInternalServerError, Message: "unable to write json"})
		}
		return
	}
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(ErrorResponse{Code: http.StatusNotFound, Message: "not found"})
}

func writeJSON(w http.ResponseWriter, v any) error {
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(v)
}
