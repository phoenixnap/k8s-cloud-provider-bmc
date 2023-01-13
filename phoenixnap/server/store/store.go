package store

import (
	"github.com/phoenixnap/go-sdk-bmc/billingapi"
	"github.com/phoenixnap/go-sdk-bmc/bmcapi"
)

// DataStore is the item that retrieves backend information to serve out
// following a contract API
type DataStore interface {
	CreateLocation(name string) (string, error)
	ListLocations() ([]string, error)
	GetLocation(name string) (string, error)
	CreateProductCategory(name string) (string, error)
	GetProductCategory(name string) (string, error)
	ListProductCategories() ([]string, error)
	ListProducts() ([]*billingapi.Product, error)
	GetProduct(code string) (*billingapi.Product, error)
	FindProduct(code, category string) (*billingapi.Product, error)
	CreateProduct(name, category string, plans []billingapi.PricingPlan) (*billingapi.Product, error)
	UpdateProduct(name string, plans []billingapi.PricingPlan) (*billingapi.Product, error)
	CreateServer(name, serverType, location string) (*bmcapi.Server, error)
	UpdateServer(server *bmcapi.Server) error
	ListServers() ([]*bmcapi.Server, error)
	GetServer(serverID string) (*bmcapi.Server, error)
	DeleteServer(serverID string) (bool, error)
}
