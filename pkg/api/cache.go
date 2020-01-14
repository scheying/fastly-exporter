package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/cespare/xxhash"
	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/peterbourgon/fastly-exporter/pkg/filter"
	"github.com/pkg/errors"
)

// Cache is a producer contract that abstracts over the
// actual ServiceCache and the DemoCache.
type Cache interface {
	Refresh(HTTPClient) error
	ServiceIDs() (ids []string)
	Metadata(id string) (name string, version int, found bool)
}

// HTTPClient is a consumer contract for the caches.
// It models a concrete http.Client.
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// Service metadata associated with a single service.
// Also serves as a DTO for api.fastly.com/service.
type Service struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version int    `json:"version"`
}

//
//
//

// ServiceCache implements Cache and polls api.fastly.com/service to keep
// metadata about one or more service IDs up-to-date.
type ServiceCache struct {
	token      string
	serviceIDs stringSet
	nameFilter filter.Filter
	shard      shardSlice
	logger     log.Logger

	mtx      sync.RWMutex
	services map[string]Service
}

var _ Cache = (*ServiceCache)(nil)

// NewServiceCache returns an empty cache of service metadata. By default, it
// will fetch metadata about all services available to the provided token. Use
// options to restrict which services the cache should manage.
func NewServiceCache(token string, options ...ServiceCacheOption) *ServiceCache {
	c := &ServiceCache{
		token:  token,
		logger: log.NewNopLogger(),
	}
	for _, option := range options {
		option(c)
	}
	return c
}

// ServiceCacheOption provides some additional behavior to a cache. Options that
// restrict which services are cached combine with AND semantics.
type ServiceCacheOption func(*ServiceCache)

// WithExplicitServiceIDs restricts the cache to fetch metadata only for the
// provided service IDs. By default, all service IDs available to the provided
// token are allowed.
func WithExplicitServiceIDs(ids ...string) ServiceCacheOption {
	return func(c *ServiceCache) { c.serviceIDs = newStringSet(ids) }
}

// WithNameFilter restricts the cache to fetch metadata only for the services
// whose names pass the provided filter. By default, no name filtering occurs.
func WithNameFilter(f filter.Filter) ServiceCacheOption {
	return func(c *ServiceCache) { c.nameFilter = f }
}

// WithShard restricts the cache to fetch metadata only for those services whose
// IDs, when hashed and taken modulo m, equal (n-1). By default, no sharding
// occurs.
//
// This option is designed to allow users to split accounts (tokens) that have a
// large number of services across multiple exporter processes. For example, to
// split across 3 processes, each process would set n={1,2,3} and m=3.
func WithShard(n, m uint64) ServiceCacheOption {
	return func(c *ServiceCache) { c.shard = shardSlice{n, m} }
}

// WithLogger sets the logger used by the cache during refresh.
// By default, no log events are emitted.
func WithLogger(logger log.Logger) ServiceCacheOption {
	return func(c *ServiceCache) { c.logger = logger }
}

// Refresh services and their metadata.
func (c *ServiceCache) Refresh(client HTTPClient) error {
	begin := time.Now()

	req, err := http.NewRequest("GET", "https://api.fastly.com/service", nil)
	if err != nil {
		return errors.Wrap(err, "error constructing API services request")
	}

	req.Header.Set("Fastly-Key", c.token)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return errors.Wrap(err, "error executing API services request")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var response struct {
			Msg string `json:"msg"`
		}
		json.NewDecoder(resp.Body).Decode(&response)
		if response.Msg == "" {
			response.Msg = "unknown error"
		}
		return errors.Errorf("api.fastly.com responded with %s (%s)", resp.Status, response.Msg)
	}

	var response []Service
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return errors.Wrap(err, "error decoding API services response")
	}

	nextgen := map[string]Service{}
	for _, s := range response {
		debug := level.Debug(log.With(c.logger,
			"service_id", s.ID,
			"service_name", s.Name,
			"service_version", s.Version,
		))

		if reject := !c.serviceIDs.empty() && !c.serviceIDs.has(s.ID); reject {
			debug.Log("result", "rejected", "reason", "service ID not explicitly allowed")
			continue
		}

		if reject := !c.nameFilter.Allow(s.Name); reject {
			debug.Log("result", "rejected", "reason", "service name rejected by name filter")
			continue
		}

		if reject := !c.shard.match(s.ID); reject {
			debug.Log("result", "rejected", "reason", "service ID in different shard")
			continue
		}

		debug.Log("result", "accepted")
		nextgen[s.ID] = s
	}

	level.Debug(c.logger).Log(
		"refresh_took", time.Since(begin),
		"total_service_count", len(response),
		"accepted_service_count", len(nextgen),
	)

	c.mtx.Lock()
	c.services = nextgen
	c.mtx.Unlock()

	return nil
}

// ServiceIDs currently being monitored by the cache.
// The set can change over time.
func (c *ServiceCache) ServiceIDs() (ids []string) {
	c.mtx.RLock()
	defer c.mtx.RUnlock()

	ids = make([]string, 0, len(c.services))
	for _, s := range c.services {
		ids = append(ids, s.ID)
	}

	sort.Strings(ids) // mostly for tests

	return ids
}

// Metadata returns selected metadata associated with a given service ID.
// If the cache doesn't contain that service ID, found will be false.
func (c *ServiceCache) Metadata(id string) (name string, version int, found bool) {
	c.mtx.RLock()
	defer c.mtx.RUnlock()

	if s, ok := c.services[id]; ok {
		name, version, found = s.Name, s.Version, true
	}
	return name, version, found
}

//
//
//

// DemoCache implements Cache but only returns the service ID "demo".
type DemoCache struct{}

var _ Cache = DemoCache{}

// Refresh is a no-op.
func (DemoCache) Refresh(client HTTPClient) error {
	return nil
}

// ServiceIDs always returns "demo".
func (DemoCache) ServiceIDs() (ids []string) {
	return []string{"demo"}
}

// Metadata for the "demo" service ID only.
func (DemoCache) Metadata(id string) (name string, version int, found bool) {
	if id == "demo" {
		return "fastly.com", 1, true
	}
	return name, version, false
}

//
//
//

type stringSet map[string]struct{}

func newStringSet(initial []string) stringSet {
	ss := stringSet{}
	for _, s := range initial {
		ss[s] = struct{}{}
	}
	return ss
}

func (ss stringSet) empty() bool {
	return len(ss) == 0
}

func (ss stringSet) has(s string) bool {
	_, ok := ss[s]
	return ok
}

type shardSlice struct{ n, m uint64 }

func (ss shardSlice) match(serviceID string) bool {
	if ss.m == 0 {
		return true // the zero value of the type matches all IDs
	}

	if ss.n == 0 {
		panic("programmer error: shard with n = 0, m != 0")
	}

	h := xxhash.New()
	fmt.Fprint(h, serviceID)
	return h.Sum64()%uint64(ss.m) == uint64(ss.n-1)
}
