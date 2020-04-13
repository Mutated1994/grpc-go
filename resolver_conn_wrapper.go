/*
 *
 * Copyright 2017 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package grpc

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/balancer"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/internal/channelz"
	"google.golang.org/grpc/internal/grpcsync"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/serviceconfig"
)

// ccResolverWrapper is a wrapper on top of cc for resolvers.
// It implements resolver.ClientConn interface.
type ccResolverWrapper struct {
	cc         *ClientConn
	resolverMu sync.Mutex
	resolver   resolver.Resolver
	done       *grpcsync.Event
	curState   resolver.State

	pollingMu sync.Mutex
	polling   chan struct{}
}

// newCCResolverWrapper uses the resolver.Builder to build a Resolver and
// returns a ccResolverWrapper object which wraps the newly built resolver.
func newCCResolverWrapper(cc *ClientConn, rb resolver.Builder) (*ccResolverWrapper, error) {
	// 好一个套娃
	ccr := &ccResolverWrapper{
		cc:   cc, // ClientConn
		done: grpcsync.NewEvent(), // cc 里面也有这个
	}

	// 跳过
	var credsClone credentials.TransportCredentials
	if creds := cc.dopts.copts.TransportCredentials; creds != nil {
		credsClone = creds.Clone()
	}

	// 这里也没有干啥
	rbo := resolver.BuildOptions{
		DisableServiceConfig: cc.dopts.disableServiceConfig, // false
		DialCreds:            credsClone, // nil
		CredsBundle:          cc.dopts.copts.CredsBundle, // nil
		Dialer:               cc.dopts.copts.Dialer,
	}

	var err error
	// We need to hold the lock here while we assign to the ccr.resolver field
	// to guard against a data race caused by the following code path,
	// rb.Build-->ccr.ReportError-->ccr.poll-->ccr.resolveNow, would end up
	// accessing ccr.resolver which is being assigned here.
	ccr.resolverMu.Lock()
	defer ccr.resolverMu.Unlock()
	// rb.Build (*passthroughBuilder) Build
	ccr.resolver, err = rb.Build(cc.parsedTarget, ccr, rbo)
	if err != nil {
		return nil, err
	}
	return ccr, nil
}

func (ccr *ccResolverWrapper) resolveNow(o resolver.ResolveNowOptions) {
	ccr.resolverMu.Lock()
	if !ccr.done.HasFired() {
		ccr.resolver.ResolveNow(o)
	}
	ccr.resolverMu.Unlock()
}

func (ccr *ccResolverWrapper) close() {
	ccr.resolverMu.Lock()
	ccr.resolver.Close()
	ccr.done.Fire()
	ccr.resolverMu.Unlock()
}

// poll begins or ends asynchronous polling of the resolver based on whether
// err is ErrBadResolverState.
// 这个函数可以忽略，暂时用不到
func (ccr *ccResolverWrapper) poll(err error) {
	ccr.pollingMu.Lock()
	defer ccr.pollingMu.Unlock()
	if err != balancer.ErrBadResolverState {
		// stop polling
		if ccr.polling != nil {
			close(ccr.polling)
			ccr.polling = nil
		}
		return // 直接就退出了
	}
	if ccr.polling != nil {
		// already polling
		return
	}
	p := make(chan struct{})
	ccr.polling = p
	go func() {
		for i := 0; ; i++ {
			ccr.resolveNow(resolver.ResolveNowOptions{})
			t := time.NewTimer(ccr.cc.dopts.resolveNowBackoff(i))
			select {
			case <-p:
				t.Stop()
				return
			case <-ccr.done.Done():
				// Resolver has been closed.
				t.Stop()
				return
			case <-t.C:
				select {
				case <-p:
					return
				default:
				}
				// Timer expired; re-resolve.
			}
		}
	}()
}

func (ccr *ccResolverWrapper) UpdateState(s resolver.State) {
	// UpdateState 理论上可以可以被多次调用
	// 但是调用 ClientConn.Close() 之后，将无法调用 UpdateState
	if ccr.done.HasFired() {
		return
	}
	channelz.Infof(ccr.cc.channelzID, "ccResolverWrapper: sending update to cc: %v", s)
	if channelz.IsOn() {
		ccr.addChannelzTraceEvent(s)
	}
	// resolver.State{
	//		Addresses: []resolver.Address{
	//			{Addr: r.target.Endpoint}, // localhost:50051 or "10.1.1.2:33,10.1.1.2:32"
	//		},
	//	}
	ccr.curState = s
	// 重点看 ccr.cc.updateResolverState，ccr.poll请忽略
	ccr.poll(ccr.cc.updateResolverState(ccr.curState, nil))
}

func (ccr *ccResolverWrapper) ReportError(err error) {
	if ccr.done.HasFired() {
		return
	}
	channelz.Warningf(ccr.cc.channelzID, "ccResolverWrapper: reporting error to cc: %v", err)
	ccr.poll(ccr.cc.updateResolverState(resolver.State{}, err))
}

// NewAddress is called by the resolver implementation to send addresses to gRPC.
func (ccr *ccResolverWrapper) NewAddress(addrs []resolver.Address) {
	if ccr.done.HasFired() {
		return
	}
	channelz.Infof(ccr.cc.channelzID, "ccResolverWrapper: sending new addresses to cc: %v", addrs)
	if channelz.IsOn() {
		ccr.addChannelzTraceEvent(resolver.State{Addresses: addrs, ServiceConfig: ccr.curState.ServiceConfig})
	}
	ccr.curState.Addresses = addrs
	ccr.poll(ccr.cc.updateResolverState(ccr.curState, nil))
}

// NewServiceConfig is called by the resolver implementation to send service
// configs to gRPC.
func (ccr *ccResolverWrapper) NewServiceConfig(sc string) {
	if ccr.done.HasFired() {
		return
	}
	channelz.Infof(ccr.cc.channelzID, "ccResolverWrapper: got new service config: %v", sc)
	if ccr.cc.dopts.disableServiceConfig {
		channelz.Info(ccr.cc.channelzID, "Service config lookups disabled; ignoring config")
		return
	}
	scpr := parseServiceConfig(sc)
	if scpr.Err != nil {
		channelz.Warningf(ccr.cc.channelzID, "ccResolverWrapper: error parsing service config: %v", scpr.Err)
		ccr.poll(balancer.ErrBadResolverState)
		return
	}
	if channelz.IsOn() {
		ccr.addChannelzTraceEvent(resolver.State{Addresses: ccr.curState.Addresses, ServiceConfig: scpr})
	}
	ccr.curState.ServiceConfig = scpr
	ccr.poll(ccr.cc.updateResolverState(ccr.curState, nil))
}

func (ccr *ccResolverWrapper) ParseServiceConfig(scJSON string) *serviceconfig.ParseResult {
	return parseServiceConfig(scJSON)
}

func (ccr *ccResolverWrapper) addChannelzTraceEvent(s resolver.State) {
	var updates []string
	var oldSC, newSC *ServiceConfig
	var oldOK, newOK bool
	if ccr.curState.ServiceConfig != nil {
		oldSC, oldOK = ccr.curState.ServiceConfig.Config.(*ServiceConfig)
	}
	if s.ServiceConfig != nil {
		newSC, newOK = s.ServiceConfig.Config.(*ServiceConfig)
	}
	if oldOK != newOK || (oldOK && newOK && oldSC.rawJSONString != newSC.rawJSONString) {
		updates = append(updates, "service config updated")
	}
	if len(ccr.curState.Addresses) > 0 && len(s.Addresses) == 0 {
		updates = append(updates, "resolver returned an empty address list")
	} else if len(ccr.curState.Addresses) == 0 && len(s.Addresses) > 0 {
		updates = append(updates, "resolver returned new addresses")
	}
	channelz.AddTraceEvent(ccr.cc.channelzID, 0, &channelz.TraceEventDesc{
		Desc:     fmt.Sprintf("Resolver state updated: %+v (%v)", s, strings.Join(updates, "; ")),
		Severity: channelz.CtINFO,
	})
}
