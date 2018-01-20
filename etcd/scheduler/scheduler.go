// Mgmt
// Copyright (C) 2013-2018+ James Shubin and the project contributors
// Written by James Shubin <james@shubin.ca> and the project contributors
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

// Package scheduler implements a distributed consensus scheduler with etcd.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	etcd "github.com/coreos/etcd/clientv3"
	"github.com/coreos/etcd/clientv3/concurrency"
	pb "github.com/coreos/etcd/etcdserver/etcdserverpb"
	errwrap "github.com/pkg/errors"
)

const (
	// DefaultMaxCount is the maximum number of hosts to schedule on if not
	// specified.
	DefaultMaxCount = 1 // TODO: what is the logical value to choose? +Inf?
)

// ErrEndOfResults is a sentinel that represents no more results will be coming.
var ErrEndOfResults = errors.New("scheduler: end of results")

var schedulerLeases = make(map[string]etcd.LeaseID) // process lifetime in-memory lease store

// schedulerResult represents output from the scheduler.
type schedulerResult struct {
	hosts []string
	err   error
}

// Result is what is returned when you request a scheduler. You can call methods
// on it, and it stores the necessary state while you're running. When one of
// these is produced, the scheduler has already kicked off running for you
// automatically.
type Result struct {
	results   chan *schedulerResult
	closeFunc func() // run this when you're done with the scheduler // TODO: replace with an input `context`
}

// Next returns the next output from the scheduler when it changes. This blocks
// until a new value is available, which is why you may wish to use a context to
// cancel any read from this. It returns ErrEndOfResults if the scheduler shuts
// down.
func (obj *Result) Next(ctx context.Context) ([]string, error) {
	select {
	case val, ok := <-obj.results:
		if !ok {
			return nil, ErrEndOfResults
		}
		return val.hosts, val.err

	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Shutdown causes everything to clean up. We no longer need the scheduler.
// TODO: should this be named Close() instead? Should it return an error?
func (obj *Result) Shutdown() {
	obj.closeFunc()
	// XXX: should we have a waitgroup to wait for it all to close?
}

// TODO: use: https://github.com/coreos/etcd/pull/8488 when available
func leaseValue(key string) etcd.Cmp {
	return etcd.Cmp{Key: []byte(key), Target: pb.Compare_LEASE}
}

// Schedule returns a scheduler result which can be queried with it's available
// methods. This automatically causes different etcd clients sharing the same
// path to discover each other and be part of the scheduled set. On close the
// keys expire and will get removed from the scheduled set. Different options
// can be passed in to customize the behaviour. Hostname represents the unique
// identifier for the caller. The behaviour is undefined if this is run more
// than once with the same path and hostname simultaneously.
func Schedule(client *etcd.Client, path string, hostname string, opts ...Option) (*Result, error) {
	if strings.HasSuffix(path, "/") {
		return nil, fmt.Errorf("scheduler: path must not end with the slash char")
	}
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("scheduler: path must start with the slash char")
	}

	// key structure is $path/election = ???
	// key structure is $path/exchange/$hostname = ???

	options := &schedulerOptions{ // default scheduler options
		reuseLease: true, // XXX: is this a safe default?
		maxCount:   DefaultMaxCount,
	}
	for _, optionFunc := range opts { // apply the scheduler options
		optionFunc(options)
	}

	if options.strategy == nil {
		return nil, fmt.Errorf("scheduler: strategy must be specified")
	}

	sessionOptions := []concurrency.SessionOption{}

	// here we try to re-use lease between multiple runs of the code
	// TODO: is it a good idea to try and re-use the lease b/w runs?
	if options.reuseLease {
		if leaseID, exists := schedulerLeases[path]; exists {
			sessionOptions = append(sessionOptions, concurrency.WithLease(leaseID))
		}
	}

	session, err := concurrency.NewSession(client, sessionOptions...)
	if err != nil {
		return nil, errwrap.Wrapf(err, "scheduler: could not create session")
	}
	leaseID := session.Lease()
	if options.reuseLease {
		// save for next time, otherwise run session.Close() somewhere
		schedulerLeases[path] = leaseID
	}

	ctx, cancel := context.WithCancel(context.Background()) // cancel below
	defer cancel()                                          // XXX: here for now... TBD

	// exchange hostname, and attach it to session (leaseID) so it expires
	// (gets deleted) when we disconnect...
	exchangePath := fmt.Sprintf("%s/exchange", path)
	exchangePathHost := fmt.Sprintf("%s/%s", exchangePath, hostname)
	exchangePathPrefix := fmt.Sprintf("%s/", exchangePath)

	// open the watch *before* we set our key so that we can see the change!
	watchChan := client.Watcher.Watch(ctx, exchangePathPrefix, etcd.WithPrefix())

	data := "TODO" // XXX: no data to exchange alongside hostnames yet
	ifops := []etcd.Cmp{
		etcd.Compare(etcd.Value(exchangePathHost), "=", data),
		etcd.Compare(leaseValue(exchangePathHost), "=", int64(leaseID)), // XXX: remove int64() after 3.3.0
	}
	elsop := etcd.OpPut(exchangePathHost, data, etcd.WithLease(leaseID))

	// it's important to do this in one transaction, and atomically, because
	// this way, we only generate one watch event, and only when it's needed
	// updating leaseID, or key expiry (deletion) both generate watch events
	// XXX: context!!!
	if txn, err := client.KV.Txn(context.TODO()).If(ifops...).Then([]etcd.Op{}...).Else(elsop).Commit(); err != nil {
		return nil, errwrap.Wrapf(err, "could not exchange in `%s`", path)
	} else if txn.Succeeded {
		options.logf("txn did nothing...") // then branch
	} else {
		options.logf("txn did and update...")
	}

	// create an election object
	electionPath := fmt.Sprintf("%s/election", path)
	election := concurrency.NewElection(session, electionPath)
	electionChan := election.Observe(ctx)

	elected := "" // who we "assume" is elected
	wg := &sync.WaitGroup{}
	ch := make(chan *schedulerResult)
	closeChan := make(chan struct{})
	send := func(hosts []string, err error) bool { // helper function for sending
		select {
		case ch <- &schedulerResult{ // send
			hosts: hosts,
			err:   err,
		}:
			return true
		case <-closeChan: // unblock
			return false // not sent
		}
	}

	once := &sync.Once{}
	onceBody := func() { // do not call directly, use closeFunc!
		// request a graceful shutdown, caller must call this to
		// shutdown when they are finished with the scheduler...
		// calling this will cause their hosts channels to close
		close(closeChan) // send a close signal
	}
	closeFunc := func() {
		once.Do(onceBody)
	}
	result := &Result{
		results: ch,
		// TODO: we could accept a context to watch for cancel instead?
		closeFunc: closeFunc,
	}

	mutex := &sync.Mutex{}
	var campaignClose chan struct{}
	var campaignRunning bool
	// goroutine to vote for someone as scheduler! each participant must be
	// able to run this or nobody will be around to vote if others are down
	campaignFunc := func() {
		// the mutex ensures we don't fly past the wg.Wait() if someone
		// shuts down the scheduler right as we are about to start this
		// campaigning loop up. we do not want to fail unnecessarily...
		mutex.Lock()
		wg.Add(1)
		mutex.Unlock()
		go func() {
			defer wg.Done()

			ctx, cancel := context.WithCancel(context.Background())
			go func() {
				defer cancel() // run cancel to stop campaigning...
				select {
				case <-campaignClose:
					return
				case <-closeChan:
					return
				}
			}()
			for {
				// TODO: previously, this looped infinitely fast
				//fmt.Printf(".") // debug

				// "Campaign puts a value as eligible for the election.
				// It blocks until it is elected, an error occurs, or
				// the context is cancelled."

				// vote for ourselves, as it's the only host we can
				// guarantee is alive, otherwise we wouldn't be voting!
				// it would be more sensible to vote for the last valid
				// hostname to keep things more stable, but if that
				// information was stale, and that host wasn't alive,
				// then this would defeat the point of picking them!
				if err := election.Campaign(ctx, hostname); err != nil {
					if err != context.Canceled {
						send(nil, errwrap.Wrapf(err, "scheduler: error campaigning"))
					}
					return
				}
			}
		}()
	}

	go func() {
		defer close(ch)
		if !options.reuseLease {
			defer session.Close() // this revokes the lease...
		}

		defer func() {
			// XXX: should we ever resign? why would this block and thus need a context?
			if elected == hostname { // TODO: is it safe to just always do this?
				if err := election.Resign(context.TODO()); err != nil { // XXX: add a timeout?
				}
			}
			elected = "" // we don't care anymore!
		}()

		// this "last" defer (first to run) should block until the other
		// goroutine has closed so we don't Close an in-use session, etc
		defer wg.Wait()

		go func() {
			defer cancel() // run cancel to "free" Observe...

			defer wg.Wait() // also wait here if parent exits first

			select {
			case <-closeChan:
				// we want the above wg.Wait() to work if this
				// close happens. lock with the campaign start
				defer mutex.Unlock()
				mutex.Lock()
				return
			}
		}()
		hostnames := make(map[string]string)
		for {
			select {
			case val, ok := <-electionChan:
				if options.debug {
					options.logf("electionChan(%t): %+v", ok, electionChan)
				}
				if !ok {
					if options.debug {
						options.logf("elections stream shutdown...")
					}
					electionChan = nil
					// done
					// TODO: do we need to send on error channel?
					// XXX: maybe if context was not called to exit us?

					// ensure everyone waiting on closeChan
					// gets cleaned up so we free mem, etc!
					if watchChan == nil { // both now closed
						closeFunc()
						return
					}
					continue

				}

				elected = string(val.Kvs[0].Value)
				//if options.debug {
				options.logf("elected: %s", elected)
				//}
				if elected != hostname { // not me!
					// start up the campaign function
					if !campaignRunning {
						campaignClose = make(chan struct{})
						campaignFunc() // run
						campaignRunning = true
					}
					continue // someone else does the scheduling...
				} else { // campaigning while i am it loops fast
					// shutdown the campaign function
					if campaignRunning {
						close(campaignClose)
						wg.Wait()
						campaignRunning = false
					}
				}

				// i was voted in to make the scheduling choice!

			case watchResp, ok := <-watchChan:
				if options.debug {
					options.logf("watchChan(%t): %+v", ok, watchResp)
				}
				if !ok {
					if options.debug {
						options.logf("watch stream shutdown...")
					}
					watchChan = nil
					// done
					// TODO: do we need to send on error channel?
					// XXX: maybe if context was not called to exit us?

					// ensure everyone waiting on closeChan
					// gets cleaned up so we free mem, etc!
					if electionChan == nil { // both now closed
						closeFunc()
						return
					}
					continue
				}

				err := watchResp.Err()
				if watchResp.Canceled || err == context.Canceled {
					// channel get closed shortly...
					continue
				}
				if watchResp.Header.Revision == 0 { // by inspection
					// received empty message ?
					// switched client connection ?
					// FIXME: what should we do here ?
					continue
				}
				if err != nil {
					send(nil, errwrap.Wrapf(err, "scheduler: exchange watcher failed"))
					continue
				}
				if len(watchResp.Events) == 0 { // nothing interesting
					continue
				}

				resp, err := client.Get(ctx, exchangePathPrefix, etcd.WithPrefix(), etcd.WithSort(etcd.SortByKey, etcd.SortAscend))
				if err != nil || resp == nil {
					send(nil, fmt.Errorf("scheduler: could not get exchange values, error: %v", err))
					continue
				}

				// FIXME: the value key could instead be host
				// specific information which is used for some
				// purpose, eg: seconds active, and other data?
				hostnames = make(map[string]string) // reset
				for _, x := range resp.Kvs {
					k := string(x.Key)
					if !strings.HasPrefix(k, exchangePathPrefix) {
						continue
					}
					k = k[len(exchangePathPrefix):] // strip
					hostnames[k] = string(x.Value)
				}

				// NOTE: not needed, exit this via Observe ctx cancel,
				// which will ultimately cause the chan to shutdown...
				//case <-closeChan:
				//	return
			}

			if len(hostnames) == 0 {
				if options.debug {
					options.logf("zero hosts available")
				}
				continue // not enough hosts available
			}

			// run actual scheduler and decide who should be chosen
			// TODO: is there any additional data that we can pass
			// to the scheduler so it can make a better decision ?
			hosts, err := options.strategy.Schedule(hostnames, options)
			if err != nil {
				send(nil, errwrap.Wrapf(err, "scheduler: strategy failed"))
				continue
			}
			sort.Strings(hosts) // for consistency

			if options.debug {
				options.logf("sending hosts: %+v", hosts)
			}
			// send that on channel!
			if !send(hosts, nil) {
				//return // pass instead, let channels clean up
			}
		}
	}()

	return result, nil
}