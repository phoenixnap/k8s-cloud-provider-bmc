// mockapi is a minimal, in-memory stand-in for the PhoenixNAP APIs used by the CCM,
// for end-to-end tests in a local cluster. It is not a faithful emulation of the API.
//
// Servers are declared with the SERVERS env var as a comma-separated list of
// hostname=privateIP pairs, matching the names of the cluster nodes.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	serverPrefix  = "/bmc/v1/servers"
	tagsPath      = "/tag-manager/v1/tags"
	blocksPrefix  = "/ips/v1/ip-blocks"
	networkPrefix = "/networks/v1/public-networks/"
)

// tagAssignment carries every field the ipapi SDK requires when decoding a tag on a block
type tagAssignment struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	Value        *string `json:"value,omitempty"`
	IsBillingTag bool    `json:"isBillingTag"`
}

// assignTags fills in the fields the API adds to tags it stores; tag names double as IDs here
func assignTags(tags []tagAssignment) []tagAssignment {
	for i := range tags {
		tags[i].ID = tags[i].Name
	}
	return tags
}

type ipBlock struct {
	ID                   string          `json:"id"`
	Location             string          `json:"location"`
	CidrBlockSize        string          `json:"cidrBlockSize"`
	Cidr                 string          `json:"cidr"`
	Status               string          `json:"status"`
	AssignedResourceID   *string         `json:"assignedResourceId,omitempty"`
	AssignedResourceType *string         `json:"assignedResourceType,omitempty"`
	Tags                 []tagAssignment `json:"tags,omitempty"`
	CreatedOn            time.Time       `json:"createdOn"`
}

// server carries every field the bmcapi SDK requires when decoding a server
type server struct {
	ID                   string         `json:"id"`
	Status               string         `json:"status"`
	Hostname             string         `json:"hostname"`
	Type                 string         `json:"type"`
	Location             string         `json:"location"`
	CPU                  string         `json:"cpu"`
	CPUCount             int            `json:"cpuCount"`
	CoresPerCPU          int            `json:"coresPerCpu"`
	CPUFrequency         float32        `json:"cpuFrequency"`
	RAM                  string         `json:"ram"`
	Storage              string         `json:"storage"`
	PricingModel         string         `json:"pricingModel"`
	PrivateIPAddresses   []string       `json:"privateIpAddresses"`
	PublicIPAddresses    []string       `json:"publicIpAddresses"`
	NetworkConfiguration map[string]any `json:"networkConfiguration"`
	StorageConfiguration map[string]any `json:"storageConfiguration"`
}

type mock struct {
	mu      sync.Mutex
	servers []server
	tags    map[string]bool
	blocks  map[string]*ipBlock
	nextID  int
	// requests is a log of "METHOD path" for every API call, for assertions
	requests []string
}

func main() {
	m := &mock{tags: map[string]bool{}, blocks: map[string]*ipBlock{}}
	for i, entry := range strings.Split(os.Getenv("SERVERS"), ",") {
		name, ip, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		m.servers = append(m.servers, server{
			ID:                   fmt.Sprintf("server-%d", i),
			Status:               "powered-on",
			Hostname:             name,
			Type:                 "s1.c1.small",
			Location:             "ASH",
			CPU:                  "E-2134",
			CPUCount:             1,
			CoresPerCPU:          4,
			CPUFrequency:         3.5,
			RAM:                  "32GB",
			Storage:              "1x 1TB NVMe",
			PricingModel:         "HOURLY",
			PrivateIPAddresses:   []string{ip},
			NetworkConfiguration: map[string]any{},
			StorageConfiguration: map[string]any{},
		})
	}
	addr := ":8080"
	if a := os.Getenv("ADDR"); a != "" {
		addr = a
	}
	log.Printf("mock PhoenixNAP API listening on %s with %d servers", addr, len(m.servers))
	log.Fatal(http.ListenAndServe(addr, m))
}

func (m *mock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	path := strings.TrimSuffix(r.URL.Path, "/")
	if path != "/state" {
		m.requests = append(m.requests, r.Method+" "+path)
		log.Printf("%s %s?%s", r.Method, path, r.URL.RawQuery)
	}

	switch {
	case path == "/token" && r.Method == http.MethodPost:
		writeJSON(w, map[string]any{"access_token": "e2e", "token_type": "Bearer", "expires_in": 3600})
	case path == "/state" && r.Method == http.MethodGet:
		blocks := []*ipBlock{}
		for _, b := range m.blocks {
			blocks = append(blocks, b)
		}
		writeJSON(w, map[string]any{"blocks": blocks, "requests": m.requests})

	case path == serverPrefix && r.Method == http.MethodGet:
		writeJSON(w, m.servers)
	case strings.HasPrefix(path, serverPrefix+"/") && r.Method == http.MethodGet:
		id := strings.TrimPrefix(path, serverPrefix+"/")
		for _, s := range m.servers {
			if s.ID == id {
				writeJSON(w, s)
				return
			}
		}
		writeError(w, http.StatusNotFound, "server not found")

	case path == tagsPath && r.Method == http.MethodGet:
		tags := []map[string]any{}
		for name := range m.tags {
			tags = append(tags, map[string]any{"id": name, "name": name, "isBillingTag": false})
		}
		writeJSON(w, tags)
	case path == tagsPath && r.Method == http.MethodPost:
		var req struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		m.tags[req.Name] = true
		writeJSON(w, map[string]any{"id": req.Name, "name": req.Name, "isBillingTag": false})

	case path == blocksPrefix && r.Method == http.MethodGet:
		writeJSON(w, m.filterBlocks(r.URL.Query()["tag"]))
	case path == blocksPrefix && r.Method == http.MethodPost:
		var req struct {
			Location      string          `json:"location"`
			CidrBlockSize string          `json:"cidrBlockSize"`
			Tags          []tagAssignment `json:"tags"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		m.nextID++
		b := &ipBlock{
			ID:            fmt.Sprintf("block-%d", m.nextID),
			Location:      req.Location,
			CidrBlockSize: req.CidrBlockSize,
			// 198.51.100.0/24 is TEST-NET-2, carved into /29s
			Cidr:      fmt.Sprintf("198.51.100.%d/29", (m.nextID%32)*8),
			Status:    "unassigned",
			Tags:      assignTags(req.Tags),
			CreatedOn: time.Now().UTC(),
		}
		m.blocks[b.ID] = b
		writeJSON(w, b)
	case strings.HasPrefix(path, blocksPrefix+"/") && strings.HasSuffix(path, "/tags") && r.Method == http.MethodPut:
		b := m.blocks[strings.TrimSuffix(strings.TrimPrefix(path, blocksPrefix+"/"), "/tags")]
		if b == nil {
			writeError(w, http.StatusNotFound, "ip block not found")
			return
		}
		var tags []tagAssignment
		if err := json.NewDecoder(r.Body).Decode(&tags); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		b.Tags = assignTags(tags)
		writeJSON(w, b)
	case strings.HasPrefix(path, blocksPrefix+"/") && r.Method == http.MethodDelete:
		id := strings.TrimPrefix(path, blocksPrefix+"/")
		b := m.blocks[id]
		switch {
		case b == nil:
			writeError(w, http.StatusNotFound, "ip block not found")
		case b.AssignedResourceID != nil:
			writeError(w, http.StatusBadRequest, "ip block is still assigned")
		default:
			delete(m.blocks, id)
			writeJSON(w, map[string]any{"result": "IP Block deleted", "ipBlockId": id})
		}

	case strings.HasPrefix(path, networkPrefix) && r.Method == http.MethodPost:
		network := strings.TrimSuffix(strings.TrimPrefix(path, networkPrefix), "/ip-blocks")
		var req struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		b := m.blocks[req.ID]
		if b == nil {
			writeError(w, http.StatusNotFound, "ip block not found")
			return
		}
		resourceType := "PUBLIC_NETWORK"
		b.AssignedResourceID, b.AssignedResourceType, b.Status = &network, &resourceType, "assigned"
		writeJSON(w, map[string]any{"id": b.ID, "cidr": b.Cidr, "usedIpsCount": "0"})
	case strings.HasPrefix(path, networkPrefix) && r.Method == http.MethodDelete:
		parts := strings.Split(strings.TrimPrefix(path, networkPrefix), "/")
		if len(parts) != 3 || m.blocks[parts[2]] == nil {
			writeError(w, http.StatusNotFound, "ip block not found")
			return
		}
		b := m.blocks[parts[2]]
		b.AssignedResourceID, b.AssignedResourceType, b.Status = nil, nil, "unassigned"
		writeJSON(w, "The IP block was removed from the public network.")

	default:
		writeError(w, http.StatusNotFound, "no mock for "+r.Method+" "+path)
	}
}

// filterBlocks returns the blocks that carry every one of the "name.value" tags
func (m *mock) filterBlocks(filters []string) []*ipBlock {
	ret := []*ipBlock{}
	for _, b := range m.blocks {
		matches := true
		for _, f := range filters {
			name, value, _ := strings.Cut(f, ".")
			found := false
			for _, t := range b.Tags {
				if t.Name == name && t.Value != nil && *t.Value == value {
					found = true
					break
				}
			}
			if !found {
				matches = false
				break
			}
		}
		if matches {
			ret = append(ret, b)
		}
	}
	return ret
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"message": msg})
}
