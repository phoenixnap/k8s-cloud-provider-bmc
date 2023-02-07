package phoenixnap

import (
	"fmt"
	"math/rand"
	"net"

	"github.com/google/uuid"
	randomdata "github.com/pallinder/go-randomdata"
	"github.com/phoenixnap/go-sdk-bmc/billingapi"
	"github.com/phoenixnap/go-sdk-bmc/ipapi"
	"github.com/phoenixnap/k8s-cloud-provider-bmc/phoenixnap/server/store"
)

var randomID = uuid.New().String()

// find "ASH" location or create it
func testGetOrCreateValidLocation(name string, backend store.DataStore) (string, error) {
	location, err := backend.GetLocation(name)
	if err != nil {
		return "", err
	}
	// if we already have it, use it
	if location != "" {
		return location, nil
	}
	// we do not have it, so create it
	return backend.CreateLocation(name)
}

// testGetOrCreateValidServerProduct find a valid server with category and plans, or create them
func testGetOrCreateValidServerProduct(name, location string, backend store.DataStore) (*billingapi.Product, error) {
	product, err := backend.GetProduct(name)
	if err != nil {
		return nil, err
	}
	// if we do not have one, create one
	if product == nil {
		product, err = backend.CreateProduct(name, serverCategory, nil)
		if err != nil {
			return nil, fmt.Errorf("unable to create server product: %w", err)
		}
	}

	// ensure we have the right category, i.e. "SERVER"
	if product.ProductCategory != serverCategory {
		return nil, fmt.Errorf("product %s is not a %s", name, serverCategory)
	}
	// ensure we have at least one plan in the given location
	var found bool
	for _, plan := range product.Plans {
		if plan.Location == location {
			found = true
			break
		}
	}
	if !found {
		// we do not have it, so create it
		plans := append(product.Plans, billingapi.PricingPlan{Sku: uuid.New().String(), Location: location})
		product, err = backend.UpdateProduct(name, plans)
		if err != nil {
			return nil, fmt.Errorf("unable to update server product: %w", err)
		}
	}
	return product, nil
}

// testGetNewServerName get a unique server name
func testGetNewServerName() string {
	return fmt.Sprintf("server-%d", rand.Intn(1000))
}

func testCreateAddressBlock(public bool, location string, size int) *ipapi.IpBlock {
	ipaddr := randomdata.IpV4Address()
	// just mask it
	ip := net.ParseIP(ipaddr)
	addr := ip.Mask(net.CIDRMask(size, 29))
	id, _ := uuid.NewUUID()
	address := ipapi.IpBlock{
		Id:            id.String(),
		Location:      location,
		CidrBlockSize: fmt.Sprintf("%d", size),
		Cidr:          fmt.Sprintf("%s/%d", addr.String(), size),
	}
	return &address
}
