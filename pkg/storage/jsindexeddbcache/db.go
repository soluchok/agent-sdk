// +build js,wasm

/*
Copyright SecureKey Technologies Inc. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package jsindexeddbcache

import (
	"errors"
	"fmt"
	"syscall/js"
	"time"

	"github.com/hyperledger/aries-framework-go/component/storage/indexeddb"
	"github.com/hyperledger/aries-framework-go/pkg/didcomm/messenger"
	"github.com/hyperledger/aries-framework-go/pkg/didcomm/protocol/introduce"
	"github.com/hyperledger/aries-framework-go/pkg/didcomm/protocol/issuecredential"
	"github.com/hyperledger/aries-framework-go/pkg/didcomm/protocol/mediator"
	"github.com/hyperledger/aries-framework-go/pkg/didcomm/protocol/presentproof"
	"github.com/hyperledger/aries-framework-go/pkg/kms/localkms"
	"github.com/hyperledger/aries-framework-go/pkg/store/connection"
	"github.com/hyperledger/aries-framework-go/pkg/store/did"
	"github.com/hyperledger/aries-framework-go/pkg/store/verifiable"
	"github.com/hyperledger/aries-framework-go/pkg/vdr/peer"
	"github.com/hyperledger/aries-framework-go/spi/storage"
	"github.com/trustbloc/edge-core/pkg/log"
)

const (
	dbName     = "aries-%s"
	defDBName  = "aries"
	timeLayout = "2006-01-02T15:04:05.000Z"
)

var logger = log.New("jsindexeddb-cache")

// Provider jsindexdbcache implementation of storage.Provider interface.
type Provider struct {
	jsindexeddbProvider storage.Provider
	storesName          map[string]string
	clearDB             bool
}

// NewProvider instantiates Provider.
func NewProvider(name string, clearCache time.Duration) (*Provider, error) {
	jsindexeddbProvider, err := indexeddb.NewProvider(name)
	if err != nil {
		return nil, err
	}

	db := defDBName
	if name != "" {
		db = fmt.Sprintf(dbName, name)
	}

	clearDB, err := checkClearTime(jsindexeddbProvider, clearCache)
	if err != nil {
		return nil, err
	}

	m := make(map[string]string)

	for _, v := range getStoreNames() {
		m[v] = db
		if clearDB {
			if err := clearStore(db, v); err != nil {
				return nil, err
			}
		}
	}

	prov := &Provider{jsindexeddbProvider: jsindexeddbProvider, storesName: m, clearDB: clearDB}

	ticker := time.NewTicker(clearCache)
	quit := make(chan struct{})
	go func(p *Provider) {
		for {
			select {
			case <-ticker.C:
				if err := p.Close(); err != nil {
					logger.Errorf(err.Error())
				}
			case <-quit:
				ticker.Stop()
				return
			}
		}
	}(prov)

	return prov, nil
}

// OpenStore open store.
func (p *Provider) OpenStore(name string) (storage.Store, error) {
	store, err := p.jsindexeddbProvider.OpenStore(name)
	if err != nil {
		return nil, err
	}

	_, exist := p.storesName[name]
	if !exist {
		databaseName := fmt.Sprintf(dbName, name)
		p.storesName[name] = databaseName
		if p.clearDB {
			if err := clearStore(databaseName, name); err != nil {
				return nil, err
			}
		}
	}

	return &cacheStore{store: store}, nil
}

func (p *Provider) SetStoreConfig(name string, config storage.StoreConfiguration) error {
	return p.jsindexeddbProvider.SetStoreConfig(name, config)
}

func (p *Provider) GetStoreConfig(name string) (storage.StoreConfiguration, error) {
	return p.jsindexeddbProvider.GetStoreConfig(name)
}

func (p *Provider) GetOpenStores() []storage.Store {
	return p.jsindexeddbProvider.GetOpenStores()
}

// Close closes all stores created under this store provider.
func (p *Provider) Close() error {
	for storeName, databaseName := range p.storesName {
		if err := clearStore(databaseName, storeName); err != nil {
			return err
		}
	}

	return nil
}

type cacheStore struct {
	store storage.Store
}

// Put stores the key and the record.
func (s *cacheStore) Put(k string, v []byte, tags ...storage.Tag) error {
	return s.store.Put(k, v, tags...)
}

// Get fetches the record based on key.
func (s *cacheStore) Get(k string) ([]byte, error) {
	return s.store.Get(k)
}

func (s *cacheStore) GetTags(key string) ([]storage.Tag, error) {
	return s.store.GetTags(key)
}

func (s *cacheStore) GetBulk(keys ...string) ([][]byte, error) {
	return s.store.GetBulk(keys...)
}

func (s *cacheStore) Query(expression string, options ...storage.QueryOption) (storage.Iterator, error) {
	return s.store.Query(expression, options...)
}

// Delete will delete record with k key.
func (s *cacheStore) Delete(k string) error {
	return s.store.Delete(k)
}

func (s *cacheStore) Batch(operations []storage.Operation) error {
	return s.store.Batch(operations)
}

func (s *cacheStore) Flush() error {
	return s.store.Flush()
}

func (s *cacheStore) Close() error {
	return s.store.Close()
}

func clearStore(databaseName, storeName string) error {
	req := js.Global().Get("indexedDB").Call("open", databaseName, 1)
	v, err := getResult(req)
	if err != nil {
		return err
	}

	req = v.Call("transaction", storeName, "readwrite").Call("objectStore", storeName).Call("clear")
	_, err = getResult(req)
	if err != nil {
		return err
	}

	return nil
}

func getResult(req js.Value) (*js.Value, error) {
	onsuccess := make(chan js.Value)
	onerror := make(chan js.Value)

	const timeout = 10

	req.Set("onsuccess", js.FuncOf(func(this js.Value, inputs []js.Value) interface{} {
		onsuccess <- this.Get("result")
		return nil
	}))
	req.Set("onerror", js.FuncOf(func(this js.Value, inputs []js.Value) interface{} {
		onerror <- this.Get("error")
		return nil
	}))
	select {
	case value := <-onsuccess:
		return &value, nil
	case value := <-onerror:
		return nil, fmt.Errorf("%s %s", value.Get("name").String(),
			value.Get("message").String())
	case <-time.After(timeout * time.Second):
		return nil, fmt.Errorf("timeout waiting for event")
	}
}

func getStoreNames() []string {
	return []string{
		messenger.MessengerStore,
		mediator.Coordination,
		connection.Namespace,
		introduce.Introduce,
		peer.StoreNamespace,
		did.StoreName,
		localkms.Namespace,
		verifiable.NameSpace,
		issuecredential.Name,
		presentproof.Name,
	}
}

func checkClearTime(jsindexeddbProvider storage.Provider, clearCache time.Duration) (bool, error) {
	cacheMeta, err := jsindexeddbProvider.OpenStore("cache_meta")
	if err != nil {
		return false, err
	}

	clearDB := false

	clearTime, err := cacheMeta.Get("clear_time")
	if err != nil {
		if !errors.Is(err, storage.ErrDataNotFound) {
			return false, err
		}

		if err := cacheMeta.Put("clear_time", []byte(time.Now().In(time.UTC).Add(clearCache).Format(timeLayout))); err != nil {
			return false, err
		}
	} else {
		// check clear time
		now := time.Now().In(time.UTC)
		t, err := time.Parse(timeLayout, string(clearTime))
		if err != nil {
			return false, err
		}

		if now.After(t) {
			clearDB = true
			if err := cacheMeta.Put("clear_time", []byte(time.Now().In(time.UTC).Add(clearCache).Format(timeLayout))); err != nil {
				return false, err
			}
		}
	}

	return clearDB, nil
}
