/*
 *
 * Copyright 2014 gRPC authors.
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
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/balancer"
	"google.golang.org/grpc/balancer/base"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/internal/backoff"
	"google.golang.org/grpc/internal/channelz"
	"google.golang.org/grpc/internal/grpcsync"
	"google.golang.org/grpc/internal/grpcutil"
	"google.golang.org/grpc/internal/transport"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/serviceconfig"
	"google.golang.org/grpc/status"

	_ "google.golang.org/grpc/balancer/roundrobin"           // To register roundrobin.
	_ "google.golang.org/grpc/internal/resolver/dns"         // To register dns resolver.
	_ "google.golang.org/grpc/internal/resolver/passthrough" // To register passthrough resolver.
)

const (
	// minimum time to give a connection to complete
	minConnectTimeout = 20 * time.Second
	// must match grpclbName in grpclb/grpclb.go
	grpclbName = "grpclb"
)

var (
	// ErrClientConnClosing indicates that the operation is illegal because
	// the ClientConn is closing.
	//
	// Deprecated: this error should not be relied upon by users; use the status
	// code of Canceled instead.
	ErrClientConnClosing = status.Error(codes.Canceled, "grpc: the client connection is closing")
	// errConnDrain indicates that the connection starts to be drained and does not accept any new RPCs.
	errConnDrain = errors.New("grpc: the connection is drained")
	// errConnClosing indicates that the connection is closing.
	errConnClosing = errors.New("grpc: the connection is closing")
	// errBalancerClosed indicates that the balancer is closed.
	errBalancerClosed = errors.New("grpc: balancer is closed")
	// invalidDefaultServiceConfigErrPrefix is used to prefix the json parsing error for the default
	// service config.
	invalidDefaultServiceConfigErrPrefix = "grpc: the provided default service config is invalid"
)

// The following errors are returned from Dial and DialContext
var (
	// errNoTransportSecurity indicates that there is no transport security
	// being set for ClientConn. Users should either set one or explicitly
	// call WithInsecure DialOption to disable security.
	errNoTransportSecurity = errors.New("grpc: no transport security set (use grpc.WithInsecure() explicitly or set credentials)")
	// errTransportCredsAndBundle indicates that creds bundle is used together
	// with other individual Transport Credentials.
	errTransportCredsAndBundle = errors.New("grpc: credentials.Bundle may not be used with individual TransportCredentials")
	// errTransportCredentialsMissing indicates that users want to transmit security
	// information (e.g., OAuth2 token) which requires secure connection on an insecure
	// connection.
	errTransportCredentialsMissing = errors.New("grpc: the credentials require transport level security (use grpc.WithTransportCredentials() to set)")
	// errCredentialsConflict indicates that grpc.WithTransportCredentials()
	// and grpc.WithInsecure() are both called for a connection.
	errCredentialsConflict = errors.New("grpc: transport credentials are set for an insecure connection (grpc.WithTransportCredentials() and grpc.WithInsecure() are both called)")
)

const (
	defaultClientMaxReceiveMessageSize = 1024 * 1024 * 4
	defaultClientMaxSendMessageSize    = math.MaxInt32
	// http2IOBufSize specifies the buffer size for sending frames.
	defaultWriteBufSize = 32 * 1024
	defaultReadBufSize  = 32 * 1024
)

// Dial creates a client connection to the given target.
func Dial(target string, opts ...DialOption) (*ClientConn, error) {
	return DialContext(context.Background(), target, opts...)
}

// DialContext creates a client connection to the given target. By default, it's
// a non-blocking dial (the function won't wait for connections to be
// established, and connecting happens in the background). To make it a blocking
// dial, use WithBlock() dial option.
//
// In the non-blocking case, the ctx does not act against the connection. It
// only controls the setup steps.
//
// In the blocking case, ctx can be used to cancel or expire the pending
// connection. Once this function returns, the cancellation and expiration of
// ctx will be noop. Users should call ClientConn.Close to terminate all the
// pending operations after this function returns.
//
// The target name syntax is defined in
// https://github.com/grpc/grpc/blob/master/doc/naming.md.
// e.g. to use dns resolver, a "dns:///" prefix should be applied to the target.
func DialContext(ctx context.Context, target string, opts ...DialOption) (conn *ClientConn, err error) {
	cc := &ClientConn{
		target:            target,
		csMgr:             &connectivityStateManager{}, // 连接状态管理
		conns:             make(map[*addrConn]struct{}),
		dopts:             defaultDialOptions(), // dialOptions
		blockingpicker:    newPickerWrapper(), // PickerWrapper 应该是用于并发安全的
		czData:            new(channelzData), // related data for ClientConn, addrConn and Server 一些时间戳的。
		firstResolveEvent: grpcsync.NewEvent(), // 用来判断 resolve 是否执行过一次
	}
	cc.retryThrottler.Store((*retryThrottler)(nil)) // 重试限流 ，还没有找到其他地方再次设置它
	cc.ctx, cc.cancel = context.WithCancel(context.Background())

	for _, opt := range opts { // dopts 安排
		opt.apply(&cc.dopts)
	}

	// 这两个没有用到，也就是一个wrap
	chainUnaryClientInterceptors(cc)
	chainStreamClientInterceptors(cc)

	defer func() {
		if err != nil {
			// 这个回头再看 TODO 比较重要
			cc.Close()
		}
	}()

	// channelz data collection. 应该是类似 metric 的东西，所有channelz都可以先忽略
	if channelz.IsOn() {
		if cc.dopts.channelzParentID != 0 {
			cc.channelzID = channelz.RegisterChannel(&channelzChannel{cc}, cc.dopts.channelzParentID, target)
			channelz.AddTraceEvent(cc.channelzID, 0, &channelz.TraceEventDesc{
				Desc:     "Channel Created",
				Severity: channelz.CtINFO,
				Parent: &channelz.TraceEventDesc{
					Desc:     fmt.Sprintf("Nested Channel(id:%d) created", cc.channelzID),
					Severity: channelz.CtINFO,
				},
			})
		} else {
			cc.channelzID = channelz.RegisterChannel(&channelzChannel{cc}, 0, target)
			channelz.Info(cc.channelzID, "Channel Created")
		}
		cc.csMgr.channelzID = cc.channelzID
	}

	// 证书相关的 忽略
	if !cc.dopts.insecure {
		if cc.dopts.copts.TransportCredentials == nil && cc.dopts.copts.CredsBundle == nil {
			return nil, errNoTransportSecurity
		}
		if cc.dopts.copts.TransportCredentials != nil && cc.dopts.copts.CredsBundle != nil {
			return nil, errTransportCredsAndBundle
		}
	} else {
		if cc.dopts.copts.TransportCredentials != nil || cc.dopts.copts.CredsBundle != nil {
			return nil, errCredentialsConflict
		}
		for _, cd := range cc.dopts.copts.PerRPCCredentials {
			if cd.RequireTransportSecurity() {
				return nil, errTransportCredentialsMissing
			}
		}
	}

	// 跳过
	if cc.dopts.defaultServiceConfigRawJSON != nil {
		scpr := parseServiceConfig(*cc.dopts.defaultServiceConfigRawJSON)
		if scpr.Err != nil {
			return nil, fmt.Errorf("%s: %v", invalidDefaultServiceConfigErrPrefix, scpr.Err)
		}
		cc.dopts.defaultServiceConfig, _ = scpr.Config.(*ServiceConfig)
	}

	// kp 配置 在这里生效的，keepalive.ClientParameters默认值
	cc.mkp = cc.dopts.copts.KeepaliveParams

	// 官方包 tcp连接 (&net.Dialer{}).DialContext
	if cc.dopts.copts.Dialer == nil {
		cc.dopts.copts.Dialer = func(ctx context.Context, addr string) (net.Conn, error) {
			network, addr := parseDialTarget(addr) // tcp , 172.16.34.140:50051
			return (&net.Dialer{}).DialContext(ctx, network, addr) // 这里官方包就不展开了
		}
		// 感兴趣再深挖下，我们也用不到
		if cc.dopts.withProxy {
			cc.dopts.copts.Dialer = newProxyDialer(cc.dopts.copts.Dialer)
		}
	}

	if cc.dopts.copts.UserAgent != "" {
		cc.dopts.copts.UserAgent += " " + grpcUA
	} else {
		cc.dopts.copts.UserAgent = grpcUA // 默认就完事了 grpc-go/1.29.0-dev
	}

	// 这个是连接超时，用传入的ctx
	// This is valid if and only if WithBlock() is present.
	if cc.dopts.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cc.dopts.timeout)
		defer cancel()
	}
	// conn也直接变nil了
	defer func() {
		select {
		case <-ctx.Done():
			switch {
			case ctx.Err() == err:
				conn = nil
			case err == nil || !cc.dopts.returnLastError:
				conn, err = nil, ctx.Err()
			default:
				conn, err = nil, fmt.Errorf("%v: %v", ctx.Err(), err)
			}
		default:
		}
	}()

	// 跳过 sc 后续研究
	scSet := false
	if cc.dopts.scChan != nil {
		// Try to get an initial service config.
		select {
		case sc, ok := <-cc.dopts.scChan:
			if ok {
				cc.sc = &sc
				scSet = true
			}
		default:
		}
	}
	if cc.dopts.bs == nil { // 这个不重要
		// dopts.resolveNowBackoff: internalbackoff.DefaultExponential.Backoff
		// DefaultExponential 也就一个成员方法
		// DefaultExponential = Exponential{Config: grpcbackoff.DefaultConfig}
		// Backoff Strategy
		cc.dopts.bs = backoff.DefaultExponential
	}

	// Determine the resolver to use.
	// 解析target, 我们使用ip的方式直接走 resolverBuilder == nil
	// 10.1.1.1:23,10.1.1.1:22,10.1.1.1:21 Endpoint 也还会是这个
	cc.parsedTarget = grpcutil.ParseTarget(cc.target)
	// type Target struct {
	//	Scheme    string ""
	//	Authority string ""
	//	Endpoint  string localhost:50051
	//}
	channelz.Infof(cc.channelzID, "parsed scheme: %q", cc.parsedTarget.Scheme)
	resolverBuilder := cc.getResolver(cc.parsedTarget.Scheme) // nil

	if resolverBuilder == nil {
		// If resolver builder is still nil, the parsed target's scheme is
		// not registered. Fallback to default resolver and set Endpoint to
		// the original target.
		// resolver 包可以自己注册 resolverBuilder
		channelz.Infof(cc.channelzID, "scheme %q not registered, fallback to default scheme", cc.parsedTarget.Scheme)
		cc.parsedTarget = resolver.Target{
			Scheme:   resolver.GetDefaultScheme(), // passthrough
			Endpoint: target, // localhost:50051
		}
		// type Target struct {
		//	Scheme    string "passthrough"
		//	Authority string ""
		//	Endpoint  string localhost:50051
		//}
		resolverBuilder = cc.getResolver(cc.parsedTarget.Scheme) // google.golang.org/grpc/resolver passthroughBuilder
		if resolverBuilder == nil {
			return nil, fmt.Errorf("could not get resolver for default scheme: %q", cc.parsedTarget.Scheme)
		}
	}

	// 跳过
	creds := cc.dopts.copts.TransportCredentials
	if creds != nil && creds.Info().ServerName != "" {
		cc.authority = creds.Info().ServerName
	} else if cc.dopts.insecure && cc.dopts.authority != "" {
		cc.authority = cc.dopts.authority
	} else {
		// Use endpoint from "scheme://authority/endpoint" as the default
		// authority for ClientConn.
		cc.authority = cc.parsedTarget.Endpoint // localhost:50051 就是跟 http1 Host差不多
	}

	// 跳过
	if cc.dopts.scChan != nil && !scSet {
		// Blocking wait for the initial service config.
		select {
		case sc, ok := <-cc.dopts.scChan:
			if ok {
				cc.sc = &sc
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	// 跳过
	if cc.dopts.scChan != nil {
		go cc.scWatcher()
	}

	var credsClone credentials.TransportCredentials
	// 跳过
	if creds := cc.dopts.copts.TransportCredentials; creds != nil {
		credsClone = creds.Clone()
	}

	cc.balancerBuildOpts = balancer.BuildOptions{
		DialCreds:        credsClone, // nil
		CredsBundle:      cc.dopts.copts.CredsBundle, // nil
		Dialer:           cc.dopts.copts.Dialer, // 看上面备注 (&net.Dialer{}).DialContext
		ChannelzParentID: cc.channelzID, // 忽略 0
		Target:           cc.parsedTarget,
	}
	// type Target struct {
	//	Scheme    string  passthrough
	//	Authority string  ""
	//	Endpoint  string localhost:50051
	//}

	// Build the resolver.
	// 不要以为这里就快结束了，这才刚开始！！！东西贼多
	rWrapper, err := newCCResolverWrapper(cc, resolverBuilder)
	if err != nil {
		return nil, fmt.Errorf("failed to build resolver: %v", err)
	}
	cc.mu.Lock()
	cc.resolverWrapper = rWrapper // ccResolverWrapper
	cc.mu.Unlock()

	// A blocking dial blocks until the clientConn is ready.
	if cc.dopts.block { // 这个先不看了，我们现在都不block的
		for {
			s := cc.GetState()
			if s == connectivity.Ready {
				break
				// copts.FailOnNonTempDialError 这个设置为 true，TODO 是不是就可以返回了？
			} else if cc.dopts.copts.FailOnNonTempDialError && s == connectivity.TransientFailure {
				if err = cc.blockingpicker.connectionError(); err != nil {
					terr, ok := err.(interface {
						Temporary() bool
					})
					if ok && !terr.Temporary() {
						return nil, err
					}
				}
			}
			//  跟 cc.csMgr.getNotifyChan() 跟 传入ctx 有关
			if !cc.WaitForStateChange(ctx, s) {
				// ctx got timeout or canceled.
				if err = cc.blockingpicker.connectionError(); err != nil && cc.dopts.returnLastError {
					return nil, err
				}
				return nil, ctx.Err()
			}
		}
	}

	return cc, nil
}

// chainUnaryClientInterceptors chains all unary client interceptors into one.
func chainUnaryClientInterceptors(cc *ClientConn) {
	interceptors := cc.dopts.chainUnaryInts
	// Prepend dopts.unaryInt to the chaining interceptors if it exists, since unaryInt will
	// be executed before any other chained interceptors.
	if cc.dopts.unaryInt != nil {
		interceptors = append([]UnaryClientInterceptor{cc.dopts.unaryInt}, interceptors...)
	}
	var chainedInt UnaryClientInterceptor
	if len(interceptors) == 0 {
		chainedInt = nil
	} else if len(interceptors) == 1 {
		chainedInt = interceptors[0]
	} else {
		chainedInt = func(ctx context.Context, method string, req, reply interface{}, cc *ClientConn, invoker UnaryInvoker, opts ...CallOption) error {
			return interceptors[0](ctx, method, req, reply, cc, getChainUnaryInvoker(interceptors, 0, invoker), opts...)
		}
	}
	cc.dopts.unaryInt = chainedInt
}

// getChainUnaryInvoker recursively generate the chained unary invoker.
func getChainUnaryInvoker(interceptors []UnaryClientInterceptor, curr int, finalInvoker UnaryInvoker) UnaryInvoker {
	if curr == len(interceptors)-1 {
		return finalInvoker
	}
	return func(ctx context.Context, method string, req, reply interface{}, cc *ClientConn, opts ...CallOption) error {
		return interceptors[curr+1](ctx, method, req, reply, cc, getChainUnaryInvoker(interceptors, curr+1, finalInvoker), opts...)
	}
}

// chainStreamClientInterceptors chains all stream client interceptors into one.
func chainStreamClientInterceptors(cc *ClientConn) {
	interceptors := cc.dopts.chainStreamInts
	// Prepend dopts.streamInt to the chaining interceptors if it exists, since streamInt will
	// be executed before any other chained interceptors.
	if cc.dopts.streamInt != nil {
		interceptors = append([]StreamClientInterceptor{cc.dopts.streamInt}, interceptors...)
	}
	var chainedInt StreamClientInterceptor
	if len(interceptors) == 0 {
		chainedInt = nil
	} else if len(interceptors) == 1 {
		chainedInt = interceptors[0]
	} else {
		chainedInt = func(ctx context.Context, desc *StreamDesc, cc *ClientConn, method string, streamer Streamer, opts ...CallOption) (ClientStream, error) {
			return interceptors[0](ctx, desc, cc, method, getChainStreamer(interceptors, 0, streamer), opts...)
		}
	}
	cc.dopts.streamInt = chainedInt
}

// getChainStreamer recursively generate the chained client stream constructor.
func getChainStreamer(interceptors []StreamClientInterceptor, curr int, finalStreamer Streamer) Streamer {
	if curr == len(interceptors)-1 {
		return finalStreamer
	}
	return func(ctx context.Context, desc *StreamDesc, cc *ClientConn, method string, opts ...CallOption) (ClientStream, error) {
		return interceptors[curr+1](ctx, desc, cc, method, getChainStreamer(interceptors, curr+1, finalStreamer), opts...)
	}
}

// connectivityStateManager keeps the connectivity.State of ClientConn.
// This struct will eventually be exported so the balancers can access it.
type connectivityStateManager struct {
	mu         sync.Mutex
	state      connectivity.State // int 代表几种状态
	notifyChan chan struct{} // 状态发生变化后，notifyChan会close
	channelzID int64
}

// updateState updates the connectivity.State of ClientConn.
// If there's a change it notifies goroutines waiting on state change to
// happen.
func (csm *connectivityStateManager) updateState(state connectivity.State) {
	// 状态更新是线程安全的
	csm.mu.Lock()
	defer csm.mu.Unlock()
	if csm.state == connectivity.Shutdown {
		return
	}
	if csm.state == state {
		return
	}
	csm.state = state
	// 日志
	channelz.Infof(csm.channelzID, "Channel Connectivity change to %v", state)
	if csm.notifyChan != nil {
		// There are other goroutines waiting on this channel.
		close(csm.notifyChan)
		csm.notifyChan = nil
	}
}

func (csm *connectivityStateManager) getState() connectivity.State {
	csm.mu.Lock()
	defer csm.mu.Unlock()
	return csm.state
}

// 每次状态发生变化后，notifyChan会close
// 调用重新这个方法又会生成一个新的channel
// 跟 context.Done 风格一毛一样，管道从来不传输东西，只用close
func (csm *connectivityStateManager) getNotifyChan() <-chan struct{} {
	csm.mu.Lock()
	defer csm.mu.Unlock()
	if csm.notifyChan == nil {
		csm.notifyChan = make(chan struct{})
	}
	return csm.notifyChan
}

// ClientConnInterface defines the functions clients need to perform unary and
// streaming RPCs.  It is implemented by *ClientConn, and is only intended to
// be referenced by generated code.
type ClientConnInterface interface {
	// Invoke performs a unary RPC and returns after the response is received
	// into reply.
	Invoke(ctx context.Context, method string, args interface{}, reply interface{}, opts ...CallOption) error
	// NewStream begins a streaming RPC.
	NewStream(ctx context.Context, desc *StreamDesc, method string, opts ...CallOption) (ClientStream, error)
}

// Assert *ClientConn implements ClientConnInterface.
var _ ClientConnInterface = (*ClientConn)(nil)

// ClientConn represents a virtual connection to a conceptual endpoint, to
// perform RPCs.
//
// A ClientConn is free to have zero or more actual connections to the endpoint
// based on configuration, load, etc. It is also free to determine which actual
// endpoints to use and may change it every RPC, permitting client-side load
// balancing.
//
// A ClientConn encapsulates a range of functionality including name
// resolution, TCP connection establishment (with retries and backoff) and TLS
// handshakes. It also handles errors on established connections by
// re-resolving the name and reconnecting.
type ClientConn struct {
	ctx    context.Context // ctx.bachground 产物
	cancel context.CancelFunc // ctx.bachground 产物

	target       string // localhost:50051 or "10.1.1.2:33,10.1.1.2:32"
	parsedTarget resolver.Target
	authority    string
	dopts        dialOptions
	csMgr        *connectivityStateManager

	balancerBuildOpts balancer.BuildOptions
	blockingpicker    *pickerWrapper

	mu              sync.RWMutex
	resolverWrapper *ccResolverWrapper
	sc              *ServiceConfig
	conns           map[*addrConn]struct{}
	// Keepalive parameter can be updated if a GoAway is received.
	mkp             keepalive.ClientParameters
	curBalancerName string
	balancerWrapper *ccBalancerWrapper
	retryThrottler  atomic.Value

	firstResolveEvent *grpcsync.Event // 用来判断是否至少一次Resolve

	channelzID int64 // channelz unique identification number
	czData     *channelzData
}

// WaitForStateChange waits until the connectivity.State of ClientConn changes from sourceState or
// ctx expires. A true value is returned in former case and false in latter.
// This is an EXPERIMENTAL API.
func (cc *ClientConn) WaitForStateChange(ctx context.Context, sourceState connectivity.State) bool {
	ch := cc.csMgr.getNotifyChan()
	// 说明已经产生了变化，退出堵塞
	if cc.csMgr.getState() != sourceState {
		return true
	}
	select {
	case <-ctx.Done():
		return false
	case <-ch:
		return true
	}
}

// GetState returns the connectivity.State of ClientConn.
// This is an EXPERIMENTAL API.
func (cc *ClientConn) GetState() connectivity.State {
	return cc.csMgr.getState()
}

func (cc *ClientConn) scWatcher() {
	for {
		select {
		case sc, ok := <-cc.dopts.scChan:
			if !ok {
				return
			}
			cc.mu.Lock()
			// TODO: load balance policy runtime change is ignored.
			// We may revisit this decision in the future.
			cc.sc = &sc
			cc.mu.Unlock()
		case <-cc.ctx.Done():
			return
		}
	}
}

// waitForResolvedAddrs blocks until the resolver has provided addresses or the
// context expires.  Returns nil unless the context expires first; otherwise
// returns a status error based on the context.
func (cc *ClientConn) waitForResolvedAddrs(ctx context.Context) error {
	// This is on the RPC path, so we use a fast path to avoid the
	// more-expensive "select" below after the resolver has returned once.
	if cc.firstResolveEvent.HasFired() { // 这个 dial 返回之前，这个肯定已经完成了
		return nil
	}
	select {
	case <-cc.firstResolveEvent.Done():
		return nil
	case <-ctx.Done():
		return status.FromContextError(ctx.Err()).Err()
	case <-cc.ctx.Done():
		return ErrClientConnClosing
	}
}

var emptyServiceConfig *ServiceConfig

func init() {
	// ServiceConfig.rawJSONString = {}
	cfg := parseServiceConfig("{}")
	if cfg.Err != nil {
		panic(fmt.Sprintf("impossible error parsing empty service config: %v", cfg.Err))
	}

	// ServiceConfig.rawJSONString = {}
	// ServiceConfig.Methods = 空值
	// 其他都是默认值
	emptyServiceConfig = cfg.Config.(*ServiceConfig)
}

// []resolver.Address{
//			{Addr: r.target.Endpoint},
//	}
func (cc *ClientConn) maybeApplyDefaultServiceConfig(addrs []resolver.Address) {
	if cc.sc != nil {
		cc.applyServiceConfigAndBalancer(cc.sc, addrs)
		return
	}
	if cc.dopts.defaultServiceConfig != nil {
		cc.applyServiceConfigAndBalancer(cc.dopts.defaultServiceConfig, addrs)
	} else {
		// 只走这里，看看上面的init,emptyServiceConfig在那边初始化的
		// ServiceConfig.rawJSONString = {}
		// ServiceConfig.Methods = 空值
		// 其他都是默认值
		cc.applyServiceConfigAndBalancer(emptyServiceConfig, addrs)
	}
}

// s resolver.State
// resolver.State{
//		Addresses: []resolver.Address{
//			{Addr: r.target.Endpoint}, // // localhost:50051 or "10.1.1.2:33,10.1.1.2:32"
//		},
//	}
func (cc *ClientConn) updateResolverState(s resolver.State, err error) error {
	defer cc.firstResolveEvent.Fire() // 没啥用，就是给流那边判断有没有resolve过
	cc.mu.Lock()
	// Check if the ClientConn is already closed. Some fields (e.g.
	// balancerWrapper) are set to nil when closing the ClientConn, and could
	// cause nil pointer panic if we don't have this check.
	// 第一次是一个空值,非nil
	// func (cc *ClientConn) Close() 时赋值 nil
	if cc.conns == nil {
		cc.mu.Unlock()
		return nil
	}

	// 跳过
	if err != nil {
		// May need to apply the initial service config in case the resolver
		// doesn't support service configs, or doesn't provide a service config
		// with the new addresses.
		cc.maybeApplyDefaultServiceConfig(nil)

		if cc.balancerWrapper != nil {
			cc.balancerWrapper.resolverError(err)
		}

		// No addresses are valid with err set; return early.
		cc.mu.Unlock()
		return balancer.ErrBadResolverState
	}


	var ret error
	if cc.dopts.disableServiceConfig || s.ServiceConfig == nil { // s.ServiceConfig == nil
		// resolver.State{
		//		Addresses: []resolver.Address{
		//			{Addr: r.target.Endpoint},
		//		},
		//	}
		cc.maybeApplyDefaultServiceConfig(s.Addresses)
		// TODO: do we need to apply a failing LB policy if there is no
		// default, per the error handling design?
	} else {
		if sc, ok := s.ServiceConfig.Config.(*ServiceConfig); s.ServiceConfig.Err == nil && ok {
			cc.applyServiceConfigAndBalancer(sc, s.Addresses)
		} else {
			ret = balancer.ErrBadResolverState
			if cc.balancerWrapper == nil {
				var err error
				if s.ServiceConfig.Err != nil {
					err = status.Errorf(codes.Unavailable, "error parsing service config: %v", s.ServiceConfig.Err)
				} else {
					err = status.Errorf(codes.Unavailable, "illegal service config type: %T", s.ServiceConfig.Config)
				}
				cc.blockingpicker.updatePicker(base.NewErrPicker(err))
				cc.csMgr.updateState(connectivity.TransientFailure)
				cc.mu.Unlock()
				return ret
			}
		}
	}

	var balCfg serviceconfig.LoadBalancingConfig
	if cc.dopts.balancerBuilder == nil && cc.sc != nil && cc.sc.lbConfig != nil { // cc.sc.lbConfig == nil 所以进不去
		balCfg = cc.sc.lbConfig.cfg
	}

	cbn := cc.curBalancerName // pick_first
	bw := cc.balancerWrapper // ccBalancerWrapper
	cc.mu.Unlock()
	if cbn != grpclbName { // 这里也可以跳过
		// Filter any grpclb addresses since we don't have the grpclb balancer.
		for i := 0; i < len(s.Addresses); {
			if s.Addresses[i].Type == resolver.GRPCLB {
				copy(s.Addresses[i:], s.Addresses[i+1:])
				s.Addresses = s.Addresses[:len(s.Addresses)-1]
				continue
			}
			i++
		}
	}
	uccsErr := bw.updateClientConnState(&balancer.ClientConnState{ResolverState: s, BalancerConfig: balCfg})
	if ret == nil {
		ret = uccsErr // prefer ErrBadResolver state since any other error is
		// currently meaningless to the caller.
	}
	return ret
}

// switchBalancer starts the switching from current balancer to the balancer
// with the given name.
//
// It will NOT send the current address list to the new balancer. If needed,
// caller of this function should send address list to the new balancer after
// this function returns.
//
// Caller must hold cc.mu.
func (cc *ClientConn) switchBalancer(name string) { // pick_first
	if strings.EqualFold(cc.curBalancerName, name) { // cc.curBalancerName = ""
		return
	}

	channelz.Infof(cc.channelzID, "ClientConn switching balancer to %q", name)
	if cc.dopts.balancerBuilder != nil {
		channelz.Info(cc.channelzID, "ignoring balancer switching: Balancer DialOption used instead")
		return
	}
	if cc.balancerWrapper != nil {
		cc.balancerWrapper.close()
	}
	// newPickfirstBuilder()
	builder := balancer.Get(name) //pickfirstBuilder
	// 跳过这个if
	if builder == nil {
		channelz.Warningf(cc.channelzID, "Channel switches to new LB policy %q due to fallback from invalid balancer name", PickFirstBalancerName)
		channelz.Infof(cc.channelzID, "failed to get balancer builder for: %v, using pick_first instead", name)
		builder = newPickfirstBuilder()
	} else {
		channelz.Infof(cc.channelzID, "Channel switches to new LB policy %q", name)
	}

	cc.curBalancerName = builder.Name() // first_pick
	// builder: newPickfirstBuilder() first_pick
	// 	cc.balancerBuildOpts = balancer.BuildOptions{
	//		DialCreds:        credsClone, // nil
	//		CredsBundle:      cc.dopts.copts.CredsBundle, // nil
	//		Dialer:           cc.dopts.copts.Dialer, // (&net.Dialer{}).DialContext
	//		ChannelzParentID: cc.channelzID, // 忽略 0
	//		Target:           cc.parsedTarget, // 请看 DialContext()
	//	}
	//	// type Target struct {
	//	//	Scheme    string  passthrough
	//	//	Authority string  ""
	//	//	Endpoint  string localhost:50051
	//	//}
	cc.balancerWrapper = newCCBalancerWrapper(cc, builder, cc.balancerBuildOpts)
}

func (cc *ClientConn) handleSubConnStateChange(sc balancer.SubConn, s connectivity.State, err error) {
	cc.mu.Lock()
	if cc.conns == nil { // == nil 说明连接被关闭
		cc.mu.Unlock()
		return
	}
	// TODO(bar switching) send updates to all balancer wrappers when balancer
	// gracefully switching is supported.
	cc.balancerWrapper.handleSubConnStateChange(sc, s, err)
	cc.mu.Unlock()
}

// newAddrConn creates an addrConn for addrs and adds it to cc.conns.
//
// Caller needs to make sure len(addrs) > 0.
func (cc *ClientConn) newAddrConn(addrs []resolver.Address, opts balancer.NewSubConnOptions) (*addrConn, error) {
	ac := &addrConn{
		state:        connectivity.Idle, // 初始状态
		cc:           cc,
		addrs:        addrs,
		scopts:       opts,
		dopts:        cc.dopts,
		czData:       new(channelzData),
		resetBackoff: make(chan struct{}),
	}
	// addrConn的ctx 是跟着cc走的
	ac.ctx, ac.cancel = context.WithCancel(cc.ctx)
	// Track ac in cc. This needs to be done before any getTransport(...) is called.
	cc.mu.Lock()
	if cc.conns == nil { // make(map[*addrConn]struct{}) 初始化的状态，只有关闭才会修改这个为nil
		cc.mu.Unlock()
		return nil, ErrClientConnClosing
	}
	if channelz.IsOn() {
		ac.channelzID = channelz.RegisterSubChannel(ac, cc.channelzID, "")
		channelz.AddTraceEvent(ac.channelzID, 0, &channelz.TraceEventDesc{
			Desc:     "Subchannel Created",
			Severity: channelz.CtINFO,
			Parent: &channelz.TraceEventDesc{
				Desc:     fmt.Sprintf("Subchannel(id:%d) created", ac.channelzID),
				Severity: channelz.CtINFO,
			},
		})
	}
	cc.conns[ac] = struct{}{}
	cc.mu.Unlock()
	return ac, nil
}

// removeAddrConn removes the addrConn in the subConn from clientConn.
// It also tears down the ac with the given error.
func (cc *ClientConn) removeAddrConn(ac *addrConn, err error) {
	cc.mu.Lock()
	if cc.conns == nil {
		cc.mu.Unlock()
		return
	}
	delete(cc.conns, ac)
	cc.mu.Unlock()
	ac.tearDown(err)
}

func (cc *ClientConn) channelzMetric() *channelz.ChannelInternalMetric {
	return &channelz.ChannelInternalMetric{
		State:                    cc.GetState(),
		Target:                   cc.target,
		CallsStarted:             atomic.LoadInt64(&cc.czData.callsStarted),
		CallsSucceeded:           atomic.LoadInt64(&cc.czData.callsSucceeded),
		CallsFailed:              atomic.LoadInt64(&cc.czData.callsFailed),
		LastCallStartedTimestamp: time.Unix(0, atomic.LoadInt64(&cc.czData.lastCallStartedTime)),
	}
}

// Target returns the target string of the ClientConn.
// This is an EXPERIMENTAL API.
func (cc *ClientConn) Target() string {
	return cc.target
}

func (cc *ClientConn) incrCallsStarted() {
	atomic.AddInt64(&cc.czData.callsStarted, 1)
	atomic.StoreInt64(&cc.czData.lastCallStartedTime, time.Now().UnixNano())
}

func (cc *ClientConn) incrCallsSucceeded() {
	atomic.AddInt64(&cc.czData.callsSucceeded, 1)
}

func (cc *ClientConn) incrCallsFailed() {
	atomic.AddInt64(&cc.czData.callsFailed, 1)
}

// connect starts creating a transport.
// It does nothing if the ac is not IDLE.
// TODO(bar) Move this to the addrConn section.
func (ac *addrConn) connect() error {
	ac.mu.Lock()
	if ac.state == connectivity.Shutdown {
		ac.mu.Unlock()
		return errConnClosing
	}
	if ac.state != connectivity.Idle {
		ac.mu.Unlock()
		return nil
	}
	// Update connectivity state within the lock to prevent subsequent or
	// concurrent calls from resetting the transport more than once.
	ac.updateConnectivityState(connectivity.Connecting, nil)
	ac.mu.Unlock()

	// Start a goroutine connecting to the server asynchronously.
	// TODO 这个才是真正连接的地方
	go ac.resetTransport()
	return nil
}

// tryUpdateAddrs tries to update ac.addrs with the new addresses list.
//
// If ac is Connecting, it returns false. The caller should tear down the ac and
// create a new one. Note that the backoff will be reset when this happens.
//
// If ac is TransientFailure, it updates ac.addrs and returns true. The updated
// addresses will be picked up by retry in the next iteration after backoff.
//
// If ac is Shutdown or Idle, it updates ac.addrs and returns true.
//
// If ac is Ready, it checks whether current connected address of ac is in the
// new addrs list.
//  - If true, it updates ac.addrs and returns true. The ac will keep using
//    the existing connection.
//  - If false, it does nothing and returns false.
func (ac *addrConn) tryUpdateAddrs(addrs []resolver.Address) bool {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	channelz.Infof(ac.channelzID, "addrConn: tryUpdateAddrs curAddr: %v, addrs: %v", ac.curAddr, addrs)
	if ac.state == connectivity.Shutdown ||
		ac.state == connectivity.TransientFailure ||
		ac.state == connectivity.Idle {
		ac.addrs = addrs
		return true
	}

	if ac.state == connectivity.Connecting {
		return false
	}

	// ac.state is Ready, try to find the connected address.
	var curAddrFound bool
	for _, a := range addrs {
		if reflect.DeepEqual(ac.curAddr, a) {
			curAddrFound = true
			break
		}
	}
	channelz.Infof(ac.channelzID, "addrConn: tryUpdateAddrs curAddrFound: %v", curAddrFound)
	if curAddrFound {
		ac.addrs = addrs
	}

	return curAddrFound
}

// GetMethodConfig gets the method config of the input method.
// If there's an exact match for input method (i.e. /service/method), we return
// the corresponding MethodConfig.
// If there isn't an exact match for the input method, we look for the default config
// under the service (i.e /service/). If there is a default MethodConfig for
// the service, we return it.
// Otherwise, we return an empty MethodConfig.
func (cc *ClientConn) GetMethodConfig(method string) MethodConfig {
	// TODO: Avoid the locking here.
	cc.mu.RLock()
	defer cc.mu.RUnlock()
	if cc.sc == nil {
		return MethodConfig{}
	}
	m, ok := cc.sc.Methods[method]
	if !ok {
		i := strings.LastIndex(method, "/")
		m = cc.sc.Methods[method[:i+1]]
	}
	return m
}

func (cc *ClientConn) healthCheckConfig() *healthCheckConfig {
	cc.mu.RLock()
	defer cc.mu.RUnlock()
	if cc.sc == nil {
		return nil
	}
	return cc.sc.healthCheckConfig
}

func (cc *ClientConn) getTransport(ctx context.Context, failfast bool, method string) (transport.ClientTransport, func(balancer.DoneInfo), error) {
	t, done, err := cc.blockingpicker.pick(ctx, failfast, balancer.PickInfo{
		Ctx:            ctx,
		FullMethodName: method,
	})
	if err != nil {
		return nil, nil, toRPCErr(err)
	}
	return t, done, nil
}

// ServiceConfig.rawJSONString = {}
// ServiceConfig.Methods = 空值
// 其他都是默认值
//
// s resolver.State
// resolver.State{
//		Addresses: []resolver.Address{
//			{Addr: r.target.Endpoint},
//		},
//	}
func (cc *ClientConn) applyServiceConfigAndBalancer(sc *ServiceConfig, addrs []resolver.Address) {
	if sc == nil {
		// should never reach here.
		return
	}
	// sc 这块还没有深挖，感觉里面又是一个世界
	cc.sc = sc

	if cc.sc.retryThrottling != nil { // retryThrottling = nil
		newThrottler := &retryThrottler{
			tokens: cc.sc.retryThrottling.MaxTokens,
			max:    cc.sc.retryThrottling.MaxTokens,
			thresh: cc.sc.retryThrottling.MaxTokens / 2,
			ratio:  cc.sc.retryThrottling.TokenRatio,
		}
		cc.retryThrottler.Store(newThrottler)
	} else {
		cc.retryThrottler.Store((*retryThrottler)(nil)) // 又来，有没有很熟悉
	}

	if cc.dopts.balancerBuilder == nil { // 进来
		// Only look at balancer types and switch balancer if balancer dial
		// option is not set.
		var newBalancerName string
		if cc.sc != nil && cc.sc.lbConfig != nil {
			newBalancerName = cc.sc.lbConfig.name
		} else { // 走这里
			var isGRPCLB bool
			for _, a := range addrs {
				if a.Type == resolver.GRPCLB { // false
					isGRPCLB = true
					break
				}
			}
			if isGRPCLB {
				newBalancerName = grpclbName
			} else if cc.sc != nil && cc.sc.LB != nil {
				newBalancerName = *cc.sc.LB
			} else { // 还是走了这里
				newBalancerName = PickFirstBalancerName // pick_first
			}
		} // 到这里也就给 balancerBuilder 选了了一个名字 pick_first
		cc.switchBalancer(newBalancerName)
	} else if cc.balancerWrapper == nil {
		// Balancer dial option was set, and this is the first time handling
		// resolved addresses. Build a balancer with dopts.balancerBuilder.
		cc.curBalancerName = cc.dopts.balancerBuilder.Name()
		cc.balancerWrapper = newCCBalancerWrapper(cc, cc.dopts.balancerBuilder, cc.balancerBuildOpts)
	}
}

func (cc *ClientConn) resolveNow(o resolver.ResolveNowOptions) {
	cc.mu.RLock()
	r := cc.resolverWrapper // ccResolverWrapper
	cc.mu.RUnlock()
	if r == nil {
		return
	}
	go r.resolveNow(o)
}

// ResetConnectBackoff wakes up all subchannels in transient failure and causes
// them to attempt another connection immediately.  It also resets the backoff
// times used for subsequent attempts regardless of the current state.
//
// In general, this function should not be used.  Typical service or network
// outages result in a reasonable client reconnection strategy by default.
// However, if a previously unavailable network becomes available, this may be
// used to trigger an immediate reconnect.
//
// This API is EXPERIMENTAL.
func (cc *ClientConn) ResetConnectBackoff() {
	cc.mu.Lock()
	conns := cc.conns
	cc.mu.Unlock()
	for ac := range conns {
		ac.resetConnectBackoff()
	}
}

// Close tears down the ClientConn and all underlying connections.
func (cc *ClientConn) Close() error {
	defer cc.cancel()

	cc.mu.Lock()
	if cc.conns == nil {
		cc.mu.Unlock()
		return ErrClientConnClosing
	}
	conns := cc.conns
	cc.conns = nil
	// 更新连接状态
	cc.csMgr.updateState(connectivity.Shutdown)

	rWrapper := cc.resolverWrapper
	cc.resolverWrapper = nil
	bWrapper := cc.balancerWrapper
	cc.balancerWrapper = nil
	cc.mu.Unlock()

	cc.blockingpicker.close()

	if rWrapper != nil {
		rWrapper.close()
	}
	if bWrapper != nil {
		bWrapper.close()
	}

	for ac := range conns {
		// 这个 err 很关键，ccb.done errConnDrain 会优雅关闭（也不一定要深挖）
		// 当然这个不优雅关闭也是有原因的，Close() 就不会阻塞了
		ac.tearDown(ErrClientConnClosing)
	}
	if channelz.IsOn() {
		ted := &channelz.TraceEventDesc{
			Desc:     "Channel Deleted",
			Severity: channelz.CtINFO,
		}
		if cc.dopts.channelzParentID != 0 {
			ted.Parent = &channelz.TraceEventDesc{
				Desc:     fmt.Sprintf("Nested channel(id:%d) deleted", cc.channelzID),
				Severity: channelz.CtINFO,
			}
		}
		channelz.AddTraceEvent(cc.channelzID, 0, ted)
		// TraceEvent needs to be called before RemoveEntry, as TraceEvent may add trace reference to
		// the entity being deleted, and thus prevent it from being deleted right away.
		channelz.RemoveEntry(cc.channelzID)
	}
	return nil
}

// addrConn is a network connection to a given address.
type addrConn struct {
	ctx    context.Context // ac.ctx, ac.cancel = context.WithCancel(cc.ctx)
	cancel context.CancelFunc

	cc     *ClientConn
	dopts  dialOptions
	acbw   balancer.SubConn
	scopts balancer.NewSubConnOptions

	// transport is set when there's a viable transport (note: ac state may not be READY as LB channel
	// health checking may require server to report healthy to set ac to READY), and is reset
	// to nil when the current transport should no longer be used to create a stream (e.g. after GoAway
	// is received, transport is closed, ac has been torn down).
	transport transport.ClientTransport // The current transport.

	mu      sync.Mutex
	curAddr resolver.Address   // The current address.
	addrs   []resolver.Address // All addresses that the resolver resolved to.

	// Use updateConnectivityState for updating addrConn's connectivity state.
	state connectivity.State

	backoffIdx   int // Needs to be stateful for resetConnectBackoff.
	resetBackoff chan struct{}

	channelzID int64 // channelz unique identification number.
	czData     *channelzData
}

// Note: this requires a lock on ac.mu.
func (ac *addrConn) updateConnectivityState(s connectivity.State, lastErr error) {
	if ac.state == s {
		return
	}
	ac.state = s
	channelz.Infof(ac.channelzID, "Subchannel Connectivity change to %v", s)
	ac.cc.handleSubConnStateChange(ac.acbw, s, lastErr)
}

// adjustParams updates parameters used to create transports upon
// receiving a GoAway.
func (ac *addrConn) adjustParams(r transport.GoAwayReason) {
	switch r {
	case transport.GoAwayTooManyPings:
		v := 2 * ac.dopts.copts.KeepaliveParams.Time
		ac.cc.mu.Lock()
		if v > ac.cc.mkp.Time {
			ac.cc.mkp.Time = v
		}
		ac.cc.mu.Unlock()
	}
}

func (ac *addrConn) resetTransport() {
	for i := 0; ; i++ {
		if i > 0 { // 跳过，不知道干嘛的反正当前空的
			ac.cc.resolveNow(resolver.ResolveNowOptions{})
		}

		ac.mu.Lock()
		if ac.state == connectivity.Shutdown { // connectivity.Shutdown 这个协程就会退出了，避免泄露
			ac.mu.Unlock()
			return
		}

		addrs := ac.addrs
		backoffFor := ac.dopts.bs.Backoff(ac.backoffIdx)
		// This will be the duration that dial gets to finish.
		dialDuration := minConnectTimeout // 20秒
		if ac.dopts.minConnectTimeout != nil {
			dialDuration = ac.dopts.minConnectTimeout()
		}

		if dialDuration < backoffFor { // 这个参数不能比重新算法得出的时间短
			// Give dial more time as we keep failing to connect.
			dialDuration = backoffFor
		}
		// We can potentially spend all the time trying the first address, and
		// if the server accepts the connection and then hangs, the following
		// addresses will never be tried.
		//
		// The spec doesn't mention what should be done for multiple addresses.
		// https://github.com/grpc/grpc/blob/master/doc/connection-backoff.md#proposed-backoff-algorithm
		connectDeadline := time.Now().Add(dialDuration)

		ac.updateConnectivityState(connectivity.Connecting, nil)
		ac.transport = nil
		ac.mu.Unlock()

		newTr, addr, reconnect, err := ac.tryAllAddrs(addrs, connectDeadline)
		if err != nil {
			// After exhausting all addresses, the addrConn enters
			// TRANSIENT_FAILURE.
			ac.mu.Lock()
			if ac.state == connectivity.Shutdown {
				ac.mu.Unlock()
				return
			}
			ac.updateConnectivityState(connectivity.TransientFailure, err)

			// Backoff.
			b := ac.resetBackoff // {chan struct{}}
			ac.mu.Unlock()

			timer := time.NewTimer(backoffFor) // 就是等待一段时间
			select {
			case <-timer.C:
				ac.mu.Lock()
				ac.backoffIdx++
				ac.mu.Unlock()
			case <-b: // 坑爹玩意，test里面会触发，其他没有地方可以触发
				timer.Stop()
			case <-ac.ctx.Done(): // addrConn的ctx 是跟着cc走的
				timer.Stop()
				return
			}
			continue
		}

		ac.mu.Lock()
		if ac.state == connectivity.Shutdown {
			ac.mu.Unlock()
			newTr.Close()
			return
		}
		ac.curAddr = addr
		ac.transport = newTr
		ac.backoffIdx = 0

		// 这个忽略，我们现在没有健康检查
		// 这里面状态变成 Ready
		// ac.updateConnectivityState(connectivity.Ready, nil)
		hctx, hcancel := context.WithCancel(ac.ctx)
		ac.startHealthCheck(hctx)
		ac.mu.Unlock()

		// Block until the created transport is down. And when this happens,
		// we restart from the top of the addr list.
		// 放到 onGoway 里面去触发这个 事件了
		// 怎么触发？就先简单认为 Transport 里面发生错误，这边就会触发
		<-reconnect.Done()
		hcancel() // 健康检查停止
		// restart connecting - the top of the loop will set state to
		// CONNECTING.  This is against the current connectivity semantics doc,
		// however it allows for graceful behavior for RPCs not yet dispatched
		// - unfortunate timing would otherwise lead to the RPC failing even
		// though the TRANSIENT_FAILURE state (called for by the doc) would be
		// instantaneous.
		//
		// Ideally we should transition to Idle here and block until there is
		// RPC activity that leads to the balancer requesting a reconnect of
		// the associated SubConn.
	}
}

// tryAllAddrs tries to creates a connection to the addresses, and stop when at the
// first successful one. It returns the transport, the address and a Event in
// the successful case. The Event fires when the returned transport disconnects.
func (ac *addrConn) tryAllAddrs(addrs []resolver.Address, connectDeadline time.Time) (transport.ClientTransport, resolver.Address, *grpcsync.Event, error) {
	var firstConnErr error
	for _, addr := range addrs {
		ac.mu.Lock()
		if ac.state == connectivity.Shutdown { // 避免这个时候 client已经被close
			ac.mu.Unlock()
			return nil, resolver.Address{}, nil, errConnClosing
		}

		ac.cc.mu.RLock()
		ac.dopts.copts.KeepaliveParams = ac.cc.mkp
		// 蜜汁操作，反向搞再来一波 ac.dopts = ac.cc.dopts ac.cc.mkp = ac.cc.dopts.copts.KeepaliveParams
		// 感觉这个赋值多余了
		ac.cc.mu.RUnlock()

		copts := ac.dopts.copts
		// nil and HealthCheckEnabled:false TODO HealthCheckEnabled 看看干啥用的
		if ac.scopts.CredsBundle != nil { // 忽略
			copts.CredsBundle = ac.scopts.CredsBundle
		}
		ac.mu.Unlock()

		channelz.Infof(ac.channelzID, "Subchannel picks a new address %q to connect", addr.Addr)

		// 如果返回错误
		// newTr = nil
		// reconnect = nil
		// connection error: desc = "transport: Error while dialing dial tcp 172.16.34.140:50051: i/o timeout"
		newTr, reconnect, err := ac.createTransport(addr, copts, connectDeadline)
		if err == nil {
			return newTr, addr, reconnect, nil
		}
		if firstConnErr == nil {
			firstConnErr = err
		}
		ac.cc.blockingpicker.updateConnectionError(err)
	}

	// Couldn't connect to any address.
	return nil, resolver.Address{}, nil, firstConnErr
}

// createTransport creates a connection to addr. It returns the transport and a
// Event in the successful case. The Event fires when the returned transport
// disconnects.
func (ac *addrConn) createTransport(addr resolver.Address, copts transport.ConnectOptions, connectDeadline time.Time) (transport.ClientTransport, *grpcsync.Event, error) {
	prefaceReceived := make(chan struct{})
	onCloseCalled := make(chan struct{})
	reconnect := grpcsync.NewEvent()

	authority := ac.cc.authority // 172.16.34.140:50051
	// addr.ServerName takes precedent over ClientConn authority, if present.
	if addr.ServerName != "" { // 跳过
		authority = addr.ServerName
	}

	target := transport.TargetInfo{
		Addr:      addr.Addr, // 172.16.34.140:50051
		Metadata:  addr.Metadata, // nil
		Authority: authority, // 172.16.34.140:50051
	}

	once := sync.Once{}
	onGoAway := func(r transport.GoAwayReason) { // 后面给http2client了
		ac.mu.Lock()
		ac.adjustParams(r)
		once.Do(func() {
			if ac.state == connectivity.Ready {
				// Prevent this SubConn from being used for new RPCs by setting its
				// state to Connecting.
				//
				// TODO: this should be Idle when grpc-go properly supports it.
				ac.updateConnectivityState(connectivity.Connecting, nil)
			}
		})
		ac.mu.Unlock()
		reconnect.Fire() // 这边调用之后触发 resetTransport
	}

	onClose := func() { // 后面给http2client了
		ac.mu.Lock()
		once.Do(func() {
			if ac.state == connectivity.Ready {
				// Prevent this SubConn from being used for new RPCs by setting its
				// state to Connecting.
				//
				// TODO: this should be Idle when grpc-go properly supports it.
				ac.updateConnectivityState(connectivity.Connecting, nil)
			}
		})
		ac.mu.Unlock()
		close(onCloseCalled)
		reconnect.Fire()
	}

	onPrefaceReceipt := func() { // 后面给http2client了
		close(prefaceReceived)
	}

	connectCtx, cancel := context.WithDeadline(ac.ctx, connectDeadline)
	defer cancel()
	if channelz.IsOn() {
		copts.ChannelzParentID = ac.channelzID
	}

	newTr, err := transport.NewClientTransport(connectCtx, ac.cc.ctx, target, copts, onPrefaceReceipt, onGoAway, onClose)
	if err != nil {
		// newTr is either nil, or closed.
		channelz.Warningf(ac.channelzID, "grpc: addrConn.createTransport failed to connect to %v. Err: %v. Reconnecting...", addr, err)
		return nil, nil, err
	}

	select {
	case <-time.After(time.Until(connectDeadline)):
		// We didn't get the preface in time.
		newTr.Close()
		channelz.Warningf(ac.channelzID, "grpc: addrConn.createTransport failed to connect to %v: didn't receive server preface in time. Reconnecting...", addr)
		return nil, nil, errors.New("timed out waiting for server handshake")
	case <-prefaceReceived: // reader 收到第一帧正确后，将会触发这个。
		// We got the preface - huzzah! things are good.
	case <-onCloseCalled: // 触发条件要留意下
		// The transport has already closed - noop.
		return nil, nil, errors.New("connection closed")
		// TODO(deklerk) this should bail on ac.ctx.Done(). Add a test and fix.
	}
	return newTr, reconnect, nil
}

// startHealthCheck starts the health checking stream (RPC) to watch the health
// stats of this connection if health checking is requested and configured.
//
// LB channel health checking is enabled when all requirements below are met:
// 1. it is not disabled by the user with the WithDisableHealthCheck DialOption
// 2. internal.HealthCheckFunc is set by importing the grpc/healthcheck package
// 3. a service config with non-empty healthCheckConfig field is provided
// 4. the load balancer requests it
//
// It sets addrConn to READY if the health checking stream is not started.
//
// Caller must hold ac.mu.
func (ac *addrConn) startHealthCheck(ctx context.Context) {
	var healthcheckManagingState bool
	defer func() {
		if !healthcheckManagingState {
			ac.updateConnectivityState(connectivity.Ready, nil)
		}
	}()

	if ac.cc.dopts.disableHealthCheck { // false
		return
	}
	healthCheckConfig := ac.cc.healthCheckConfig()
	if healthCheckConfig == nil { // nil
		return
	}
	if !ac.scopts.HealthCheckEnabled { // false
		return
	}
	healthCheckFunc := ac.cc.dopts.healthCheckFunc
	if healthCheckFunc == nil {
		// The health package is not imported to set health check function.
		//
		// TODO: add a link to the health check doc in the error message.
		channelz.Error(ac.channelzID, "Health check is requested but health check function is not set.")
		return
	}

	healthcheckManagingState = true

	// Set up the health check helper functions.
	currentTr := ac.transport
	newStream := func(method string) (interface{}, error) {
		ac.mu.Lock()
		if ac.transport != currentTr {
			ac.mu.Unlock()
			return nil, status.Error(codes.Canceled, "the provided transport is no longer valid to use")
		}
		ac.mu.Unlock()
		return newNonRetryClientStream(ctx, &StreamDesc{ServerStreams: true}, method, currentTr, ac)
	}
	setConnectivityState := func(s connectivity.State, lastErr error) {
		ac.mu.Lock()
		defer ac.mu.Unlock()
		if ac.transport != currentTr {
			return
		}
		ac.updateConnectivityState(s, lastErr)
	}
	// Start the health checking stream.
	go func() { // TODO 看看健康检查是不是靠谱
		err := ac.cc.dopts.healthCheckFunc(ctx, newStream, setConnectivityState, healthCheckConfig.ServiceName)
		if err != nil {
			if status.Code(err) == codes.Unimplemented {
				channelz.Error(ac.channelzID, "Subchannel health check is unimplemented at server side, thus health check is disabled")
			} else {
				channelz.Errorf(ac.channelzID, "HealthCheckFunc exits with unexpected error %v", err)
			}
		}
	}()
}

func (ac *addrConn) resetConnectBackoff() {
	ac.mu.Lock()
	close(ac.resetBackoff)
	ac.backoffIdx = 0
	ac.resetBackoff = make(chan struct{})
	ac.mu.Unlock()
}

// getReadyTransport returns the transport if ac's state is READY.
// Otherwise it returns nil, false.
// If ac's state is IDLE, it will trigger ac to connect.
func (ac *addrConn) getReadyTransport() (transport.ClientTransport, bool) {
	ac.mu.Lock()
	if ac.state == connectivity.Ready && ac.transport != nil {
		t := ac.transport
		ac.mu.Unlock()
		return t, true
	}
	var idle bool
	if ac.state == connectivity.Idle { // 目前看来，应该不可能会出现这种情况
		idle = true
	}
	ac.mu.Unlock()
	// Trigger idle ac to connect.
	if idle {
		ac.connect() // 这里面的协程不会反复被触发，没有问题
	}
	return nil, false
}

// tearDown starts to tear down the addrConn.
// TODO(zhaoq): Make this synchronous to avoid unbounded memory consumption in
// some edge cases (e.g., the caller opens and closes many addrConn's in a
// tight loop.
// tearDown doesn't remove ac from ac.cc.conns.
func (ac *addrConn) tearDown(err error) {
	ac.mu.Lock()
	if ac.state == connectivity.Shutdown {
		ac.mu.Unlock()
		return
	}
	curTr := ac.transport
	ac.transport = nil
	// We have to set the state to Shutdown before anything else to prevent races
	// between setting the state and logic that waits on context cancellation / etc.
	ac.updateConnectivityState(connectivity.Shutdown, nil)
	ac.cancel() // 看看这里是谁给他的，他的取消对其他地方又有什么作用
	ac.curAddr = resolver.Address{}
	if err == errConnDrain && curTr != nil {
		// GracefulClose(...) may be executed multiple times when
		// i) receiving multiple GoAway frames from the server; or
		// ii) there are concurrent name resolver/Balancer triggered
		// address removal and GoAway.
		// We have to unlock and re-lock here because GracefulClose => Close => onClose, which requires locking ac.mu.
		ac.mu.Unlock()
		curTr.GracefulClose()
		ac.mu.Lock()
	}
	if channelz.IsOn() {
		channelz.AddTraceEvent(ac.channelzID, 0, &channelz.TraceEventDesc{
			Desc:     "Subchannel Deleted",
			Severity: channelz.CtINFO,
			Parent: &channelz.TraceEventDesc{
				Desc:     fmt.Sprintf("Subchanel(id:%d) deleted", ac.channelzID),
				Severity: channelz.CtINFO,
			},
		})
		// TraceEvent needs to be called before RemoveEntry, as TraceEvent may add trace reference to
		// the entity being deleted, and thus prevent it from being deleted right away.
		channelz.RemoveEntry(ac.channelzID)
	}
	ac.mu.Unlock()
}

func (ac *addrConn) getState() connectivity.State {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	return ac.state
}

func (ac *addrConn) ChannelzMetric() *channelz.ChannelInternalMetric {
	ac.mu.Lock()
	addr := ac.curAddr.Addr
	ac.mu.Unlock()
	return &channelz.ChannelInternalMetric{
		State:                    ac.getState(),
		Target:                   addr,
		CallsStarted:             atomic.LoadInt64(&ac.czData.callsStarted),
		CallsSucceeded:           atomic.LoadInt64(&ac.czData.callsSucceeded),
		CallsFailed:              atomic.LoadInt64(&ac.czData.callsFailed),
		LastCallStartedTimestamp: time.Unix(0, atomic.LoadInt64(&ac.czData.lastCallStartedTime)),
	}
}

func (ac *addrConn) incrCallsStarted() {
	atomic.AddInt64(&ac.czData.callsStarted, 1)
	atomic.StoreInt64(&ac.czData.lastCallStartedTime, time.Now().UnixNano())
}

func (ac *addrConn) incrCallsSucceeded() {
	atomic.AddInt64(&ac.czData.callsSucceeded, 1)
}

func (ac *addrConn) incrCallsFailed() {
	atomic.AddInt64(&ac.czData.callsFailed, 1)
}

type retryThrottler struct {
	max    float64
	thresh float64
	ratio  float64

	mu     sync.Mutex
	tokens float64 // TODO(dfawley): replace with atomic and remove lock.
}

// throttle subtracts a retry token from the pool and returns whether a retry
// should be throttled (disallowed) based upon the retry throttling policy in
// the service config.
func (rt *retryThrottler) throttle() bool {
	if rt == nil {
		return false
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.tokens--
	if rt.tokens < 0 {
		rt.tokens = 0
	}
	return rt.tokens <= rt.thresh
}

func (rt *retryThrottler) successfulRPC() {
	if rt == nil {
		return
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.tokens += rt.ratio
	if rt.tokens > rt.max {
		rt.tokens = rt.max
	}
}

type channelzChannel struct {
	cc *ClientConn
}

func (c *channelzChannel) ChannelzMetric() *channelz.ChannelInternalMetric {
	return c.cc.channelzMetric()
}

// ErrClientConnTimeout indicates that the ClientConn cannot establish the
// underlying connections within the specified timeout.
//
// Deprecated: This error is never returned by grpc and should not be
// referenced by users.
var ErrClientConnTimeout = errors.New("grpc: timed out when dialing")

func (cc *ClientConn) getResolver(scheme string) resolver.Builder {
	for _, rb := range cc.dopts.resolvers {
		if scheme == rb.Scheme() {
			return rb
		}
	}
	return resolver.Get(scheme)
}
