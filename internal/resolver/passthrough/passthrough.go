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

// Package passthrough implements a pass-through resolver. It sends the target
// name without scheme back to gRPC as resolved address.
package passthrough

import "google.golang.org/grpc/resolver"

const scheme = "passthrough"

type passthroughBuilder struct{}

func (*passthroughBuilder) Build(target resolver.Target, cc resolver.ClientConn, opts resolver.BuildOptions) (resolver.Resolver, error) {
	// type Target struct {
	//	Scheme    string  passthrough
	//	Authority string  ""
	//	Endpoint  string localhost:50051
	//}
	r := &passthroughResolver{
		target: target,
		cc:     cc, // 这个不是clientConn,而是resolver.ClientConn（就是那个ccr）
	}
	// cc.curState = s
	// cc.cc.firstResolveEvent.Fire()
	// cc.cc.sc = emptyServiceConfig
	// cc.cc.curBalancerName =  first_pick
	// cc.cc.balancerWrapper = ccBalancerWrapper
	r.start()
	return r, nil
}

func (*passthroughBuilder) Scheme() string {
	return scheme
}

type passthroughResolver struct {
	target resolver.Target
	cc     resolver.ClientConn
}

func (r *passthroughResolver) start() {
	// cc 是 resolver.ClientConn 不是 clientConn
	// r.target.Endpoint 可以是 localhost:50051 也可以 "10.1.1.2:33,10.1.1.2:32"
	// resolver.Address 也可以是多个
	r.cc.UpdateState(resolver.State{
		Addresses: []resolver.Address{
			{Addr: r.target.Endpoint},
		},
	})
	// 继续切换频道回到 ccResolverWrapper.UpdateState
	// ccResolverWrapper 实现了 resolver.ClientConn 接口
}

func (*passthroughResolver) ResolveNow(o resolver.ResolveNowOptions) {}

func (*passthroughResolver) Close() {}

func init() {
	resolver.Register(&passthroughBuilder{})
}
