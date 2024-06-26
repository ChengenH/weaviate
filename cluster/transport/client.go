//                           _       _
// __      _____  __ ___   ___  __ _| |_ ___
// \ \ /\ / / _ \/ _` \ \ / / |/ _` | __/ _ \
//  \ V  V /  __/ (_| |\ V /| | (_| | ||  __/
//   \_/\_/ \___|\__,_| \_/ |_|\__,_|\__\___|
//
//  Copyright © 2016 - 2024 Weaviate B.V. All rights reserved.
//
//  CONTACT: hello@weaviate.io
//

package transport

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"

	cmd "github.com/weaviate/weaviate/cluster/proto/api"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const serviceConfig = `
{
	"methodConfig": [
		{
			"name": [
				{
					"service": "weaviate.internal.cluster.ClusterService", "method": "Apply"
				},
				{
					"service": "weaviate.internal.cluster.ClusterService", "method": "Query"
				}
			],
			"waitForReady": true,			
			"retryPolicy": {
				"MaxAttempts": 5,
				"BackoffMultiplier": 2,
				"InitialBackoff": "0.5s",
				"MaxBackoff": "5s",
				"RetryableStatusCodes": [
					"ABORTED",
					"RESOURCE_EXHAUSTED",
					"INTERNAL",
					"UNAVAILABLE",
					"NOT_FOUND"
				]
			}
		}
	]
}`

type rpcAddressResolver interface {
	// Address returns the RPC address corresponding to the given Raft address.
	Address(raftAddress string) (string, error)
}

// Client is used for communication with remote nodes in a RAFT cluster.
type Client struct {
	rpc rpcAddressResolver

	connLock   sync.Mutex
	leaderAddr string
	leaderConn *grpc.ClientConn
}

func NewClient(r rpcAddressResolver) *Client {
	return &Client{rpc: r}
}

// Join joins this node to an existing cluster identified by its leader's address.
// If a new leader has been elected, the request is redirected to the new leader.
func (cl *Client) Join(ctx context.Context, leaderAddress string, req *cmd.JoinPeerRequest) (*cmd.JoinPeerResponse, error) {
	conn, err := cl.getConn(leaderAddress)
	if err != nil {
		return nil, err
	}
	c := cmd.NewClusterServiceClient(conn)
	return c.JoinPeer(ctx, req)
}

// Notify informs a remote node rAddr of this node's readiness to join.
func (cl *Client) Notify(ctx context.Context, rAddr string, req *cmd.NotifyPeerRequest) (*cmd.NotifyPeerResponse, error) {
	addr, err := cl.rpc.Address(rAddr)
	if err != nil {
		return nil, fmt.Errorf("resolve address: %w", err)
	}

	conn, err := grpc.DialContext(ctx, addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	c := cmd.NewClusterServiceClient(conn)
	return c.NotifyPeer(ctx, req)
}

// Remove removes this node from an existing cluster identified by its leader's address.
// If a new leader has been elected, the request is redirected to the new leader.
func (cl *Client) Remove(ctx context.Context, leaderAddress string, req *cmd.RemovePeerRequest) (*cmd.RemovePeerResponse, error) {
	conn, err := cl.getConn(leaderAddress)
	if err != nil {
		return nil, err
	}
	c := cmd.NewClusterServiceClient(conn)
	return c.RemovePeer(ctx, req)
}

func (cl *Client) Apply(leaderAddr string, req *cmd.ApplyRequest) (*cmd.ApplyResponse, error) {
	ctx := context.Background()
	conn, err := cl.getConn(leaderAddr)
	if err != nil {
		return nil, err
	}

	c := cmd.NewClusterServiceClient(conn)
	return c.Apply(ctx, req)
}

func (cl *Client) Query(ctx context.Context, leaderAddress string, req *cmd.QueryRequest) (*cmd.QueryResponse, error) {
	conn, err := cl.getConn(leaderAddress)
	if err != nil {
		return nil, err
	}

	c := cmd.NewClusterServiceClient(conn)
	return c.Query(ctx, req)
}

func (cl *Client) Close() {
	if cl.leaderConn != nil {
		cl.leaderConn.Close()
	}
}

func (cl *Client) getConn(leaderAddress string) (*grpc.ClientConn, error) {
	cl.connLock.Lock()
	defer cl.connLock.Unlock()

	if cl.leaderConn != nil && leaderAddress == cl.leaderAddr {
		return cl.leaderConn, nil
	}

	if cl.leaderConn != nil {
		// close open conn if leader addr changed
		cl.leaderConn.Close()
	}

	addr, err := cl.rpc.Address(leaderAddress)
	if err != nil {
		return nil, fmt.Errorf("resolve address: %w", err)
	}

	cl.leaderConn, err = grpc.Dial(
		addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultServiceConfig(serviceConfig),
	)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}

	cl.leaderAddr = leaderAddress

	return cl.leaderConn, nil
}

func NewRPCResolver(isLocalHost bool, rpcPort int) rpcAddressResolver {
	return &rpcResolver{isLocalCluster: isLocalHost, rpcPort: rpcPort}
}

type rpcResolver struct {
	isLocalCluster bool
	rpcPort        int
}

// rpcAddressFromRAFT returns the RPC address based on the provided RAFT address.
// In a real cluster, the RPC port is the same for all nodes.
// In a local environment, the RAFT ports need to be different. Specifically,
// we calculate the RPC port as the RAFT port + 1.
func (cl *rpcResolver) Address(raftAddr string) (string, error) {
	host, port, err := net.SplitHostPort(raftAddr)
	if err != nil {
		return "", err
	}
	if !cl.isLocalCluster {
		return fmt.Sprintf("%s:%d", host, cl.rpcPort), nil
	}
	iPort, err := strconv.Atoi(port)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("%s:%d", host, iPort+1), nil
}
