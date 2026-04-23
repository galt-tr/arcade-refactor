// Package factory constructs a store.Store + store.Leaser pair from config.
// It lives outside the store package so backend implementations can depend on
// store's interfaces without creating an import cycle with the dispatcher.
package factory

import (
	"fmt"

	"github.com/bsv-blockchain/arcade/config"
	"github.com/bsv-blockchain/arcade/store"
	"github.com/bsv-blockchain/arcade/store/aerospike"
	"github.com/bsv-blockchain/arcade/store/pebble"
)

// New constructs a Store and Leaser pair dispatching on cfg.Store.Backend.
// Both return values point to the same underlying backend — every supported
// backend implements both interfaces — so callers can pass the returned
// Leaser into services.propagation without a second factory.
func New(cfg *config.Config) (store.Store, store.Leaser, error) {
	switch cfg.Store.Backend {
	case "", "aerospike":
		s, err := aerospike.New(cfg.Store.Aerospike)
		if err != nil {
			return nil, nil, err
		}
		return s, s, nil
	case "pebble":
		s, err := pebble.New(cfg.Store.Pebble)
		if err != nil {
			return nil, nil, err
		}
		return s, s, nil
	default:
		return nil, nil, fmt.Errorf("unknown store.backend %q", cfg.Store.Backend)
	}
}
