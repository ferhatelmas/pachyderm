package collection

import (
	"context"
	"fmt"
	"log"
	"path"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/pachyderm/pachyderm/src/server/pkg/errutil"
	"github.com/pachyderm/pachyderm/src/server/pkg/watch"

	etcd "github.com/coreos/etcd/clientv3"
	"github.com/coreos/etcd/mvcc/mvccpb"
	"github.com/gogo/protobuf/proto"
)

//QueryPaginationLimit defines the maximum number of results returned in an
// etcd query using a paginated iterator
const QueryPaginationLimit = 10000
const defaultLimit int64 = 1024

type collection struct {
	etcdClient *etcd.Client
	prefix     string
	indexes    []Index
	// The limit used when listing the collection. This gets automatically
	// tuned when requests fail so it's stored per collection.
	limit int64
	// We need this to figure out the concrete type of the objects
	// that this collection is storing. It's pretty retarded, but
	// not sure what else we can do since types in Go are not first-class
	// objects.
	// To be clear, this is only necessary because of `Delete`, where we
	// need to know the type in order to properly remove secondary indexes.
	template proto.Message
	// keyCheck is a function that checks if a key is valid.  Invalid keys
	// cannot be created.
	keyCheck func(string) error
}

// NewCollection creates a new collection.
func NewCollection(etcdClient *etcd.Client, prefix string, indexes []Index, template proto.Message, keyCheck func(string) error) Collection {
	// We want to ensure that the prefix always ends with a trailing
	// slash.  Otherwise, when you list the items under a collection
	// such as `foo`, you might end up listing items under `foobar`
	// as well.
	if len(prefix) > 0 && prefix[len(prefix)-1] != '/' {
		prefix = prefix + "/"
	}

	return &collection{
		prefix:     prefix,
		etcdClient: etcdClient,
		indexes:    indexes,
		limit:      defaultLimit,
		template:   template,
		keyCheck:   keyCheck,
	}
}

func (c *collection) ReadWrite(stm STM) ReadWriteCollection {
	return &readWriteCollection{
		collection: c,
		stm:        stm,
	}
}

func (c *collection) ReadWriteInt(stm STM) ReadWriteIntCollection {
	return &readWriteIntCollection{
		collection: c,
		stm:        stm,
	}
}

func (c *collection) ReadOnly(ctx context.Context) ReadonlyCollection {
	return &readonlyCollection{
		collection: c,
		ctx:        ctx,
	}
}

// Path returns the full path of a key in the etcd namespace
func (c *collection) Path(key string) string {
	return path.Join(c.prefix, key)
}

func (c *collection) indexRoot(index Index) string {
	// remove trailing slash from c.prefix
	return fmt.Sprintf("%s__index_%s/",
		strings.TrimRight(c.prefix, "/"), index.Field)
}

// See the documentation for `Index` for details.
func (c *collection) indexDir(index Index, indexVal interface{}) string {
	var indexValStr string
	if marshaller, ok := indexVal.(proto.Marshaler); ok {
		if indexValBytes, err := marshaller.Marshal(); err == nil {
			// use marshalled proto as index. This way we can rename fields without
			// breaking our index.
			indexValStr = string(indexValBytes)
		} else {
			// log error but keep going (this used to be the only codepath)
			log.Printf("ERROR trying to marshal index value: %v", err)
			indexValStr = fmt.Sprintf("%v", indexVal)
		}
	} else {
		indexValStr = fmt.Sprintf("%v", indexVal)
	}
	return path.Join(c.indexRoot(index), indexValStr)
}

// See the documentation for `Index` for details.
func (c *collection) indexPath(index Index, indexVal interface{}, key string) string {
	return path.Join(c.indexDir(index, indexVal), key)
}

func (c *collection) checkType(val interface{}) error {
	if c.template != nil {
		valType, templateType := reflect.TypeOf(val), reflect.TypeOf(c.template)
		if valType != templateType {
			return fmt.Errorf("invalid type, got: %s, expected: %s", valType, templateType)
		}
	}
	return nil
}

type readWriteCollection struct {
	*collection
	stm STM
}

func (c *readWriteCollection) Get(key string, val proto.Message) error {
	if err := watch.CheckType(c.template, val); err != nil {
		return err
	}
	valStr, err := c.stm.Get(c.Path(key))
	if err != nil {
		return err
	}
	return proto.Unmarshal([]byte(valStr), val)
}

func cloneProtoMsg(original proto.Message) proto.Message {
	val := reflect.ValueOf(original)
	if val.Kind() == reflect.Ptr {
		val = reflect.Indirect(val)
	}
	return reflect.New(val.Type()).Interface().(proto.Message)
}

// Giving a value, an index, and the key of the item, return the path
// under which the new index item should be stored.
func (c *readWriteCollection) getIndexPath(val interface{}, index Index, key string) string {
	reflVal := reflect.ValueOf(val)
	field := reflect.Indirect(reflVal).FieldByName(index.Field).Interface()
	return c.indexPath(index, field, key)
}

// Giving a value, a multi-index, and the key of the item, return the
// paths under which the multi-index items should be stored.
func (c *readWriteCollection) getMultiIndexPaths(val interface{}, index Index, key string) []string {
	var indexPaths []string
	field := reflect.Indirect(reflect.ValueOf(val)).FieldByName(index.Field)
	for i := 0; i < field.Len(); i++ {
		indexPaths = append(indexPaths, c.indexPath(index, field.Index(i).Interface(), key))
	}
	return indexPaths
}

func (c *readWriteCollection) Put(key string, val proto.Message) error {
	return c.PutTTL(key, val, 0)
}

func (c *readWriteCollection) TTL(key string) (int64, error) {
	return c.stm.TTL(c.Path(key))
}

func (c *readWriteCollection) PutTTL(key string, val proto.Message, ttl int64) error {
	if err := watch.CheckType(c.template, val); err != nil {
		return err
	}

	if c.collection.keyCheck != nil {
		if err := c.collection.keyCheck(key); err != nil {
			return err
		}
	}
	if c.indexes != nil {
		clone := cloneProtoMsg(val)

		// Put the appropriate record in any of c's secondary indexes
		for _, index := range c.indexes {
			if index.Multi {
				indexPaths := c.getMultiIndexPaths(val, index, key)
				for _, indexPath := range indexPaths {
					// Only put the index if it doesn't already exist; otherwise
					// we might trigger an unnecessary event if someone is
					// watching the index
					if _, err := c.stm.Get(indexPath); err != nil && IsErrNotFound(err) {
						if err := c.stm.Put(indexPath, key, ttl); err != nil {
							return err
						}
					}
				}
				// If we can get the original value, we remove the original indexes
				if err := c.Get(key, clone); err == nil {
					for _, originalIndexPath := range c.getMultiIndexPaths(clone, index, key) {
						var found bool
						for _, indexPath := range indexPaths {
							if originalIndexPath == indexPath {
								found = true
							}
						}
						if !found {
							c.stm.Del(originalIndexPath)
						}
					}
				}
			} else {
				indexPath := c.getIndexPath(val, index, key)
				// If we can get the original value, we remove the original indexes
				if err := c.Get(key, clone); err == nil {
					originalIndexPath := c.getIndexPath(clone, index, key)
					if originalIndexPath != indexPath {
						c.stm.Del(originalIndexPath)
					}
				}
				// Only put the index if it doesn't already exist; otherwise
				// we might trigger an unnecessary event if someone is
				// watching the index
				if _, err := c.stm.Get(indexPath); err != nil && IsErrNotFound(err) {
					if err := c.stm.Put(indexPath, key, ttl); err != nil {
						return err
					}
				}
			}
		}
	}
	bytes, err := proto.Marshal(val)
	if err != nil {
		return err
	}
	return c.stm.Put(c.Path(key), string(bytes), ttl)
}

// Update reads the current value associated with 'key', calls 'f' to update
// the value, and writes the new value back to the collection. 'key' must be
// present in the collection, or a 'Not Found' error is returned
func (c *readWriteCollection) Update(key string, val proto.Message, f func() error) error {
	if err := watch.CheckType(c.template, val); err != nil {
		return err
	}
	if err := c.Get(key, val); err != nil {
		return err
	}
	if err := f(); err != nil {
		return err
	}
	return c.Put(key, val)
}

// Upsert is like Update but 'key' is not required to be present
func (c *readWriteCollection) Upsert(key string, val proto.Message, f func() error) error {
	if err := watch.CheckType(c.template, val); err != nil {
		return err
	}
	if err := c.Get(key, val); err != nil && !IsErrNotFound(err) {
		return err
	}
	if err := f(); err != nil {
		return err
	}
	return c.Put(key, val)
}

func (c *readWriteCollection) Create(key string, val proto.Message) error {
	if err := watch.CheckType(c.template, val); err != nil {
		return err
	}
	fullKey := c.Path(key)
	_, err := c.stm.Get(fullKey)
	if err != nil && !IsErrNotFound(err) {
		return err
	}
	if err == nil {
		return ErrExists{c.prefix, key}
	}
	c.Put(key, val)
	return nil
}

func (c *readWriteCollection) Delete(key string) error {
	fullKey := c.Path(key)
	if _, err := c.stm.Get(fullKey); err != nil {
		return err
	}
	if c.indexes != nil && c.template != nil {
		val := proto.Clone(c.template)
		for _, index := range c.indexes {
			if err := c.Get(key, val.(proto.Message)); err == nil {
				if index.Multi {
					indexPaths := c.getMultiIndexPaths(val, index, key)
					for _, indexPath := range indexPaths {
						c.stm.Del(indexPath)
					}
				} else {
					indexPath := c.getIndexPath(val, index, key)
					c.stm.Del(indexPath)
				}
			}
		}
	}
	c.stm.Del(fullKey)
	return nil
}

func (c *readWriteCollection) DeleteAll() {
	// Delete indexes
	for _, index := range c.indexes {
		c.stm.DelAll(c.indexRoot(index))
	}
	c.stm.DelAll(c.prefix)
}

func (c *readWriteCollection) DeleteAllPrefix(prefix string) {
	c.stm.DelAll(path.Join(c.prefix, prefix) + "/")
}

type readWriteIntCollection struct {
	*collection
	stm STM
}

func (c *readWriteIntCollection) Create(key string, val int) error {
	fullKey := c.Path(key)
	_, err := c.stm.Get(fullKey)
	if err != nil && !IsErrNotFound(err) {
		return err
	}
	if err == nil {
		return ErrExists{c.prefix, key}
	}
	return c.stm.Put(fullKey, strconv.Itoa(val), 0)
}

func (c *readWriteIntCollection) Get(key string) (int, error) {
	valStr, err := c.stm.Get(c.Path(key))
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(valStr)
}

func (c *readWriteIntCollection) Increment(key string) error {
	return c.IncrementBy(key, 1)
}

func (c *readWriteIntCollection) IncrementBy(key string, n int) error {
	fullKey := c.Path(key)
	valStr, err := c.stm.Get(fullKey)
	if err != nil {
		return err
	}
	val, err := strconv.Atoi(valStr)
	if err != nil {
		return ErrMalformedValue{c.prefix, key, valStr}
	}
	return c.stm.Put(fullKey, strconv.Itoa(val+n), 0)
}

func (c *readWriteIntCollection) Decrement(key string) error {
	return c.DecrementBy(key, 1)
}

func (c *readWriteIntCollection) DecrementBy(key string, n int) error {
	fullKey := c.Path(key)
	valStr, err := c.stm.Get(fullKey)
	if err != nil {
		return err
	}
	val, err := strconv.Atoi(valStr)
	if err != nil {
		return ErrMalformedValue{c.prefix, key, valStr}
	}
	return c.stm.Put(fullKey, strconv.Itoa(val-n), 0)
}

func (c *readWriteIntCollection) Delete(key string) error {
	fullKey := c.Path(key)
	if _, err := c.stm.Get(fullKey); err != nil {
		return err
	}
	c.stm.Del(fullKey)
	return nil
}

type readonlyCollection struct {
	*collection
	ctx context.Context
}

func (c *readonlyCollection) Get(key string, val proto.Message) error {
	if err := watch.CheckType(c.template, val); err != nil {
		return err
	}
	resp, err := c.etcdClient.Get(c.ctx, c.Path(key))
	if err != nil {
		return err
	}

	if len(resp.Kvs) == 0 {
		return ErrNotFound{c.prefix, key}
	}

	return proto.Unmarshal(resp.Kvs[0].Value, val)
}

// an indirect iterator goes through a list of keys and retrieve those
// items from the collection.
type indirectIterator struct {
	index int
	resp  *etcd.GetResponse
	col   *readonlyCollection
}

func (i *indirectIterator) Next(key *string, val proto.Message) (ok bool, retErr error) {
	if err := watch.CheckType(i.col.template, val); err != nil {
		return false, err
	}
	for {
		if i.index < len(i.resp.Kvs) {
			kv := i.resp.Kvs[i.index]
			i.index++

			*key = path.Base(string(kv.Key))
			if err := i.col.Get(*key, val); err != nil {
				if IsErrNotFound(err) {
					// In cases where we changed how certain objects are
					// indexed, we could end up in a situation where the
					// object is deleted but the old indexes still exist.
					continue
				} else {
					return false, err
				}
			}

			return true, nil
		}
		return false, nil
	}
}

func (c *readonlyCollection) GetByIndex(index Index, val interface{}) (Iterator, error) {
	resp, err := c.etcdClient.Get(c.ctx, c.indexDir(index, val), etcd.WithPrefix(), etcd.WithSort(etcd.SortByModRevision, etcd.SortDescend))
	if err != nil {
		return nil, err
	}
	return &indirectIterator{
		resp: resp,
		col:  c,
	}, nil
}

func (c *readonlyCollection) GetByIndexF(index Index, indexVal interface{}, val proto.Message, f func(key string) error) error {
	if index.limit == nil {
		limit := defaultLimit
		index.limit = &limit
	}
	return c.listF(c.indexDir(index, val), index.limit, func(kv *mvccpb.KeyValue) error {
		key := path.Base(string(kv.Key))
		if err := c.Get(key, val); err != nil {
			if IsErrNotFound(err) {
				// In cases where we changed how certain objects are
				// indexed, we could end up in a situation where the
				// object is deleted but the old indexes still exist.
				return nil
			} else {
				return err
			}
		}
		return f(key)
	})
}

func (c *readonlyCollection) GetBlock(key string, val proto.Message) error {
	if err := watch.CheckType(c.template, val); err != nil {
		return err
	}
	watcher, err := watch.NewWatcher(c.ctx, c.etcdClient, c.prefix, c.Path(key), c.template)
	if err != nil {
		return err
	}
	defer watcher.Close()
	for {
		e := <-watcher.Watch()
		if e.Err != nil {
			return e.Err
		}
		return e.Unmarshal(&key, val)
	}
}

func endKeyFromPrefix(prefix string) string {
	// Lexigraphically increment the last character
	return prefix[0:len(prefix)-1] + string(byte(prefix[len(prefix)-1])+1)
}

// ListPrefix returns lexigraphically sorted (not sorted by create time)
// results and returns an iterator that is paginated
func (c *readonlyCollection) ListPrefix(prefix string) (Iterator, error) {
	queryPrefix := c.prefix
	if prefix != "" {
		// If we always call join, we'll get rid of the trailing slash we need
		// on the root c.prefix
		queryPrefix = filepath.Join(c.prefix, prefix)
	}
	// omit sort so that we get results lexigraphically ordered, so that we can paginate properly
	resp, err := c.etcdClient.Get(c.ctx, queryPrefix, etcd.WithPrefix(), etcd.WithLimit(QueryPaginationLimit))
	if err != nil {
		return nil, err
	}
	return &paginatedIterator{
		endKey:     endKeyFromPrefix(queryPrefix),
		resp:       resp,
		etcdClient: c.etcdClient,
		ctx:        c.ctx,
		col:        c,
	}, nil
}

// List returns an iterator that can be used to iterate over the collection.
// The objects are sorted by revision time in descending order, i.e. newer
// objects are returned first.
func (c *readonlyCollection) List() (Iterator, error) {
	resp, err := c.etcdClient.Get(c.ctx, c.prefix, etcd.WithPrefix(), etcd.WithSort(etcd.SortByModRevision, etcd.SortDescend))
	if err != nil {
		return nil, err
	}
	return &iterator{
		resp: resp,
		col:  c,
	}, nil
}

// ListF returns objects sorted by CreateRevision, i.e. the order in which they
// were created. f will be called with each key, val will contain the
// corresponding value. Val is not an argument to f because that would require
// f to perform a cast before it could be used. You can break out of iteration by return errutil.ErrBreak.
func (c *readonlyCollection) ListF(val proto.Message, f func(string) error) error {
	if err := watch.CheckType(c.template, val); err != nil {
		return err
	}
	return c.listF(c.prefix, &c.limit, func(kv *mvccpb.KeyValue) error {
		if err := proto.Unmarshal(kv.Value, val); err != nil {
			return err
		}
		return f(strings.TrimPrefix(string(kv.Key), c.prefix))
	})
}

func (c *readonlyCollection) listF(prefix string, limitPtr *int64, f func(*mvccpb.KeyValue) error) error {
	rev := int64(-1)
	keysInRev := make(map[string]bool)
	for {
		opts := []etcd.OpOption{etcd.WithPrefix(), etcd.WithSort(etcd.SortByCreateRevision, etcd.SortDescend)}
		if rev != -1 {
			opts = append(opts, etcd.WithMaxCreateRev(rev))
		}
		var resp *etcd.GetResponse
		var limit int64
		for {
			limit = atomic.LoadInt64(limitPtr)
			var err error
			resp, err = c.etcdClient.Get(c.ctx, prefix, append(opts, etcd.WithLimit(limit))...)
			if err != nil {
				if strings.Contains(err.Error(), "received message larger than max") {
					atomic.CompareAndSwapInt64(limitPtr, limit, limit/2)
					continue
				}
				return err
			}
			break
		}
		if len(resp.Kvs) == int(limit) && resp.Kvs[0].CreateRevision == resp.Kvs[len(resp.Kvs)-1].CreateRevision {
			return fmt.Errorf("revision contains too many objects to fit in one batch (this is likely a bug)")
		}
		for _, kv := range resp.Kvs {
			if keysInRev[string(kv.Key)] {
				continue
			}
			if err := f(kv); err != nil {
				if err == errutil.ErrBreak {
					return nil
				}
				return err
			}
			if kv.CreateRevision != rev {
				keysInRev = make(map[string]bool)
				rev = kv.CreateRevision
			}
			keysInRev[string(kv.Key)] = true
		}
		if len(resp.Kvs) < int(limit) {
			return nil
		}
	}
}

// ListPaginated returns an iterator that can be used to iterate over the collection.
// The objects are not sorted, but are paginated.
func (c *readonlyCollection) ListPaginated() (Iterator, error) {
	resp, err := c.etcdClient.Get(c.ctx, c.prefix, etcd.WithPrefix(), etcd.WithLimit(QueryPaginationLimit))
	if err != nil {
		return nil, err
	}
	return &paginatedIterator{
		endKey:     endKeyFromPrefix(c.prefix),
		resp:       resp,
		etcdClient: c.etcdClient,
		ctx:        c.ctx,
		col:        c,
	}, nil
}

type iterator struct {
	index int
	resp  *etcd.GetResponse
	col   *readonlyCollection
}

type paginatedIterator struct {
	index      int
	endKey     string
	resp       *etcd.GetResponse
	etcdClient *etcd.Client
	ctx        context.Context
	col        *readonlyCollection
}

func (c *readonlyCollection) Count() (int64, error) {
	resp, err := c.etcdClient.Get(c.ctx, c.prefix, etcd.WithPrefix(), etcd.WithCountOnly())
	if err != nil {
		return 0, err
	}
	return resp.Count, err
}

func (i *iterator) Next(key *string, val proto.Message) (ok bool, retErr error) {
	if err := watch.CheckType(i.col.template, val); err != nil {
		return false, err
	}
	if i.index < len(i.resp.Kvs) {
		kv := i.resp.Kvs[i.index]
		i.index++

		*key = path.Base(string(kv.Key))
		if err := proto.Unmarshal(kv.Value, val); err != nil {
			return false, err
		}

		return true, nil
	}
	return false, nil
}

// Next() writes a fully qualified key (including the path) to the key value
func (i *paginatedIterator) Next(key *string, val proto.Message) (ok bool, retErr error) {
	if err := watch.CheckType(i.col.template, val); err != nil {
		return false, err
	}
	if i.index < len(i.resp.Kvs) {
		kv := i.resp.Kvs[i.index]
		i.index++

		*key = string(kv.Key)
		if err := proto.Unmarshal(kv.Value, val); err != nil {
			return false, err
		}

		return true, nil
	}
	if len(i.resp.Kvs) == 0 {
		//empty response
		return false, nil
	}
	// Reached end of resp, try for another page
	fromKey := string(i.resp.Kvs[len(i.resp.Kvs)-1].Key)
	resp, err := i.etcdClient.Get(i.ctx, fromKey, etcd.WithRange(i.endKey), etcd.WithLimit(QueryPaginationLimit))
	if err != nil {
		return false, err
	}
	if len(resp.Kvs) == 1 {
		// Only got the from key, there are no more kvs to fetch from etcd
		return false, nil
	}
	i.index = 1 // Move past the from key
	i.resp = resp
	return i.Next(key, val)
}

// Watch a collection, returning the current content of the collection as
// well as any future additions.
func (c *readonlyCollection) Watch() (watch.Watcher, error) {
	return watch.NewWatcher(c.ctx, c.etcdClient, c.prefix, c.prefix, c.template)
}

func (c *readonlyCollection) WatchWithPrev() (watch.Watcher, error) {
	return watch.NewWatcherWithPrev(c.ctx, c.etcdClient, c.prefix, c.prefix, c.template)
}

// WatchByIndex watches items in a collection that match a particular index
func (c *readonlyCollection) WatchByIndex(index Index, val interface{}) (watch.Watcher, error) {
	eventCh := make(chan *watch.Event)
	done := make(chan struct{})
	watcher, err := watch.NewWatcher(c.ctx, c.etcdClient, c.prefix, c.indexDir(index, val), c.template)
	if err != nil {
		return nil, err
	}
	go func() (retErr error) {
		defer func() {
			if retErr != nil {
				eventCh <- &watch.Event{
					Type: watch.EventError,
					Err:  retErr,
				}
				watcher.Close()
			}
			close(eventCh)
		}()
		for {
			var ev *watch.Event
			var ok bool
			select {
			case ev, ok = <-watcher.Watch():
			case <-done:
				watcher.Close()
				return nil
			}
			if !ok {
				watcher.Close()
				return nil
			}

			var directEv *watch.Event
			switch ev.Type {
			case watch.EventError:
				// pass along the error
				return ev.Err
			case watch.EventPut:
				resp, err := c.etcdClient.Get(c.ctx, c.Path(path.Base(string(ev.Key))))
				if err != nil {
					return err
				}
				if len(resp.Kvs) == 0 {
					// this happens only if the item was deleted shortly after
					// we receive this event.
					continue
				}
				directEv = &watch.Event{
					Key:      []byte(path.Base(string(ev.Key))),
					Value:    resp.Kvs[0].Value,
					Type:     ev.Type,
					Template: c.template,
				}
			case watch.EventDelete:
				directEv = &watch.Event{
					Key:      []byte(path.Base(string(ev.Key))),
					Type:     ev.Type,
					Template: c.template,
				}
			}
			eventCh <- directEv
		}
	}()
	return watch.MakeWatcher(eventCh, done), nil
}

// WatchOne watches a given item.  The first value returned from the watch
// will be the current value of the item.
func (c *readonlyCollection) WatchOne(key string) (watch.Watcher, error) {
	return watch.NewWatcher(c.ctx, c.etcdClient, c.prefix, c.Path(key), c.template)
}
