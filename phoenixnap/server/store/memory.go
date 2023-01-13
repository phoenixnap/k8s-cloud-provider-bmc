package store

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/apparentlymart/go-cidr/cidr"
	"github.com/google/uuid"
	"github.com/pallinder/go-randomdata"
	"github.com/phoenixnap/go-sdk-bmc/billingapi"
	"github.com/phoenixnap/go-sdk-bmc/bmcapi"
)

const (
	privateIPRange = "10.0.10.0/24"
)

// Memory is an implementation of DataStore which stores everything in memory
type Memory struct {
	locations         map[string]bool
	servers           map[string]*bmcapi.Server
	ProductCategories map[string]bool
	products          map[string]*billingapi.Product
	privateIPRange    string
	lastIP            net.IP
	mutex             sync.Mutex
}

// NewMemory returns a properly initialized Memory
func NewMemory() (*Memory, error) {
	ip := strings.SplitN(privateIPRange, "/", 2)
	if len(ip) != 2 {
		return nil, fmt.Errorf("invalid private IP range: %s", privateIPRange)
	}
	sizeAsInt, err := strconv.ParseInt(ip[1], 10, 64)
	if err != nil {
		return nil, err
	}
	start, _ := cidr.AddressRange(&net.IPNet{
		IP:   net.ParseIP(ip[0]),
		Mask: net.CIDRMask(int(sizeAsInt), 32),
	})

	mem := &Memory{
		locations:         map[string]bool{},
		servers:           map[string]*bmcapi.Server{},
		ProductCategories: map[string]bool{},
		products:          map[string]*billingapi.Product{},
		privateIPRange:    privateIPRange,
		lastIP:            cidr.Inc(start),
	}

	// create default location
	_, _ = mem.CreateLocation("ASH")
	// create default product
	_, _ = mem.CreateProduct("d1.c1.small", "SERVER", nil)
	return mem, nil
}

// getID get new unique number ID
func (m *Memory) getID() string {
	u, _ := uuid.NewUUID()
	return u.String()
}

// CreateLocation creates a new location
func (m *Memory) CreateLocation(name string) (string, error) {
	m.locations[name] = true
	return name, nil
}

// ListLocations returns locations; if blank, it knows about ASH
func (m *Memory) ListLocations() ([]string, error) {
	var locations []string
	for k := range m.locations {
		locations = append(locations, k)
	}
	return locations, nil
}

// GetLocation get a single location
func (m *Memory) GetLocation(name string) (string, error) {
	if _, ok := m.locations[name]; ok {
		return name, nil
	}
	return "", nil
}

// CreateProductCategory create a single product type
func (m *Memory) CreateProductCategory(name string) (string, error) {
	m.ProductCategories[name] = true
	return name, nil
}

// GetProductCategory get a single product type
func (m *Memory) GetProductCategory(name string) (string, error) {
	if _, ok := m.ProductCategories[name]; ok {
		return name, nil
	}
	return "", nil
}

// CreateProduct create a single product
func (m *Memory) CreateProduct(name, category string, plans []billingapi.PricingPlan) (*billingapi.Product, error) {
	product := &billingapi.Product{
		ProductCode:     name,
		ProductCategory: category,
		Plans:           plans,
	}
	m.products[name] = product
	return product, nil
}

// UpdateProduct update a single product
func (m *Memory) UpdateProduct(name string, plans []billingapi.PricingPlan) (*billingapi.Product, error) {
	product, ok := m.products[name]
	if !ok {
		return nil, fmt.Errorf("product not found: %s", name)
	}
	product.Plans = plans
	m.products[name] = product
	return product, nil
}

func (m *Memory) ListProductCategories() ([]string, error) {
	var categories []string
	for k := range m.ProductCategories {
		categories = append(categories, k)
	}
	return categories, nil
}

// ListProducts list all products
func (m *Memory) ListProducts() ([]*billingapi.Product, error) {
	var products []*billingapi.Product
	for _, p := range m.products {
		products = append(products, p)
	}
	return products, nil
}

// GetProduct get a product by name and category
func (m *Memory) FindProduct(code, category string) (*billingapi.Product, error) {
	product, ok := m.products[code]
	if !ok {
		return nil, nil
	}
	if product.ProductCategory != category {
		return nil, nil
	}
	return product, nil
}

// GetProduct get a product by ID
func (m *Memory) GetProduct(code string) (*billingapi.Product, error) {
	if product, ok := m.products[code]; ok {
		return product, nil
	}
	return nil, nil
}

// CreateServer creates a new server
func (m *Memory) CreateServer(name, serverType, location string) (*bmcapi.Server, error) {
	serverProduct, err := m.GetProduct(serverType)
	if err != nil {
		return nil, fmt.Errorf("unknown server type: %s", serverType)
	}
	// interpret the location and see if it is a valid one
	if _, err := m.GetLocation(location); err != nil {
		return nil, fmt.Errorf("unknown location: %s", location)
	}
	// go through the serverProduct and make sure the location is acceptable in one of the plans
	var found bool
	for _, plan := range serverProduct.Plans {
		if plan.Location == location {
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("location %s is not supported for server type %s", location, serverType)
	}
	id := m.getID()
	m.mutex.Lock()
	m.lastIP = cidr.Inc(m.lastIP)
	privateIP := m.lastIP
	m.mutex.Unlock()
	server := &bmcapi.Server{
		Id:                 id,
		Hostname:           name,
		Status:             "active",
		Location:           location,
		Type:               serverType,
		PublicIpAddresses:  []string{randomdata.IpV4Address()},
		PrivateIpAddresses: []string{privateIP.String()},
	}
	m.servers[id] = server
	return server, nil
}

// UpdateServer updates an existing device
func (m *Memory) UpdateServer(server *bmcapi.Server) error {
	if server == nil {
		return fmt.Errorf("must include a valid server")
	}
	if _, ok := m.servers[server.Id]; ok {
		m.servers[server.Id] = server
		return nil
	}
	return fmt.Errorf("server not found")
}

// ListServers list all known servers for the project
func (m *Memory) ListServers() ([]*bmcapi.Server, error) {
	var servers []*bmcapi.Server
	for _, s := range m.servers {
		servers = append(servers, s)
	}
	return servers, nil
}

// GetServer get information about a single server
func (m *Memory) GetServer(serverID string) (*bmcapi.Server, error) {
	if server, ok := m.servers[serverID]; ok {
		return server, nil
	}
	return nil, nil
}

// DeleteServer delete a single server
func (m *Memory) DeleteServer(serverID string) (bool, error) {
	if _, ok := m.servers[serverID]; ok {
		delete(m.servers, serverID)
		return true, nil
	}
	return false, nil
}
