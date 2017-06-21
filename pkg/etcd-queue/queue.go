// Package etcdqueue implements queue service backed by etcd.
package etcdqueue

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"sync"
	"time"

	"github.com/coreos/etcd/clientv3"
	"github.com/coreos/etcd/embed"
	"github.com/coreos/etcd/etcdserver/api/v3client"
	"github.com/coreos/etcd/mvcc/mvccpb"
	"github.com/golang/glog"
)

// Item is a job item.
// Key is used as a key in etcd.
// Marshalled JSON struct data as a value.
type Item struct {
	// Bucket is the name or job category for namespacing.
	// All keys will be prefixed with this bucket name.
	Bucket string `json:"bucket"`

	// CreatedAt is the time when this item is created.
	CreatedAt time.Time `json:"created_at"`

	// Key is autogenerated and used as a key when written to etcd.
	Key string `json:"key"`

	// Value contains any data (e.g. encoded computation results).
	Value string `json:"value"`

	// Progress is the progress status value (range from 0 to 100).
	Progress int `json:"progress"`

	// Canceled is true.
	Canceled bool `json:"canceled"`

	// Error contains any error message.
	// It's defined as string for different language interpolation.
	Error string `json:"error"`

	// RequestID is to be populated by external service to uniquely identify each item.
	RequestID string `json:"request_id"`
}

// CreateItem creates an item with auto-generated ID of unix nano seconds.
// The maximum weight(priority) is 99999.
func CreateItem(bucket string, weight uint64, value string) *Item {
	if weight > 99999 {
		weight = 99999
	}
	// so that maximum weight comes first lexicographically
	priority := 99999 - weight
	return &Item{
		Bucket:    bucket,
		CreatedAt: time.Now(),
		Key:       path.Join(bucket, fmt.Sprintf("%05d%035X", priority, time.Now().UnixNano())),
		Value:     value,
		Progress:  0,
		Error:     "",
	}
}

// ItemWatcher is for clients that subscribes the status of job item.
type ItemWatcher <-chan *Item

// Queue is the queue service backed by etcd.
type Queue interface {
	// ClientEndpoints returns the client endpoints.
	ClientEndpoints() []string

	// Client returns the client.
	Client() *clientv3.Client

	// Stop stops the queue service.
	Stop()

	// Enqueue adds/overwrites an item in the queue. Updates are to be
	// done by other external worker services. The worker first fetches
	// the first item via 'Front' method, and writes back with 'Enqueue'
	// method. Enqueue returns a channel that notifies any events on the
	// item. The channel is closed when the job is completed or canceled.
	Enqueue(ctx context.Context, it *Item) ItemWatcher

	// Front returns ItemWatcher that returns the first item in the queue.
	// It blocks until there is at least one item to return.
	Front(ctx context.Context, bucket string) ItemWatcher

	// Dequeue deletes the item in the queue, whether the item is completed
	// or in progress. The item needs not be the first one in the queue.
	Dequeue(ctx context.Context, it *Item) error

	// Watch creates a item watcher, assuming that the job is already scheduled
	// by 'Enqueue' method. The returned channel is never closed until the
	// context is canceled.
	Watch(ctx context.Context, key string) ItemWatcher
}

const (
	pfxScheduled = "_schd" // requested by client, added to queue
	pfxCompleted = "_cmpl" // finished by worker

	// MaxProgress is the progress value when the job is done!
	MaxProgress = 100
)

type queue struct {
	mu         sync.RWMutex
	cli        *clientv3.Client
	rootCtx    context.Context
	rootCancel func()
}

// embeddedQueue implements Queue interface with a single-node embedded etcd cluster.
type embeddedQueue struct {
	srv *embed.Etcd
	Queue
}

// NewEmbeddedQueue starts a new embedded etcd server.
// cport is the TCP port used for etcd client request serving.
// pport is for etcd peer traffic, and still needed even if it's a single-node cluster.
func NewEmbeddedQueue(ctx context.Context, cport, pport int, dataDir string) (Queue, error) {
	cfg := embed.NewConfig()
	cfg.ClusterState = embed.ClusterStateFlagNew

	cfg.Name = "etcd-queue"
	cfg.Dir = dataDir

	curl := url.URL{Scheme: "http", Host: fmt.Sprintf("localhost:%d", cport)}
	cfg.ACUrls, cfg.LCUrls = []url.URL{curl}, []url.URL{curl}

	purl := url.URL{Scheme: "http", Host: fmt.Sprintf("localhost:%d", pport)}
	cfg.APUrls, cfg.LPUrls = []url.URL{purl}, []url.URL{purl}

	cfg.InitialCluster = fmt.Sprintf("%s=%s", cfg.Name, cfg.APUrls[0].String())

	// auto-compaction every hour
	cfg.AutoCompactionRetention = 1
	// single-node, so aggressively snapshot/discard Raft log entries
	cfg.SnapCount = 1000

	glog.Infof("starting %q with endpoint %q", cfg.Name, curl.String())
	srv, err := embed.StartEtcd(cfg)
	if err != nil {
		return nil, err
	}
	select {
	case <-srv.Server.ReadyNotify():
		err = nil
	case err = <-srv.Err():
	case <-srv.Server.StopNotify():
		err = fmt.Errorf("received from etcdserver.Server.StopNotify")
	case <-ctx.Done():
		err = ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	glog.Infof("started %q with endpoint %q", cfg.Name, curl.String())

	cli := v3client.New(srv.Server)

	// issue linearized read to ensure leader election
	glog.Infof("sending GET to endpoint %q", curl.String())
	_, err = cli.Get(ctx, "foo")
	glog.Infof("sent GET to endpoint %q (error: %v)", curl.String(), err)

	cctx, cancel := context.WithCancel(ctx)
	return &embeddedQueue{
		srv: srv,
		Queue: &queue{
			cli:        cli,
			rootCtx:    cctx,
			rootCancel: cancel,
		},
	}, err
}

func (qu *embeddedQueue) ClientEndpoints() []string {
	eps := make([]string, 0, len(qu.srv.Config().LCUrls))
	for i := range qu.srv.Config().LCUrls {
		eps = append(eps, qu.srv.Config().LCUrls[i].String())
	}
	return eps
}

func (qu *embeddedQueue) Stop() {
	glog.Info("stopping queue with an embedded etcd server")
	qu.Queue.Stop()
	qu.srv.Close()
	glog.Info("stopped queue with an embedded etcd server")
}

// NewQueue creates a new queue from given etcd client.
func NewQueue(cli *clientv3.Client) (Queue, error) {
	// issue linearized read to ensure leader election
	glog.Infof("GET request to endpoint %v", cli.Endpoints())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, err := cli.Get(ctx, "foo")
	cancel()
	glog.Infof("GET request succeeded on endpoint %v", cli.Endpoints())
	if err != nil {
		return nil, err
	}

	ctx, cancel = context.WithCancel(context.Background())
	return &queue{
		cli:        cli,
		rootCtx:    ctx,
		rootCancel: cancel,
	}, nil
}

func (qu *queue) ClientEndpoints() []string { return qu.cli.Endpoints() }
func (qu *queue) Client() *clientv3.Client  { return qu.cli }
func (qu *queue) Stop() {
	qu.mu.Lock()
	defer qu.mu.Unlock()

	glog.Info("stopping queue")
	qu.rootCancel()
	qu.cli.Close()
	glog.Info("stopped queue")
}

func (qu *queue) put(ctx context.Context, key, val string) error {
	_, err := qu.cli.Put(ctx, key, val)
	return err
}

func (qu *queue) delete(ctx context.Context, key string) error {
	_, err := qu.cli.Delete(ctx, key)
	return err
}

func (qu *queue) Enqueue(ctx context.Context, item *Item) ItemWatcher {
	// TODO: make this configurable
	ch := make(chan *Item, 100)

	if item == nil {
		ch <- &Item{Error: "received <nil> Item"}
		close(ch)
		return ch
	}

	cur := *item
	key := path.Join(pfxScheduled, cur.Key)

	data, err := json.Marshal(&cur)
	if err != nil {
		cur.Error = err.Error()
		ch <- &cur
		close(ch)
		return ch
	}
	val := string(data)

	qu.mu.Lock()
	defer qu.mu.Unlock()

	if err = qu.put(ctx, key, val); err != nil {
		cur.Error = err.Error()
		ch <- &cur
		close(ch)
		return ch
	}
	glog.Infof("enqueue: wrote %q", item.Key)

	if cur.Progress == MaxProgress {
		if err = qu.delete(ctx, key); err != nil {
			cur.Error = err.Error()
			ch <- &cur
			close(ch)
			return ch
		}

		if err := qu.put(ctx, path.Join(pfxCompleted, cur.Key), val); err != nil {
			cur.Error = err.Error()
			ch <- &cur
			close(ch)
			return ch
		}

		glog.Infof("enqueue: %q is finished", cur.Key)
		ch <- &cur
		close(ch)
		return ch
	}

	wch := qu.cli.Watch(ctx, key, clientv3.WithPrevKV())
	go func() {
		for {
			select {
			case wresp := <-wch:
				if len(wresp.Events) != 1 {
					cur.Error = fmt.Sprintf("enqueue-watcher: %q expects 1 event from watch, got %+v", cur.Key, wresp.Events)
					ch <- &cur
					close(ch)
					return
				}

				if wresp.Events[0].Type == mvccpb.DELETE {
					glog.Infof("enqueue-watcher: %q has been deleted; either completed or canceled", cur.Key)
					var prev Item
					if err := json.Unmarshal(wresp.Events[0].PrevKv.Value, &prev); err != nil {
						prev.Error = fmt.Sprintf("enqueue-watcher: cannot parse %q", string(wresp.Events[0].PrevKv.Value))
						ch <- &prev
						close(ch)
						return
					}

					if prev.Progress != 100 {
						prev.Canceled = true
						glog.Infof("enqueue-watcher: found %q progress is only %d (canceled)", prev.Key, prev.Progress)
					}

					ch <- &prev
					close(ch)
					return
				}

				if err := json.Unmarshal(wresp.Events[0].Kv.Value, &cur); err != nil {
					cur.Error = fmt.Sprintf("enqueue-watcher: cannot parse %q", string(wresp.Events[0].Kv.Value))
					ch <- &cur
					close(ch)
					return
				}

				ch <- &cur
				if cur.Error != "" {
					glog.Warningf("enqueue-watcher: %q contains error %v", cur.Key, cur.Error)
					close(ch)
					return
				}
				if cur.Progress == 100 {
					glog.Infof("enqueue-watcher: %q is finished", cur.Key)
					close(ch)
					return
				}
				glog.Infof("enqueue-watcher: %q has been updated (waiting for next updates)", cur.Key)

			case <-ctx.Done():
				cur.Error = ctx.Err().Error()
				ch <- &cur
				close(ch)
				return
			}
		}
	}()
	return ch
}

func (qu *queue) Front(ctx context.Context, bucket string) ItemWatcher {
	scheduledKey := path.Join(pfxScheduled, bucket)
	ch := make(chan *Item, 1)

	resp, err := qu.cli.Get(ctx, scheduledKey, clientv3.WithFirstKey()...)
	if err != nil {
		ch <- &Item{Error: err.Error()}
		close(ch)
		return ch
	}

	if len(resp.Kvs) == 0 {
		wch := qu.cli.Watch(ctx, scheduledKey, clientv3.WithPrefix())
		go func() {
			defer close(ch)

			select {
			case wresp := <-wch:
				if len(wresp.Events) != 1 {
					ch <- &Item{Error: fmt.Sprintf("%q did not return 1 event via watch (got %+v)", scheduledKey, wresp)}
					return
				}
				v := wresp.Events[0].Kv.Value
				var item Item
				if err := json.Unmarshal(v, &item); err != nil {
					ch <- &Item{Error: fmt.Sprintf("%q returned wrong JSON value %q (%v)", scheduledKey, string(v), err)}
				} else {
					ch <- &item
				}
			case <-ctx.Done():
				ch <- &Item{Error: ctx.Err().Error()}
			}
		}()
		return ch
	}

	if len(resp.Kvs) != 1 {
		ch <- &Item{Error: fmt.Sprintf("%q returned more than 1 key", scheduledKey)}
		close(ch)
		return ch
	}
	v := resp.Kvs[0].Value
	var item Item
	if err := json.Unmarshal(v, &item); err != nil {
		ch <- &Item{Error: fmt.Sprintf("%q returned wrong JSON value %q (%v)", scheduledKey, string(v), err)}
		close(ch)
	} else {
		ch <- &item
	}
	return ch
}

func (qu *queue) Dequeue(ctx context.Context, it *Item) error {
	key := path.Join(pfxScheduled, it.Key)

	qu.mu.Lock()
	defer qu.mu.Unlock()

	glog.Infof("dequeue-ing %q", key)
	if err := qu.delete(ctx, key); err != nil {
		return err
	}
	glog.Infof("dequeue-ed %q", key)
	return nil
}

func (qu *queue) Watch(ctx context.Context, key string) ItemWatcher {
	glog.Infof("watch: started watching on %q", key)

	key = path.Join(pfxScheduled, key)
	ch := make(chan *Item, 100)

	wch := qu.cli.Watch(ctx, key)
	go func() {
		for {
			select {
			case wresp := <-wch:
				if len(wresp.Events) != 1 {
					ch <- &Item{Error: fmt.Sprintf("watch: %q did not return 1 event via watch (got %+v)", key, wresp)}
					continue
				}
				glog.Infof("watch: received event on %q", key)
				v := wresp.Events[0].Kv.Value
				var item Item
				if err := json.Unmarshal(v, &item); err != nil {
					ch <- &Item{Error: fmt.Sprintf("watch: %q returned wrong JSON value %q (%v)", key, string(v), err)}
				} else {
					ch <- &item
					glog.Infof("watch: sent event on %q", key)
				}
			case <-ctx.Done():
				glog.Infof("watch: canceled on %q (closing channel)", key)
				close(ch)
				return
			}
		}
	}()

	return ch
}
