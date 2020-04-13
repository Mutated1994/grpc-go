/*
 *
 * Copyright 2015 gRPC authors.
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

// Package main implements a client for Greeter service.
package main

import (
	"context"
	"log"
	"os"
	"time"

	"google.golang.org/grpc"
	pb "google.golang.org/grpc/examples/helloworld/helloworld"
	"google.golang.org/grpc/keepalive"
)

const (
	address     = "172.16.34.140:50051"
	defaultName = "world"
)

func main() {
	// Set up a connection to the server.
	conn, err := grpc.Dial(address, grpc.WithInsecure(), grpc.WithBlock(), grpc.WithKeepaliveParams(keepalive.ClientParameters{}))
	if err != nil {
		log.Fatalf("did not connect: %v", err)
	}
	defer conn.Close()

	c := pb.NewGreeterClient(conn)
	//Contact the server and print out its response.
	name := defaultName
	if len(os.Args) > 1 {
		name = os.Args[1]
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*1000)
	defer cancel()

	//var conns []*grpc.ClientConn
	//for i := 0; i < 1000; i++ {
	//	conn, err := grpc.Dial(address, grpc.WithInsecure(), grpc.WithBlock())
	//	if err != nil {
	//		log.Fatalf("did not connect: %v", err)
	//	}
	//	conns = append(conns, conn)
	//}
	//
	//t := time.Now()
	//wg := sync.WaitGroup{}
	//for i := 0; i < 100000; i++ {
	//	wg.Add(1)
	//	c := pb.NewGreeterClient(conns[i%1000])
	//	go func() {
	//		_, err := c.SayHello(ctx, &pb.HelloRequest{Name: name})
	//		if err != nil {
	//			log.Fatalf("could not greet: %v", err)
	//		}
	//		//log.Printf("Greeting: %s", r.GetMessage())
	//		wg.Done()
	//	}()
	//}
	//wg.Wait()
	r, err := c.SayHello(ctx, &pb.HelloRequest{Name: name})
	if err != nil {
		log.Fatalf("could not greet: %v", err)
	}
	log.Printf("Greeting: %s", r.GetMessage())

	//wg := sync.WaitGroup{}
	//for i := 0; i < 1000; i++ {
	//	wg.Add(1)
	//	go func() {
	//		_, err := c.SayHello(ctx, &pb.HelloRequest{Name: name})
	//		if err != nil {
	//			log.Fatalf("could not greet: %v", err)
	//		}
	//		//log.Printf("Greeting: %s", r.GetMessage())
	//		wg.Done()
	//	}()
	//}
	//wg.Wait()
	//
	//t := time.Now()
	//
	//for i := 0; i < 400000; i++ {
	//	wg.Add(1)
	//	go func() {
	//		_, err := c.SayHello(ctx, &pb.HelloRequest{Name: name})
	//		if err != nil {
	//			log.Fatalf("could not greet: %v", err)
	//		}
	//		//log.Printf("Greeting: %s", r.GetMessage())
	//		wg.Done()
	//	}()
	//}
	//wg.Wait()

	//fmt.Println(time.Since(t))
}
